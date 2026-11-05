package main

import (
	"bytes"
	"fmt"
	"testing"

	"bedrock-cross-version-relay/internal/blocktranslate"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestSessionBlockTranslation(t *testing.T) {
	s := &session{hostProto: 2193}
	if err := configureBlockTranslation(s, 1001, minecraft.GameData{}); err != nil {
		t.Fatal(err)
	}
	source, _ := blocktranslate.New(2193, 2193, false, nil)
	target, _ := blocktranslate.New(1001, 1001, false, nil)
	in, ok := source.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone missing")
	}
	want, _ := target.SourceID("minecraft:stone", nil)
	pk := &packet.UpdateBlock{NewBlockRuntimeID: in}
	got, err := translateSessionPacket(s, pk)
	if err != nil {
		t.Fatal(err)
	}
	if got.(*packet.UpdateBlock).NewBlockRuntimeID != want {
		t.Fatal("session mapper not applied")
	}
	plain := &session{hostProto: 2193}
	if err := configureBlockTranslation(plain, 2193, minecraft.GameData{}); err != nil {
		t.Fatal(err)
	}
	if out, err := translateSessionPacket(plain, pk); err != nil || out != pk {
		t.Fatal("same-protocol packet changed")
	}
}

// TestC2HInteractionsSkipDoubleMap pins the live 21:02-21:08 defect: a
// registry-aware client codec (NewWithRegistries) already maps
// UseItemTransactionData.BlockRuntimeID during ConvertToLatest, so pk arriving
// in the C->H relay loop holds a NATIVE 2193 ID. The reverse mapper is built
// for CLIENT IDs; feeding it a native ID silently replaces the clicked/placed
// block with the info_update placeholder. translateC2HInteractions must skip it.
func TestC2HInteractionsSkipDoubleMap(t *testing.T) {
	s := &session{hostProto: 2193}
	if err := configureBlockTranslation(s, 1001, minecraft.GameData{UseBlockNetworkIDHashes: true}); err != nil {
		t.Fatal(err)
	}
	if s.reverseBlocks == nil {
		t.Fatal("reverse mapper was not built")
	}

	const nativeID = uint32(0x7fff_ff00) // a NATIVE 2193 ID, not a 1001 ID
	tx := &protocol.UseItemTransactionData{
		ActionType:     protocol.UseItemActionClickBlock,
		BlockRuntimeID: nativeID,
	}
	pk := &packet.InventoryTransaction{TransactionData: tx}

	// Sanity: without the gate the reverse mapper really does corrupt a native ID.
	corrupted, err := s.reverseBlocks.TranslateInteractions(pk)
	if err != nil {
		t.Fatal(err)
	}
	got := corrupted.(*packet.InventoryTransaction).TransactionData.(*protocol.UseItemTransactionData).BlockRuntimeID
	if got == nativeID {
		t.Fatalf("reverse mapper kept native ID %#x; the double-map premise is wrong", nativeID)
	}

	out, err := translateC2HInteractions(s, pk, true)
	if err != nil {
		t.Fatal(err)
	}
	if out != pk {
		t.Fatal("registry-aware codec: packet must be passed through untouched")
	}
	if got := out.(*packet.InventoryTransaction).TransactionData.(*protocol.UseItemTransactionData).BlockRuntimeID; got != nativeID {
		t.Fatalf("double map: BlockRuntimeID %#x -> %#x", nativeID, got)
	}
}

// TestC2HInteractionsUsesReverseMapper keeps the wire-only codec path alive:
// with New() (no registries) ConvertToLatest is a pass-through, so the manual
// reverse mapper MUST still translate client IDs.
func TestC2HInteractionsUsesReverseMapper(t *testing.T) {
	s := &session{hostProto: 2193}
	if err := configureBlockTranslation(s, 1001, minecraft.GameData{UseBlockNetworkIDHashes: true}); err != nil {
		t.Fatal(err)
	}
	clientRegistry, err := blocktranslate.New(1001, 1001, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientID, ok := clientRegistry.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone missing from the 1001 palette")
	}
	pk := &packet.InventoryTransaction{TransactionData: &protocol.UseItemTransactionData{
		ActionType:     protocol.UseItemActionClickBlock,
		BlockRuntimeID: clientID,
	}}
	out, err := translateC2HInteractions(s, pk, false)
	if err != nil {
		t.Fatal(err)
	}
	// Hashed block IDs are protocol-independent (StateHash of name+properties),
	// so a vanilla block may map to the SAME numeric value. What matters is that
	// TranslateInteractions ran and produced a cloned packet, unlike the skipped
	// registry-aware path above.
	if out == pk {
		t.Fatal("wire-only codec: packet must be cloned/translated")
	}
	got := out.(*packet.InventoryTransaction).TransactionData.(*protocol.UseItemTransactionData).BlockRuntimeID
	if got != clientID && got == 0 {
		t.Fatal("wire-only codec: block ID was zeroed")
	}
}

