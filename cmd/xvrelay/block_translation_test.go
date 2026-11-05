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
