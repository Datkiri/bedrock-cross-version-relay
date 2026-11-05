package main

import (
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"

	"bedrock-cross-version-relay/internal/blocktranslate"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// c2hSkipLogged keeps the once-per-process explanation out of the hot path.
var c2hSkipLogged atomic.Bool

// hashScrubLogged keeps the once-per-process explanation out of the hot path.
var hashScrubLogged atomic.Bool

// hashScrubMismatchLogged bounds the warning for an output field that had no
// scrubbed counterpart (i.e. the pinned adapter invented a block ID field).
var hashScrubMismatchLogged atomic.Bool

// resolveProto deliberately uses wire-only New() adapters. If those adapters
// gain registry-aware conversion, keep block IDs mapped in exactly one layer.
func configureBlockTranslation(s *session, clientProtocol int32, gd minecraft.GameData) error {
	if s.hostProto == clientProtocol {
		return nil
	}
	m, err := blocktranslate.New(s.hostProto, clientProtocol, gd.UseBlockNetworkIDHashes, gd.CustomBlocks)
	if err != nil {
		return err
	}
	s.blocks = m
	if adapter1001HasRegistry && s.hostProto == 1001 {
		// The 1001 adapter already maps blocks for the C→H direction
		// (PlayerAuthInput/InventoryTransaction). Building a reverse mapper here would double-map.
		slog.Info("xvrelay: block palette translation enabled (one-way)",
			"hostProtocol", s.hostProto, "clientProtocol", clientProtocol,
			"hashIDs", gd.UseBlockNetworkIDHashes, "sourceStates", m.SourceCount(),
			"placeholderStates", m.FallbackCount(), "skippedCustom", m.SkippedCustom(),
			"placeholder", "minecraft:info_update", "reverse", "adapter1001")
		return nil
	}
	reverse, err := blocktranslate.New(clientProtocol, s.hostProto, gd.UseBlockNetworkIDHashes, gd.CustomBlocks)
	if err != nil {
		return err
	}
	s.reverseBlocks = reverse
	slog.Info("xvrelay: block palette translation enabled", "hostProtocol", s.hostProto, "clientProtocol", clientProtocol,
		"hashIDs", gd.UseBlockNetworkIDHashes, "sourceStates", m.SourceCount(), "placeholderStates", m.FallbackCount(), "placeholder", "minecraft:info_update")
	return nil
}

// adapterMapsBlocks reports whether the client codec already maps blocks for
// this packet, so the relay must NOT run blocktranslate over it a second time.
//
// With UseBlockNetworkIDHashes=true the answer is YES for every non-chunk
// packet, regardless of protocol: a StateHash is protocol-independent and
// blocktranslate would only replace an unknown (pack) hash with the
// info_update placeholder. Chunk payloads still need STRUCTURAL translation
// (v8 -> v9 upgrade, request-mode normalisation), so they stay on the
// blocktranslate path.
func adapterMapsBlocks(clientProto int32, pk packet.Packet, hashed bool) bool {
	if hashed {
		switch pk.(type) {
		case *packet.LevelChunk, *packet.SubChunk:
			return false
		}
		return true
	}
	if !adapter1001HasRegistry || clientProto != 1001 {
		return false
	}
	switch pk.(type) {
	case *packet.LevelChunk, *packet.SubChunk:
		return false
	}
	return true
}

func translateSessionPacket(s *session, pk packet.Packet) (packet.Packet, error) {
	if s == nil || s.blocks == nil {
		return pk, nil
	}
	return s.blocks.Translate(pk)
}

// scrubInteractionBlocks hides hashed block IDs from the pinned adapters.
//
// ★ THE ROOT CAUSE OF "place block / click block / spawn egg does nothing".
// With UseBlockNetworkIDHashes=true a client's UseItemTransactionData carries
// a 32-bit StateHash in BlockRuntimeID (and possibly in ItemStack.BlockRuntimeID).
// The registry-aware adapters assume NUMERIC runtime IDs and call
// blocks.TargetToNative(hash): that lookup fails, mapInventoryTransactionData
// returns (nil,false), convertGameplayToLatest returns an EMPTY slice, and
// gophertunnel silently DROPS the packet at read time. Measured offline:
// proto1001().ConvertToLatest(ClickBlock with stone's hash 0x80310e21) = [].
//
// Hashed block IDs are protocol-independent (StateHash of name+properties),
// i.e. they are ALREADY valid native 2193 IDs — no translation is needed. We
// zero them (and force ClickAir so the adapter skips the block branch), let the
// adapter translate the ITEM ids, then restore the original values verbatim on
// the CONVERTED packet (the adapter returns a clone, so pointers cannot be kept).
func scrubInteractionBlocks(pk packet.Packet) (restore func(packet.Packet), ok bool) {
	var tx *protocol.UseItemTransactionData
	var writeBack func()
	var topActions []protocol.InventoryAction

	switch p := pk.(type) {
	case *packet.InventoryTransaction:
		topActions = p.Actions
		if t, isUseItem := p.TransactionData.(*protocol.UseItemTransactionData); isUseItem {
			tx = t
		}
	case *packet.PlayerAuthInput:
		if v, set := p.ItemInteractionData.Value(); set {
			tx = &v
			writeBack = func() { p.ItemInteractionData = protocol.Option(*tx) }
		}
	default:
		return nil, false
	}
	if tx == nil && len(topActions) == 0 {
		return nil, false
	}

	var savedBlock, savedAction uint32
	var stackIDs, topIDs []int32
	scrubStack := func(s *protocol.ItemStack) {
		stackIDs = append(stackIDs, s.BlockRuntimeID)
		s.BlockRuntimeID = 0
	}
	if tx != nil {
		savedBlock, savedAction = tx.BlockRuntimeID, tx.ActionType
		scrubStack(&tx.HeldItem.Stack)
		for i := range tx.Actions {
			scrubStack(&tx.Actions[i].OldItem.Stack)
			scrubStack(&tx.Actions[i].NewItem.Stack)
		}
		tx.BlockRuntimeID = 0
		tx.ActionType = protocol.UseItemActionClickAir
	}
	for i := range topActions {
		topIDs = append(topIDs, topActions[i].OldItem.Stack.BlockRuntimeID, topActions[i].NewItem.Stack.BlockRuntimeID)
		topActions[i].OldItem.Stack.BlockRuntimeID = 0
		topActions[i].NewItem.Stack.BlockRuntimeID = 0
	}
	if writeBack != nil {
		writeBack()
	}

	return func(out packet.Packet) {
		var otx *protocol.UseItemTransactionData
		var oWriteBack func()
		var oTop []protocol.InventoryAction
		switch q := out.(type) {
		case *packet.InventoryTransaction:
			oTop = q.Actions
			if t, isUseItem := q.TransactionData.(*protocol.UseItemTransactionData); isUseItem {
				otx = t
			}
		case *packet.PlayerAuthInput:
			if v, set := q.ItemInteractionData.Value(); set {
				otx = &v
				oWriteBack = func() { q.ItemInteractionData = protocol.Option(*otx) }
			}
		default:
			return
		}
		idx := 0
		restoreStack := func(s *protocol.ItemStack) {
			if idx < len(stackIDs) {
				s.BlockRuntimeID = stackIDs[idx]
				idx++
			}
		}
		if otx != nil {
			otx.BlockRuntimeID, otx.ActionType = savedBlock, savedAction
			restoreStack(&otx.HeldItem.Stack)
			for i := range otx.Actions {
				restoreStack(&otx.Actions[i].OldItem.Stack)
				restoreStack(&otx.Actions[i].NewItem.Stack)
			}
		}
		for i := range oTop {
			if 2*i+1 < len(topIDs) {
				oTop[i].OldItem.Stack.BlockRuntimeID = topIDs[2*i]
				oTop[i].NewItem.Stack.BlockRuntimeID = topIDs[2*i+1]
			}
		}
		if oWriteBack != nil {
			oWriteBack()
		}
	}, true
}

// translateC2HInteractions maps client->host BlockRuntimeIDs for interaction
// packets (InventoryTransaction / PlayerAuthInput.ItemInteractionData).
//
// ★ DO NOT RUN IT FOR A REGISTRY-AWARE CLIENT CODEC.
// NewWithRegistries(1001/2168/2169) already maps BOTH item runtime IDs and
// UseItemTransactionData.BlockRuntimeID inside ConvertToLatest, which runs when
// the relay READS from the client conn. By the time pk reaches this relay loop
// it therefore holds NATIVE 2193 IDs. blocktranslate.New(clientProto, hostProto)
// expects CLIENT IDs as input, so feeding it a native ID either misses the map
// (=> info_update placeholder) or maps a different state. That is exactly the
// "place block / click block / spawn egg does nothing" symptom measured live
// 21:02-21:08: the GUI client sent 51 InventoryTransaction + 7112
// PlayerAuthInput, and every clicked block ID was silently replaced.
//
// The reverse mapper is still needed for the wire-only codecs (New(), i.e.
// -map-gameplay-items=false) where ConvertToLatest is a pass-through.
func translateC2HInteractions(s *session, pk packet.Packet, adapterAlreadyMaps bool) (packet.Packet, error) {
	if s == nil || s.reverseBlocks == nil {
		return pk, nil
	}
	if adapterAlreadyMaps {
		if c2hSkipLogged.CompareAndSwap(false, true) {
			slog.Info("xvrelay: C->H interaction IDs already translated by the client codec; reverse mapper skipped",
				"reason", "NewWithRegistries maps InventoryTransaction/PlayerAuthInput in ConvertToLatest")
		}
		return pk, nil
	}
	return s.reverseBlocks.TranslateInteractions(pk)
}

// ── Hashed block-ID guard (H→C) ─────────────────────────────────────────────────

// levelEventUsesBlockID reports whether a LevelEvent's EventData PACKS a block
// runtime ID. The pinned adapter maps those through mapBlockRuntimeID. The
// item-based legacy event (|14) deliberately stays out: its EventData must keep
// the ITEM id translation that scrubHashedBlockIDs must not undo.
func levelEventUsesBlockID(eventType int32) bool {
	switch eventType {
	case packet.LevelEventParticlesDestroyBlock,
		packet.LevelEventParticlesCrackBlock,
		packet.LevelEventParticleLegacyEvent | 21:
		return true
	}
	return false
}

// blockIDRef is one hashed block ID field together with a STRUCTURAL key that
// survives the pinned adapter dropping list elements. A flat positional list
// is NOT enough: targetCreativeContent drops 143 of 2076 native creative items
// on 1001 (100 on 2168/2169) and mapCraftingData filters recipes. Measured
// 2026-11-05: a positional restore corrupted 1812/1933 surviving creative
// block IDs from item 121 onward.
type blockIDRef struct {
	key   string
	value uint32
}

func blockIDFieldValue(f reflect.Value) (uint32, bool) {
	switch f.Kind() {
	case reflect.Int32:
		return uint32(f.Int()), true
	case reflect.Uint32:
		return uint32(f.Uint()), true
	}
	return 0, false
}

func setBlockIDField(f reflect.Value, v uint32) {
	switch f.Kind() {
	case reflect.Int32:
		f.SetInt(int64(int32(v)))
	case reflect.Uint32:
		f.SetUint(uint64(v))
	}
}

// stableElemKey returns a path segment for a slice element. The pinned adapters
// drop CreativeContent items and CraftingData recipes, so a plain index would
// shift every later restore. Those structs carry a PRESERVED network ID
// (CreativeItemNetworkID / RecipeNetworkID), which is exactly the stable
// identity we need. Everything else (inventory slots, block change entries,
// recipe outputs, group icons) keeps a fixed length, so an index is safe there.
func stableElemKey(v reflect.Value, index int) string {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			break
		}
		v = v.Elem()
	}
	if v.IsValid() && v.Kind() == reflect.Struct {
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			sf := t.Field(i)
			if sf.PkgPath != "" || (sf.Name != "CreativeItemNetworkID" && sf.Name != "RecipeNetworkID") {
				continue
			}
			if n, ok := blockIDFieldValue(v.Field(i)); ok {
				return sf.Name + "=" + strconv.FormatUint(uint64(n), 10)
			}
		}
	}
	return strconv.Itoa(index)
}