// TestRegistryAwareCodecKeepsClickBlockTransaction probes the C2H read path:
// mapInventoryTransactionData calls blocks.TargetToNative on the clicked block.
// With UseBlockNetworkIDHashes=true the client sends a 32-bit StateHash, so a
// naive numeric lookup would fail and ConvertToLatest would return an EMPTY
// slice — gophertunnel then silently drops the packet, which is exactly what
// "place block / click block does nothing" looks like in-game.
func TestRegistryAwareCodecKeepsClickBlockTransaction(t *testing.T) {
	reg, err := blocktranslate.New(1001, 1001, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := reg.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone missing from the 1001 palette")
	}
	pk := &packet.InventoryTransaction{TransactionData: &protocol.UseItemTransactionData{
		ActionType:     protocol.UseItemActionClickBlock,
		BlockRuntimeID: id,
	}}
	// Production path: the client conn's protocol is clientVersionProtocol
	// wrapping the adapter. It must neither drop the packet nor corrupt the
	// hashed block ID.
	out := (clientVersionProtocol{Protocol: proto1001()}).ConvertToLatest(pk, (*minecraft.Conn)(nil))
	if len(out) == 0 {
		t.Fatalf("registry-aware 1001 codec DROPPED a ClickBlock InventoryTransaction (hashed id %#x)", id)
	}
	got := out[0].(*packet.InventoryTransaction).TransactionData.(*protocol.UseItemTransactionData)
	if got.BlockRuntimeID != id {
		t.Fatalf("hashed BlockRuntimeID %#x -> %#x (must be preserved verbatim)", id, got.BlockRuntimeID)
	}
	if got.ActionType != protocol.UseItemActionClickBlock {
		t.Fatalf("ActionType %d was not restored to ClickBlock", got.ActionType)
	}
}

// fakeTranslatedPacket mimics the unexported *translatedPacket wrapper the
// 1001/2168/2169 wire pools return from ReadPacket. It has the same `inner`
// field so unwrapWirePacket's reflect+unsafe path can be exercised offline.
type fakeTranslatedPacket struct{ inner packet.Packet }

func (p *fakeTranslatedPacket) ID() uint32          { return p.inner.ID() }
func (p *fakeTranslatedPacket) Marshal(protocol.IO) {}

func TestUnwrapWirePacketFindsInner(t *testing.T) {
	inner := &packet.InventoryTransaction{}
	wrapped := &fakeTranslatedPacket{inner: inner}
	if got := unwrapWirePacket(wrapped); got != inner {
		t.Fatalf("unwrapWirePacket returned %T, want the inner packet", got)
	}
	if got := unwrapWirePacket(inner); got != inner {
		t.Fatal("a plain packet must unwrap to itself")
	}
}

func TestScrubRestoresPlayerAuthInputInteraction(t *testing.T) {
	reg, err := blocktranslate.New(1001, 1001, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := reg.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone missing from the 1001 palette")
	}
	pk := &packet.PlayerAuthInput{
		ItemInteractionData: protocol.Option(protocol.UseItemTransactionData{
			ActionType:     protocol.UseItemActionClickBlock,
			BlockRuntimeID: id,
		}),
	}
	out := (clientVersionProtocol{Protocol: proto1001()}).ConvertToLatest(pk, (*minecraft.Conn)(nil))
	if len(out) == 0 {
		t.Fatal("PlayerAuthInput with ItemInteractionData was dropped")
	}
	tx, set := out[0].(*packet.PlayerAuthInput).ItemInteractionData.Value()
	if !set {
		t.Fatal("ItemInteractionData disappeared")
	}
	if tx.BlockRuntimeID != id || tx.ActionType != protocol.UseItemActionClickBlock {
		t.Fatalf("interaction data not restored: id=%#x action=%d", tx.BlockRuntimeID, tx.ActionType)
	}
}

