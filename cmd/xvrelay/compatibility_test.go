package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	v1_26_30 "github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
)

// Obtain a real, negotiated connection without accessing private Conn fields.
// No external game or Xbox service is used by these loopback tests.
func negotiatedConn(t *testing.T, id int32) *minecraft.Conn {
	t.Helper()
	codec, ok := resolveProto(id)
	if !ok {
		t.Fatalf("unknown test protocol %d", id)
	}
	l, err := (minecraft.ListenConfig{AuthenticationDisabled: true, AcceptedProtocols: []minecraft.Protocol{wrapClientVersion(codec, false)}}).Listen("raknet", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	go func() {
		c, err := (minecraft.Dialer{Protocol: codec, ClientData: login.ClientData{GameVersion: codec.Ver()}, IdentityData: login.IdentityData{DisplayName: "CodecTest"}}).DialContext(ctx, "raknet", l.Addr().String())
		if err == nil {
			_ = c.Close()
		}
	}()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	select {
	case c := <-accepted:
		if c == nil {
			t.Fatal("listener closed before negotiation")
		}
		t.Cleanup(func() { _ = c.Close() })
		return c.(*minecraft.Conn)
	case <-ctx.Done():
		t.Fatal("loopback negotiation timed out")
		return nil
	}
}

func TestLegacyChunkPreservesWorldData(t *testing.T) {
	// Captured host1001 payload, not emitted by the serializer under test:
	// capture-2026-10-26_18-19-55-18080/00014-HOST-OUT-58.bin.
	// Header: x=0,z=0,dim=0,count=0xfffffffe,limit=9,cache=false;
	// length=34 followed by biome/border data.
	raw, err := hex.DecodeString("000000feffffff0f09000022010801080108010801080108010801080108ffffffffffffffffffffffffffffff00")
	if err != nil {
		t.Fatal(err)
	}
	codec, _ := resolveProto(1001)
	pk := codec.Packets(false)[packet.IDLevelChunk]()
	pk.Marshal(codec.NewReader(bytes.NewBuffer(raw), 0, true))
	chunk := codec.ConvertToLatest(pk, nil)[0].(*packet.LevelChunk)
	if len(chunk.RawPayload) != 34 {
		t.Fatalf("bad fixture: payload=%d", len(chunk.RawPayload))
	}
	// KEEP-payload mode (the old default) — this checks data preservation on its own.
	prev := emptyShellPayload
	emptyShellPayload = false
	defer func() { emptyShellPayload = prev }()
	client := negotiatedConn(t, 2193)
	sessions.Store(client, &session{hostProto: 1001, capture: &spawnCapture{}})
	defer sessions.Delete(client)
	before := *chunk
	converted := client.Proto().ConvertFromLatest(chunk, client)
	if len(converted) != 1 {
		t.Fatalf("converted count=%d", len(converted))
	}
	got := converted[0].(*packet.LevelChunk)
	if !bytes.Equal(got.RawPayload, chunk.RawPayload) {
		t.Errorf("lost terrain payload: got %d bytes, want %d", len(got.RawPayload), len(chunk.RawPayload))
	}
	if limit, ok := got.SubChunkLimit.Value(); !ok || limit != 9 {
		t.Errorf("changed host request limit: %v", got.SubChunkLimit)
	}
	if got.SubChunkCount != 0 {
		t.Errorf("legacy sentinel not normalised: %d", got.SubChunkCount)
	}
	if !reflect.DeepEqual(*chunk, before) {
		t.Error("conversion mutated source")
	}
	// The native envelope is independently specified: varints 0,0,0,0;
	// optional=true, zigzag(9)=18, cache=false, hashes=0, payload length=34.
	var wire bytes.Buffer
	got.Marshal(minecraft.DefaultProtocol.NewWriter(&wire, 0))
	want := append([]byte{0, 0, 0, 0, 1, 18, 0, 0, 34}, chunk.RawPayload...)
	if !bytes.Equal(wire.Bytes(), want) {
		t.Error("native wire differs from independently specified envelope")
	}
}

// TestShellPayloadEmptyForCrossVersion: the new default — a request-mode shell sent to a client
// on a DIFFERENT version must carry an empty payload (a 1.26.32 client on the same protocol plays
// fine with a nil payload; a 1.26.51 client dies 0.5 s after receiving the 1001 payload).
func TestShellPayloadEmptyForCrossVersion(t *testing.T) {
	prev := emptyShellPayload
	emptyShellPayload = true
	defer func() { emptyShellPayload = prev }()
	client := negotiatedConn(t, 2193)
	sessions.Store(client, &session{hostProto: 1001, capture: &spawnCapture{}})
	defer sessions.Delete(client)
	chunk := &packet.LevelChunk{
		SubChunkCount: ^uint32(0) - 1,
		SubChunkLimit: protocol.Option(int32(9)),
		RawPayload:    []byte{1, 2, 3, 4},
	}
	got := client.Proto().ConvertFromLatest(chunk, client)[0].(*packet.LevelChunk)
	if len(got.RawPayload) != 0 {
		t.Errorf("payload must be empty for a cross-version client, got %d bytes", len(got.RawPayload))
	}
	if got.SubChunkCount != 0 {
		t.Errorf("count must be 0, got %d", got.SubChunkCount)
	}
	if v, ok := got.SubChunkLimit.Value(); !ok || v != 9 {
		t.Errorf("limit must stay 9, got %v", got.SubChunkLimit)
	}
}

func TestNativeChunkPreservesCacheAndPayload(t *testing.T) {
	prev := emptyShellPayload
	emptyShellPayload = false
	defer func() { emptyShellPayload = prev }()
	client := negotiatedConn(t, 2193)
	sessions.Store(client, &session{hostProto: 1001, capture: &spawnCapture{}})
	defer sessions.Delete(client)
	for _, limit := range []int32{-1, 0, 9} {
		chunk := &packet.LevelChunk{SubChunkCount: ^uint32(0) - 1, SubChunkLimit: protocol.Option(limit), CacheEnabled: true, BlobHashes: []uint64{123}, RawPayload: []byte{0, 10}}
		got := client.Proto().ConvertFromLatest(chunk, client)[0].(*packet.LevelChunk)
		if !got.CacheEnabled || !reflect.DeepEqual(got.BlobHashes, chunk.BlobHashes) || !bytes.Equal(got.RawPayload, chunk.RawPayload) {
			t.Errorf("cache/inline data lost for limit %d", limit)
		}
		if v, _ := got.SubChunkLimit.Value(); v != limit {
			t.Errorf("limit=%d, want %d", v, limit)
		}
	}
}

func TestInlineNativeChunkUnchanged(t *testing.T) {
	client := negotiatedConn(t, 2193)
	sessions.Store(client, &session{hostProto: 2193, capture: &spawnCapture{}})
	defer sessions.Delete(client)
	chunk := &packet.LevelChunk{SubChunkCount: 2, RawPayload: []byte{1, 2, 3}}
	got := client.Proto().ConvertFromLatest(chunk, client)[0]
	if got != chunk {
		t.Fatal("same-wire inline chunk changed")
	}
}

func numericIngredientRecipe() []byte {
	// One shapeless recipe: numeric item5, meta0, count1; no outputs.
	// Recipe envelopes are hand-written according to the 1001 schema.
	raw := []byte{1, 0, 4, 't', 'e', 's', 't', 1, 1, 5, 0, 0, 0, 2, 0}
	raw = append(raw, make([]byte, 16)...)
	raw = append(raw, 14)
	raw = append(raw, "crafting_table"...)
	return append(raw, 0, 1, 1, 0, 0, 0, 1)
}

func TestLegacyNumericIngredientUsesHostRegistry(t *testing.T) {
	raw := numericIngredientRecipe()
	// Demonstrate the pinned adapter loses the numeric ID before conversion.
	base, _ := resolveProto(1001)
	pk := base.Packets(false)[packet.IDCraftingData]()
	pk.Marshal(base.NewReader(bytes.NewBuffer(raw), 0, true))
	broken := base.ConvertToLatest(pk, nil)[0].(*packet.CraftingData)
	if d := broken.ShapelessRecipes[0].Input[0].Descriptor.(*protocol.DefaultItemDescriptor); d.Name != "" {
		t.Fatal("upstream changed; re-evaluate the regression")
	}
	for _, name := range []string{"minecraft:bed", "minecraft:lime_wool"} {
		got, err := decodeHostRecipes1001(raw, []protocol.ItemEntry{{Name: name, RuntimeID: 5}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		d := got.ShapelessRecipes[0].Input[0].Descriptor.(*protocol.DefaultItemDescriptor)
		if d.Name != name {
			t.Fatalf("ingredient=%q, want per-session registry name %q", d.Name, name)
		}
		if got.ShapelessRecipes[0].Input[0].Count != 1 {
			t.Fatal("ingredient count changed")
		}
	}
}

func TestLegacyRecipesRejectUnknownNumericIngredient(t *testing.T) {
	if _, err := decodeHostRecipes1001(numericIngredientRecipe(), nil, 0); err == nil {
		t.Fatal("unknown numeric item must not become an empty ingredient")
	}
	if _, err := decodeHostRecipes1001([]byte{1}, nil, 0); err == nil {
		t.Fatal("truncated recipe must fail")
	}
}

func TestHostRecipeFailureClosesConnection(t *testing.T) {
	conn := negotiatedConn(t, 1001)
	base, _ := resolveProto(1001)
	p := host1001Protocol{Protocol: base}
	pk := p.Packets(false)[packet.IDCraftingData]()
	pk.Marshal(p.NewReader(bytes.NewBuffer(numericIngredientRecipe()), 0, true))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("missing host registry must fail conversion")
			}
		}()
		p.ConvertToLatest(pk, conn)
	}()
	select {
	case <-conn.Context().Done():
	default:
		t.Fatal("invalid recipes left connection open to silently skip CraftingData")
	}
}