// walkBlockIDFields walks an H→C packet and calls fn(key, field) for every
// settable int32/uint32 field whose name contains "BlockRuntimeID". That naming
// covers UpdateBlock.NewBlockRuntimeID, BlockChangeEntry.BlockRuntimeID and the
// ItemStack.BlockRuntimeID nested inside every item-carrying packet family.
//
// Bytes/strings/maps are skipped: LevelChunk.RawPayload is a multi-kilobyte
// []byte and must not be walked byte-by-byte. Unexported fields (e.g.
// Optional.val) are skipped for safety; no H→C packet wraps a block ID in one.
func walkBlockIDFields(v reflect.Value, fn func(key string, f reflect.Value)) {
	visitBlockIDFields(v, "", 0, fn)
}

func visitBlockIDFields(v reflect.Value, path string, depth int, fn func(string, reflect.Value)) {
	if depth > 16 || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			visitBlockIDFields(v.Elem(), path, depth+1, fn)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			sf := t.Field(i)
			if sf.PkgPath != "" { // unexported field
				continue
			}
			f := v.Field(i)
			next := path + "." + sf.Name
			if strings.Contains(sf.Name, "BlockRuntimeID") && f.CanSet() {
				if _, ok := blockIDFieldValue(f); ok {
					fn(next, f)
					continue
				}
			}
			visitBlockIDFields(f, next, depth+1, fn)
		}
	case reflect.Slice, reflect.Array:
		if v.Len() > 4096 {
			return
		}
		switch v.Type().Elem().Kind() {
		case reflect.Uint8, reflect.Int8, reflect.String:
			return
		}
		for i := 0; i < v.Len(); i++ {
			elem := v.Index(i)
			visitBlockIDFields(elem, path+"["+stableElemKey(elem, i)+"]", depth+1, fn)
		}
	}
}