// TestClientMovementPredictionSyncGrownToNative pins the second live defect:
// a 1001 client sends 130 actor flags, the native 2193 host writer demands
// protocol.EntityDataFlagCount (=131), and writing used to panic with
// "bitset size mismatch: expected 131, got 130" (13 dropped packets / 6 min).
func TestClientMovementPredictionSyncGrownToNative(t *testing.T) {
	if protocol.EntityDataFlagCount <= 130 {
		t.Fatalf("native EntityDataFlagCount=%d, expected >130", protocol.EntityDataFlagCount)
	}
	legacy := protocol.NewBitset(130)
	legacy.Set(3)
	legacy.Set(129)

	p := clientVersionProtocol{Protocol: proto1001()}
	out := p.ConvertToLatest(&packet.ClientMovementPredictionSync{ActorFlags: legacy}, (*minecraft.Conn)(nil))
	if len(out) != 1 {
		t.Fatalf("ConvertToLatest returned %d packets, want 1", len(out))
	}
	sync, ok := out[0].(*packet.ClientMovementPredictionSync)
	if !ok {
		t.Fatalf("ConvertToLatest returned %T", out[0])
	}
	if sync.ActorFlags.Len() != protocol.EntityDataFlagCount {
		t.Fatalf("ActorFlags.Len()=%d, want %d", sync.ActorFlags.Len(), protocol.EntityDataFlagCount)
	}
	if !sync.ActorFlags.Load(3) || !sync.ActorFlags.Load(129) {
		t.Fatal("resizing lost an existing actor flag")
	}

	// The whole point: encoding the grown bitset with the NATIVE 2193 layout
	// must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("native encode panicked: %v", r)
		}
	}()
	var buf bytes.Buffer
	w := protocol.NewWriter(&buf, 0)
	sync.Marshal(w)
	if buf.Len() == 0 {
		t.Fatal("native encode produced no bytes")
	}
}

