package main

import (
	"log/slog"
	"sync/atomic"

	"bedrock-cross-version-relay/internal/blocktranslate"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// c2hSkipLogged keeps the once-per-process explanation out of the hot path.
var c2hSkipLogged atomic.Bool

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

// adapterMapsBlocks reports whether the 1001 adapter already maps blocks for this packet.
// It does NOT map chunk payloads (header only), so we still have to translate those.
func adapterMapsBlocks(clientProto int32, pk packet.Packet) bool {
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