func TestLegacyRecipeFamilies(t *testing.T) {
	input := protocol.ItemDescriptorCount{Descriptor: &protocol.DefaultItemDescriptor{Name: "minecraft:stone", MetadataValue: -1}, Count: 1}
	unlock := protocol.Option(protocol.RecipeUnlockRequirement{Context: protocol.RecipeUnlockContextNone, Ingredients: []protocol.ItemDescriptorCount{input}})
	shaped := protocol.ShapedRecipe{RecipeID: "shape", Width: 1, Height: 1, Input: []protocol.ItemDescriptorCount{input}, Block: "crafting_table", UnlockRequirement: unlock, RecipeNetworkID: 1}
	shapeless := protocol.ShapelessRecipe{RecipeID: "loose", Input: []protocol.ItemDescriptorCount{input}, Block: "crafting_table", UnlockRequirement: unlock, RecipeNetworkID: 2}
	want := &packet.CraftingData{
		ShapedRecipes: []protocol.ShapedRecipe{shaped}, ShapelessRecipes: []protocol.ShapelessRecipe{shapeless},
		MultiRecipes:                 []protocol.MultiRecipe{{RecipeNetworkID: 3}},
		UserDataShapelessRecipes:     []protocol.UserDataShapelessRecipe{{ShapelessRecipe: shapeless}},
		ShapelessChemistryRecipes:    []protocol.ShapelessChemistryRecipe{{ShapelessRecipe: shapeless}},
		ShapedChemistryRecipes:       []protocol.ShapedChemistryRecipe{{ShapedRecipe: shaped}},
		SmithingTransformRecipes:     []protocol.SmithingTransformRecipe{{RecipeID: "smith", Template: input, Base: input, Addition: input, Block: "smithing_table", RecipeNetworkID: 4}},
		SmithingTrimRecipes:          []protocol.SmithingTrimRecipe{{RecipeID: "trim", Template: input, Base: input, Addition: input, Block: "smithing_table", RecipeNetworkID: 5}},
		PotionRecipes:                []protocol.PotionRecipe{{InputPotionID: 1, OutputPotionID: 2}},
		PotionContainerChangeRecipes: []protocol.PotionContainerChangeRecipe{{InputItemID: 1, OutputItemID: 2}}, ClearRecipes: true,
	}
	// Recipe families are a test of the 1001 WIRE encoder. The registry-aware adapter
	// (NewWithRegistries) deliberately drops CraftingData while the connection has no item
	// registry yet, so use the wire-only adapter here to exercise the codec.
	base := v1_26_30.New()
	var raw bytes.Buffer
	base.ConvertFromLatest(want, nil)[0].Marshal(base.NewWriter(&raw, 0))
	got, err := decodeHostRecipes1001(raw.Bytes(), []protocol.ItemEntry{{Name: "minecraft:stone", RuntimeID: 1}, {Name: "minecraft:grass_block", RuntimeID: 2}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Compare native encodings to treat absent/empty slices as equivalent.
	var a, b bytes.Buffer
	want.Marshal(minecraft.DefaultProtocol.NewWriter(&a, 0))
	got.Marshal(minecraft.DefaultProtocol.NewWriter(&b, 0))
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("legacy recipe family changed during conversion")
	}
	if _, err := decodeHostRecipes1001(append(raw.Bytes(), 0), nil, 0); err == nil {
		t.Fatal("trailing garbage accepted")
	}
}

func storeClientGate(addr net.Addr, gate *spawnPrefixGate) *clientGate {
	g := &clientGate{gate: gate}
	clientGates.Store(addr.String(), g)
	return g
}

func TestSpawnGateSessionIsolation(t *testing.T) {
	a, b := newSpawnPrefixGate(), newSpawnPrefixGate()
	addrA, addrB := &net.UDPAddr{Port: 41001}, &net.UDPAddr{Port: 41002}
	ga, gb := storeClientGate(addrA, a), storeClientGate(addrB, b)
	defer func() { clientGates.Delete(addrA.String()) }()
	defer func() { clientGates.Delete(addrB.String()) }()
	clientPacketFunc(packet.Header{PacketID: packet.IDCreativeContent}, []byte{0, 0}, addrA, nil)
	select {
	case <-a.done():
	default:
		t.Fatal("session A gate did not open")
	}
	select {
	case <-b.done():
		t.Fatal("session A opened session B gate")
	default:
	}
	if n := ga.count.Load(); n != 1 {
		t.Fatalf("session A counter = %d, want 1", n)
	}
	if n := gb.count.Load(); n != 0 {
		t.Fatalf("session B counter = %d, want 0", n)
	}
}

// TestStaleSessionKeepsNewGate encodes a race that really happened (log 13-33-24, session 13:40):
// the old session ended AFTER the new one had already Stored over the same address ⇒ the old
// session's deferred Delete must NOT wipe the new session's gate, otherwise every "client sent"
// line (including id=113) from the new session disappears from the log.
func TestStaleSessionKeepsNewGate(t *testing.T) {
	addr := &net.UDPAddr{Port: 41003}
	old := storeClientGate(addr, newSpawnPrefixGate())
	fresh := storeClientGate(addr, newSpawnPrefixGate()) // the new session Stores over the same key

	// The old session ends: delete only if the gate is still its own.
	if cur, ok := clientGates.Load(addr.String()); ok && cur == old {
		clientGates.Delete(addr.String())
	}

	// The new session's gate and log must be intact.
	clientPacketFunc(packet.Header{PacketID: packet.IDCreativeContent}, []byte{0, 0}, addr, nil)
	select {
	case <-fresh.gate.done():
	default:
		t.Fatal("stale session delete wiped the fresh session gate")
	}
	if n := fresh.count.Load(); n != 1 {
		t.Fatalf("fresh session counter = %d, want 1", n)
	}
	clientGates.Delete(addr.String())
}

// TestClientGateSees113: PacketFunc must still record id=113 even past the 80-packet log limit —
// this is the packet that closes conn.spawn ⇒ StartGame returns nil.
func TestClientGateSees113(t *testing.T) {
	addr := &net.UDPAddr{Port: 41004}
	g := storeClientGate(addr, newSpawnPrefixGate())
	defer clientGates.Delete(addr.String())
	g.count.Store(200) // past the 80-packet threshold
	clientPacketFunc(packet.Header{PacketID: packet.IDSetLocalPlayerAsInitialised}, make([]byte, 9), addr, nil)
	if !g.saw113.Load() {
		t.Fatal("id=113 not recorded")
	}
}

func TestDimensionAutoPreservesVanillaHost(t *testing.T) {
	previous := dimDataMode
	defer func() { dimDataMode = previous }()
	dimDataMode = "auto"
	gd := minecraft.GameData{}
	applyDimensionFallback(&gd)
	if len(gd.Dimensions) != 0 {
		t.Fatal("auto injected data-driven definitions that override vanilla dimensions")
	}
	want := []protocol.DimensionDefinition{{Name: "test:dimension", DimensionType: 1000, MinimumY: -32, HeightRange: 255}}
	gd.Dimensions = want
	applyDimensionFallback(&gd)
	if !reflect.DeepEqual(gd.Dimensions, want) {
		t.Fatal("host custom dimension definitions changed")
	}
}

// TestIsTransientDialErr: only IMMEDIATE errors are retried — "deadline exceeded" has already
// spent the whole dialTimeout (the host really is not answering), so retrying would only hang.
func TestIsTransientDialErr(t *testing.T) {
	cases := map[string]bool{
		"decode batch: empty packet":                              true,
		"signal offer: no address known for network ID 12345":     true,
		"nethernet: no address known for network ID 12345":        true,
		"dial: connect: connection refused":                       true,
		"read: connection reset by peer":                          true,
		"decompress batch: unexpected compression algorithm":      true,
		"dial minecraft …: context deadline exceeded":             false,
		"cannot dial the host with any protocol [2193]: …":            false,
	}
	for msg, want := range cases {
		if got := isTransientDialErr(errors.New(msg)); got != want {
			t.Errorf("isTransientDialErr(%q)=%v, want %v", msg, got, want)
		}
	}
	if isTransientDialErr(nil) {
		t.Error("nil must be false")
	}
}