// TestHashedBlockIDsSurviveClientCodec pins the live GUI symptom "block
// placed/broken renders wrong" (log 21:00-21:08, 104 UpdateBlock in 6 min).
// With UseBlockNetworkIDHashes=true the wire carries protocol-independent
// StateHash values, but NewWithRegistries maps NUMERIC runtime IDs:
//
//	stone hash 0x80310e21 -> 0x3326 (1001) / 0x353d (2168, 2169)
//
// The corruption hits every H→C surface the adapter maps: UpdateBlock,
// UpdateBlockSynced, UpdateSubChunkBlocks, packed LevelEvent/LevelSoundEvent
// data, and ItemStack.BlockRuntimeID inside item-carrying packets.
func TestHashedBlockIDsSurviveClientCodec(t *testing.T) {
	m, err := blocktranslate.New(2193, 1001, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash, ok := m.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone hash missing")
	}
	// The direct chunk translation path must be identity for a hash.
	direct := &packet.UpdateBlock{NewBlockRuntimeID: hash}
	gotDirect, err := m.Translate(direct)
	if err != nil {
		t.Fatal(err)
	}
	if id := gotDirect.(*packet.UpdateBlock).NewBlockRuntimeID; id != hash {
		t.Fatalf("blocktranslate changed a protocol-independent hash: %#x -> %#x", hash, id)
	}

	stack := func() protocol.ItemInstance {
		return protocol.ItemInstance{Stack: protocol.ItemStack{
			ItemType:       protocol.ItemType{NetworkID: 1},
			BlockRuntimeID: int32(hash),
		}}
	}
	cases := []struct {
		name string
		make func() packet.Packet
		get  func(packet.Packet) (uint32, bool)
	}{
		{"UpdateBlock", func() packet.Packet {
			return &packet.UpdateBlock{NewBlockRuntimeID: hash}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.UpdateBlock)
			return q.NewBlockRuntimeID, ok
		}},
		{"UpdateBlockSynced", func() packet.Packet {
			return &packet.UpdateBlockSynced{NewBlockRuntimeID: hash}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.UpdateBlockSynced)
			return q.NewBlockRuntimeID, ok
		}},
		{"UpdateSubChunkBlocks/Blocks", func() packet.Packet {
			return &packet.UpdateSubChunkBlocks{Blocks: []protocol.BlockChangeEntry{{BlockRuntimeID: hash}}}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.UpdateSubChunkBlocks)
			if !ok || len(q.Blocks) == 0 {
				return 0, false
			}
			return q.Blocks[0].BlockRuntimeID, true
		}},
		{"UpdateSubChunkBlocks/Extra", func() packet.Packet {
			return &packet.UpdateSubChunkBlocks{Extra: []protocol.BlockChangeEntry{{BlockRuntimeID: hash}}}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.UpdateSubChunkBlocks)
			if !ok || len(q.Extra) == 0 {
				return 0, false
			}
			return q.Extra[0].BlockRuntimeID, true
		}},
		{"LevelEvent/DestroyBlock", func() packet.Packet {
			return &packet.LevelEvent{EventType: packet.LevelEventParticlesDestroyBlock, EventData: int32(hash)}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.LevelEvent)
			return uint32(q.EventData), ok
		}},
		{"LevelEvent/CrackBlock", func() packet.Packet {
			return &packet.LevelEvent{EventType: packet.LevelEventParticlesCrackBlock, EventData: int32(hash)}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.LevelEvent)
			return uint32(q.EventData), ok
		}},
		{"LevelEvent/LegacyBlock", func() packet.Packet {
			return &packet.LevelEvent{EventType: packet.LevelEventParticleLegacyEvent | 21, EventData: int32(hash)}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.LevelEvent)
			return uint32(q.EventData), ok
		}},
		{"LevelSoundEvent/Place", func() packet.Packet {
			return &packet.LevelSoundEvent{SoundType: packet.SoundEventPlace, ExtraData: int32(hash)}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.LevelSoundEvent)
			return uint32(q.ExtraData), ok
		}},
		{"InventoryContent/stack", func() packet.Packet {
			return &packet.InventoryContent{Content: []protocol.ItemInstance{stack()}}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.InventoryContent)
			if !ok || len(q.Content) == 0 {
				return 0, false
			}
			return uint32(q.Content[0].Stack.BlockRuntimeID), true
		}},
		{"MobEquipment/stack", func() packet.Packet {
			return &packet.MobEquipment{NewItem: stack()}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.MobEquipment)
			return uint32(q.NewItem.Stack.BlockRuntimeID), ok
		}},
		{"CreativeContent/stack", func() packet.Packet {
			// targetCreativeContent rejects an item whose GroupIndex has no
			// matching group, so the fixture must carry one group.
			return &packet.CreativeContent{
				Groups: []protocol.CreativeGroup{{Category: 1, Name: "itemGroup.name.all"}},
				Items: []protocol.CreativeItem{
					{CreativeItemNetworkID: 1, GroupIndex: 0, Item: protocol.ItemStack{
						ItemType:       protocol.ItemType{NetworkID: 1},
						BlockRuntimeID: int32(hash),
					}},
				},
			}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.CreativeContent)
			if !ok || len(q.Items) == 0 {
				return 0, false
			}
			return uint32(q.Items[0].Item.BlockRuntimeID), true
		}},
		{"AddItemActor/stack", func() packet.Packet {
			return &packet.AddItemActor{Item: stack()}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.AddItemActor)
			return uint32(q.Item.Stack.BlockRuntimeID), ok
		}},
		{"InventoryTransaction/UseItem", func() packet.Packet {
			return &packet.InventoryTransaction{TransactionData: &protocol.UseItemTransactionData{
				ActionType:     protocol.UseItemActionClickBlock,
				BlockRuntimeID: hash,
				HeldItem:       stack(),
			}}
		}, func(p packet.Packet) (uint32, bool) {
			q, ok := p.(*packet.InventoryTransaction)
			if !ok {
				return 0, false
			}
			tx, ok := q.TransactionData.(*protocol.UseItemTransactionData)
			if !ok {
				return 0, false
			}
			return tx.BlockRuntimeID, true
		}},
	}

	protocols := []struct {
		id   int32
		make func() minecraft.Protocol
	}{
		{1001, proto1001},
		{2168, proto2168},
		{2169, proto2169},
	}
	for _, pr := range protocols {
		p := clientVersionProtocol{Protocol: pr.make()}
		for _, c := range cases {
			t.Run(fmt.Sprintf("%d/%s", pr.id, c.name), func(t *testing.T) {
				out := p.ConvertFromLatest(c.make(), (*minecraft.Conn)(nil))
				if len(out) == 0 {
					t.Fatalf("codec returned 0 packets: family silently dropped at write")
				}
				got, found := c.get(unwrapWirePacket(out[0]))
				if !found {
					t.Fatalf("converted packet has unexpected type %T", unwrapWirePacket(out[0]))
				}
				if got != hash {
					t.Fatalf("hashed block ID corrupted: %#x -> %#x (client renders the wrong block)", hash, got)
				}
			})
		}
	}
}