// scrubHashedBlockIDs hides protocol-independent StateHash block IDs from the
// pinned registry-aware adapters and returns a restore func that puts them back
// verbatim on each CONVERTED packet.
//
// ★ ROOT CAUSE OF "block placed / broken renders the wrong block". Measured
// offline (2026-11-05, 2193 host -> 1001/2168/2169 client, hashIDs=true):
//
//	stone StateHash 0x80310e21
//	  1001 UpdateBlock  -> 0x3326      (numeric 1001 runtime ID)
//	  2168/2169         -> 0x353d
//
// NewWithRegistries is built for NUMERIC block runtime IDs. With
// UseBlockNetworkIDHashes=true the wire carries StateHash values that are
// already valid native IDs, so NativeToTarget(hash) misses and substitutes an
// unrelated numeric ID. The GUI client then renders a different block for
// every dynamic update (104 UpdateBlock in the 6-minute live session).
//
// The same mapper also rewrites ItemStack.BlockRuntimeID, LevelEvent.EventData
// and LevelSoundEvent.ExtraData, so the guard is packet-generic rather than a
// hand-written switch over the three UpdateBlock families. Zeroing the IDs
// before conversion guarantees the adapter never drops the packet (mapLevelEventData
// and MapLevelSoundBlockRuntimeID return false on an unmapped ID).
//
// ★ Restore is KEYED, not positional. targetCreativeContent drops native items
// that have no target counterpart (2076 -> 1933 on 1001) and mapCraftingData
// filters recipes; a flat list would assign the dropped element's hash to the
// next survivor. Measured 2026-11-05: positional restore corrupted 1812/1933
// creative block IDs from item 121 on. Each field is keyed by a structural path
// where CreativeItemNetworkID / RecipeNetworkID replace the slice index.
func scrubHashedBlockIDs(pk packet.Packet, hashed bool) (restore func(packet.Packet), ok bool) {
	if !hashed || pk == nil {
		return nil, false
	}
	var refs []blockIDRef
	var packedEvent, packedSound int32
	var hasEvent, hasSound bool
	switch p := pk.(type) {
	case *packet.LevelEvent:
		if levelEventUsesBlockID(p.EventType) {
			packedEvent, hasEvent = p.EventData, true
			p.EventData = 0
		}
	case *packet.LevelSoundEvent:
		// Only block-typed sounds carry a runtime ID in ExtraData; restoring the
		// saved value is a no-op for all other sound types.
		packedSound, hasSound = p.ExtraData, true
		p.ExtraData = 0
	}
	walkBlockIDFields(reflect.ValueOf(pk), func(key string, f reflect.Value) {
		v, _ := blockIDFieldValue(f)
		refs = append(refs, blockIDRef{key: key, value: v})
		setBlockIDField(f, 0)
	})
	if len(refs) == 0 && !hasEvent && !hasSound {
		return nil, false
	}
	if hashScrubLogged.CompareAndSwap(false, true) {
		slog.Info("xvrelay: UseBlockNetworkIDHashes=true — hiding StateHash block IDs from the client codec",
			"reason", "pinned registry-aware adapters map numeric runtime IDs and corrupt protocol-independent hashes",
			"restore", "stable structural keys, not positional",
			"type", reflect.TypeOf(pk).String(), "fields", len(refs),
			"packedLevelEvent", hasEvent, "packedLevelSound", hasSound)
	}
	return func(out packet.Packet) {
		// Fresh queue on every call: the same refs restore the INPUT packet
		// (verifyRoundTrip discards the output) AND every converted output.
		queue := make(map[string][]uint32, len(refs))
		for _, r := range refs {
			queue[r.key] = append(queue[r.key], r.value)
		}
		cursor := make(map[string]int, len(queue))
		var unmatched int
		walkBlockIDFields(reflect.ValueOf(out), func(key string, f reflect.Value) {
			vals := queue[key]
			i := cursor[key]
			if i >= len(vals) {
				unmatched++
				return
			}
			setBlockIDField(f, vals[i])
			cursor[key] = i + 1
		})
		switch q := out.(type) {
		case *packet.LevelEvent:
			if hasEvent {
				q.EventData = packedEvent
			}
		case *packet.LevelSoundEvent:
			if hasSound {
				q.ExtraData = packedSound
			}
		}
		if unmatched > 0 && hashScrubMismatchLogged.CompareAndSwap(false, true) {
			slog.Warn("xvrelay: hashed block restore found an unmatched field",
				"type", reflect.TypeOf(out).String(), "count", unmatched,
				"reason", "the client codec produced a block-ID field the guard did not scrub")
		}
	}, true
}