// TestHashedBlockScrubGate pins the non-hashed (numeric runtime ID) path: the
// guard must stay OFF there so the adapter can legitimately map 2193 numeric
// IDs into the client's numeric palette.
func TestHashedBlockScrubGate(t *testing.T) {
	pk := &packet.UpdateBlock{NewBlockRuntimeID: 1234}
	if restore, ok := scrubHashedBlockIDs(pk, false); ok || restore != nil {
		t.Fatal("scrub must be disabled for numeric block IDs")
	}
	if pk.NewBlockRuntimeID != 1234 {
		t.Fatalf("numeric ID was zeroed: %d", pk.NewBlockRuntimeID)
	}
	if _, ok := scrubHashedBlockIDs(&packet.MovePlayer{}, true); ok {
		t.Fatal("a packet with no block ID fields must not be scrubbed")
	}
}

// TestHashedBlockScrubRestoresInput guards the verify/dump path: verifyRoundTrip
// calls ConvertFromLatest(pk, nil) and DISCARDS the result. The deferred restore
// must also undo the in-place scrub on pk, or the real WritePacket sends zeros.
func TestHashedBlockScrubRestoresInput(t *testing.T) {
	m, _ := blocktranslate.New(2193, 1001, true, nil)
	hash, _ := m.SourceID("minecraft:stone", nil)
	pk := &packet.UpdateBlock{NewBlockRuntimeID: hash}
	p := clientVersionProtocol{Protocol: proto1001()}
	out := p.ConvertFromLatest(pk, (*minecraft.Conn)(nil))
	if len(out) != 1 {
		t.Fatalf("got %d packets", len(out))
	}
	if pk.NewBlockRuntimeID != hash {
		t.Fatalf("input packet left scrubbed: %#x (want %#x)", pk.NewBlockRuntimeID, hash)
	}
	if got := unwrapWirePacket(out[0]).(*packet.UpdateBlock).NewBlockRuntimeID; got != hash {
		t.Fatalf("output ID %#x, want %#x", got, hash)
	}
}

func TestAdapterMapsBlocksHashed(t *testing.T) {
	if !adapterMapsBlocks(2169, &packet.UpdateBlock{}, true) {
		t.Fatal("hashed mode: dynamic block packets must skip blocktranslate")
	}
	if !adapterMapsBlocks(1001, &packet.CreativeContent{}, true) {
		t.Fatal("hashed mode: item-stack block IDs must skip blocktranslate too")
	}
	if adapterMapsBlocks(1001, &packet.LevelChunk{}, true) || adapterMapsBlocks(1001, &packet.SubChunk{}, true) {
		t.Fatal("hashed mode: chunk payloads still need structural translation")
	}
	if adapterMapsBlocks(2169, &packet.UpdateBlock{}, false) {
		t.Fatal("numeric mode: non-1001 codecs need blocktranslate")
	}
}

func TestExplicitCustomHashSurvivesBothDirections(t *testing.T) {
	entry := protocol.BlockEntry{Name: "xvrelay:test", Properties: map[string]any{"direction": int32(2)}}
	s := &session{hostProto: 2193}
	if err := configureBlockTranslation(s, 1001, minecraft.GameData{UseBlockNetworkIDHashes: true, CustomBlocks: []protocol.BlockEntry{entry}}); err != nil {
		t.Fatal(err)
	}
	hash, err := blocktranslate.StateHash(entry.Name, entry.Properties)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.blocks.Map(hash); err != nil || got != hash {
		t.Fatalf("custom forward hash changed: %#x %v", got, err)
	}
	if got, err := s.reverseBlocks.Map(hash); err != nil || got != hash {
		t.Fatalf("custom reverse hash changed: %#x %v", got, err)
	}
}

// TestClientCodecDoesNotDropOtherC2HFamilies closes the "vân vân" gap from the
// live GUI log: an adapter that returns an EMPTY slice from ConvertToLatest
// drops the packet at read time, so it never appears in the relay tally. Two
// families were already fixed (UseItem transactions + prediction sync); this
// table pins every OTHER C2H family the 21:02-21:08 GUI session actually sent,
// including ones carrying a hashed BlockRuntimeID inside an ItemStack
// (MobEquipment = spawn egg / hotbar selection).
func TestClientCodecDoesNotDropOtherC2HFamilies(t *testing.T) {
	reg, err := blocktranslate.New(1001, 1001, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash, ok := reg.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone hash missing")
	}
	tx := protocol.UseItemTransactionData{
		ActionType:     protocol.UseItemActionClickBlock,
		BlockRuntimeID: hash,
		BlockPosition:  protocol.BlockPos{0, -60, 0},
		BlockFace:      1,
	}
	cases := []struct {
		name string
		make func() packet.Packet
	}{
		{"ClientMovementPredictionSync", func() packet.Packet {
			return &packet.ClientMovementPredictionSync{ActorFlags: protocol.NewBitset(130)}
		}},
		{"InventoryTransaction/UseItem", func() packet.Packet {
			return &packet.InventoryTransaction{TransactionData: &tx}
		}},
		{"PlayerAuthInput/ItemInteractionData", func() packet.Packet {
			return &packet.PlayerAuthInput{ItemInteractionData: protocol.Option(tx)}
		}},
		{"MobEquipment/stack-with-hash", func() packet.Packet {
			return &packet.MobEquipment{
				EntityRuntimeID: 1,
				NewItem: protocol.ItemInstance{
					Stack: protocol.ItemStack{BlockRuntimeID: int32(hash)},
				},
			}
		}},
		{"ItemStackRequest", func() packet.Packet {
			return &packet.ItemStackRequest{}
		}},
		{"Interact", func() packet.Packet {
			return &packet.Interact{ActionType: packet.InteractActionOpenInventory}
		}},
		{"PlayerAction", func() packet.Packet {
			return &packet.PlayerAction{EntityRuntimeID: 1, BlockFace: 1}
		}},
		{"Animate", func() packet.Packet {
			return &packet.Animate{ActionType: packet.AnimateActionSwingArm, EntityRuntimeID: 1}
		}},
		{"SubChunkRequest", func() packet.Packet {
			return &packet.SubChunkRequest{}
		}},
		{"SetPlayerInventoryOptions", func() packet.Packet {
			return &packet.SetPlayerInventoryOptions{}
		}},
		{"LevelEvent/legacy-item-particle", func() packet.Packet {
			return &packet.LevelEvent{EventType: packet.ParticleTypeIconCrack, EventData: int32(hash)}
		}},
	}
	// A packet can survive ConvertToLatest yet still carry a CORRUPTED ID: the
	// adapter may map a protocol-independent StateHash as if it were a numeric
	// runtime ID. MobEquipment.NewItem.Stack.BlockRuntimeID is exactly the field
	// a spawn-egg / hotbar selection uses, so pin its value, not just the count.
	hashInt := int32(hash)
	for _, pr := range []struct {
		id   int32
		make func() minecraft.Protocol
	}{
		{1001, proto1001},
		{2168, proto2168},
		{2169, proto2169},
	} {
		out := (clientVersionProtocol{Protocol: pr.make()}).ConvertToLatest(
			&packet.MobEquipment{
				EntityRuntimeID: 1,
				NewItem: protocol.ItemInstance{
					Stack: protocol.ItemStack{BlockRuntimeID: hashInt},
				},
			}, (*minecraft.Conn)(nil))
		got := out[0].(*packet.MobEquipment).NewItem.Stack.BlockRuntimeID
		if got != hashInt {
			t.Errorf("proto %d: MobEquipment BlockRuntimeID %#x -> %#x (hashed ID must survive verbatim)", pr.id, hashInt, got)
		}
	}

	protocols := []struct {
		id   int32
		make func() minecraft.Protocol
	}{
		{1001, proto1001},
		{2168, proto2168},
		{2169, proto2169},
	}
	for _, pr := range protocols {
		p := clientVersionProtocol{Protocol: pr.make()}
		for _, c := range cases {
			t.Run(fmt.Sprintf("%d/%s", pr.id, c.name), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("ConvertToLatest panicked: %v", r)
					}
				}()
				out := p.ConvertToLatest(c.make(), (*minecraft.Conn)(nil))
				if len(out) == 0 {
					t.Fatalf("codec returned 0 packets: family silently dropped at read")
				}
			})
		}
	}
}

// TestHashedBlockRestoreIsAlignmentProof pins the restore mechanism directly:
// targetCreativeContent drops native items that have no target counterpart, so
// a POSITIONAL restore would give the surviving item the DROPPED item's hash.
func TestHashedBlockRestoreIsAlignmentProof(t *testing.T) {
	// var (not const): int32(0x80310e21+1) is a runtime conversion, a constant
	// conversion would be rejected as an int32 overflow by the compiler.
	var stone uint32 = 0x80310e21
	mk := func(items []protocol.CreativeItem) *packet.CreativeContent {
		return &packet.CreativeContent{
			Groups: []protocol.CreativeGroup{{Category: 1, Name: "itemGroup.name.all",
				Icon: protocol.ItemStack{ItemType: protocol.ItemType{NetworkID: 1}, BlockRuntimeID: int32(stone)}}},
			Items: items,
		}
	}
	item := func(id, block uint32) protocol.CreativeItem {
		return protocol.CreativeItem{CreativeItemNetworkID: id, GroupIndex: 0,
			Item: protocol.ItemStack{ItemType: protocol.ItemType{NetworkID: 1}, BlockRuntimeID: int32(block)}}
	}
	in := mk([]protocol.CreativeItem{item(1, stone+1), item(2, stone+2)})
	restore, ok := scrubHashedBlockIDs(in, true)
	if !ok {
		t.Fatal("creative content was not scrubbed")
	}
	if in.Items[0].Item.BlockRuntimeID != 0 || in.Items[1].Item.BlockRuntimeID != 0 {
		t.Fatal("input block IDs were not zeroed before conversion")
	}
	// Simulate the adapter dropping the first item. A flat positional restore
	// would put stone+1 into this survivor.
	out := mk([]protocol.CreativeItem{item(2, 0)})
	restore(out)
	if got := uint32(out.Items[0].Item.BlockRuntimeID); got != stone+2 {
		t.Fatalf("dropped item shifted the restore: got %#x, want %#x", got, stone+2)
	}
	if got := uint32(out.Groups[0].Icon.BlockRuntimeID); got != stone {
		t.Fatalf("group icon block ID %#x, want %#x", got, stone)
	}
	// verifyRoundTrip discards the output: the input must be restored too.
	restore(in)
	if uint32(in.Items[0].Item.BlockRuntimeID) != stone+1 || uint32(in.Items[1].Item.BlockRuntimeID) != stone+2 {
		t.Fatal("input packet was left scrubbed after the verify/dump path")
	}
}

// TestHashedBlockCreativeContentSurvivesCodec is the end-to-end regression for
// the measured alignment bug: 2076 native creative items -> 1933 (1001) /
// 1976 (2168, 2169) after filtering. Before the keyed restore, 1812/1933
// surviving items got the wrong StateHash from item 121 onward.
func TestHashedBlockCreativeContentSurvivesCodec(t *testing.T) {
	m, err := blocktranslate.New(2193, 1001, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	stone, ok := m.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone hash missing")
	}
	native, err := blocktranslate.NativeItems()
	if err != nil {
		t.Fatal(err)
	}
	makePK := func() *packet.CreativeContent {
		items := make([]protocol.CreativeItem, 0, len(native))
		for i, it := range native {
			items = append(items, protocol.CreativeItem{
				CreativeItemNetworkID: uint32(i + 1), GroupIndex: 0,
				Item: protocol.ItemStack{
					ItemType:       protocol.ItemType{NetworkID: int32(it.RuntimeID)},
					BlockRuntimeID: int32(stone + uint32(i)),
				},
			})
		}
		return &packet.CreativeContent{
			Groups: []protocol.CreativeGroup{{Category: 1, Name: "itemGroup.name.all",
				Icon: protocol.ItemStack{ItemType: protocol.ItemType{NetworkID: int32(native[0].RuntimeID)}, BlockRuntimeID: int32(stone)}}},
			Items: items,
		}
	}
	protocols := []struct {
		id   int32
		make func() minecraft.Protocol
	}{{1001, proto1001}, {2168, proto2168}, {2169, proto2169}}
	for _, pr := range protocols {
		t.Run(fmt.Sprintf("%d", pr.id), func(t *testing.T) {
			p := clientVersionProtocol{Protocol: pr.make()}
			in := makePK()
			out := p.ConvertFromLatest(in, (*minecraft.Conn)(nil))
			if len(out) != 1 {
				t.Fatalf("codec returned %d packets", len(out))
			}
			q, ok := unwrapWirePacket(out[0]).(*packet.CreativeContent)
			if !ok {
				t.Fatalf("converted packet has type %T", unwrapWirePacket(out[0]))
			}
			if len(q.Items) == 0 {
				t.Fatal("all creative items were dropped")
			}
			if len(q.Items) >= len(native) {
				t.Fatalf("adapter filtered no creative item (%d/%d): alignment stress not exercised", len(q.Items), len(native))
			}
			for i, it := range q.Items {
				want := stone + uint32(it.CreativeItemNetworkID-1)
				if got := uint32(it.Item.BlockRuntimeID); got != want {
					t.Fatalf("item %d creativeID=%d block %#x, want %#x (a positional restore fails here)",
						i, it.CreativeItemNetworkID, got, want)
				}
			}
			if got := uint32(q.Groups[0].Icon.BlockRuntimeID); got != stone {
				t.Fatalf("group icon block ID %#x, want %#x", got, stone)
			}
			for i, it := range in.Items {
				if uint32(it.Item.BlockRuntimeID) != stone+uint32(i) {
					t.Fatalf("input item %d was left scrubbed", i)
				}
			}
			t.Logf("proto=%d creative items %d -> %d, all %d hashed block IDs intact",
				pr.id, len(native), len(q.Items), len(q.Items)+1)
		})
	}
}

// TestHashedBlockCraftingDataSurvivesCodec pins the second list-filtering path:
// mapCraftingData drops recipes whose items have no target counterpart, so the
// recipe's RecipeNetworkID (not its slice index) must anchor the restore.
func TestHashedBlockCraftingDataSurvivesCodec(t *testing.T) {
	m, err := blocktranslate.New(2193, 1001, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	stone, ok := m.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone hash missing")
	}
	native, err := blocktranslate.NativeItems()
	if err != nil {
		t.Fatal(err)
	}
	makePK := func() *packet.CraftingData {
		recipes := make([]protocol.ShapedRecipe, 0, 8)
		for i := 0; i < 8; i++ {
			netID := int32(native[i*7].RuntimeID)
			if i == 3 {
				// Deliberately unmappable: filterShapedRecipes drops this recipe,
				// so a positional restore would shift every later recipe.
				netID = 99999
			}
			recipes = append(recipes, protocol.ShapedRecipe{
				RecipeID: fmt.Sprintf("xvrelay:%d", i),
				Width:    1, Height: 1,
				Output: []protocol.ItemStack{{
					ItemType:       protocol.ItemType{NetworkID: netID},
					BlockRuntimeID: int32(stone + uint32(i)),
				}},
				RecipeNetworkID: uint32(i + 1),
			})
		}
		return &packet.CraftingData{ShapedRecipes: recipes}
	}
	protocols := []struct {
		id   int32
		make func() minecraft.Protocol
	}{{1001, proto1001}, {2168, proto2168}, {2169, proto2169}}
	for _, pr := range protocols {
		t.Run(fmt.Sprintf("%d", pr.id), func(t *testing.T) {
			p := clientVersionProtocol{Protocol: pr.make()}
			in := makePK()
			out := p.ConvertFromLatest(in, (*minecraft.Conn)(nil))
			if len(out) != 1 {
				t.Fatalf("codec returned %d packets", len(out))
			}
			q, ok := unwrapWirePacket(out[0]).(*packet.CraftingData)
			if !ok {
				t.Fatalf("converted packet has type %T", unwrapWirePacket(out[0]))
			}
			if len(q.ShapedRecipes) == 0 {
				t.Fatal("all shaped recipes were dropped")
			}
			if len(q.ShapedRecipes) >= len(in.ShapedRecipes) {
				t.Fatalf("adapter filtered no recipe (%d/%d): alignment stress not exercised", len(q.ShapedRecipes), len(in.ShapedRecipes))
			}
			for _, r := range q.ShapedRecipes {
				if len(r.Output) == 0 {
					t.Fatalf("recipe %d lost its output stack", r.RecipeNetworkID)
				}
				want := stone + r.RecipeNetworkID - 1
				if got := uint32(r.Output[0].BlockRuntimeID); got != want {
					t.Fatalf("recipe %d output block %#x, want %#x", r.RecipeNetworkID, got, want)
				}
			}
			for i, r := range in.ShapedRecipes {
				if uint32(r.Output[0].BlockRuntimeID) != stone+uint32(i) {
					t.Fatalf("input recipe %d was left scrubbed", i)
				}
			}
			t.Logf("proto=%d shaped recipes %d -> %d, hashed block IDs intact",
				pr.id, len(in.ShapedRecipes), len(q.ShapedRecipes))
		})
	}
}
