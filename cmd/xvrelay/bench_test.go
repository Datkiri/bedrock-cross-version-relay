package main

// Hot-path benchmarks for the three things the relay actually spends time on:
//   1. codec conversion      ConvertFromLatest (write to client) / ConvertToLatest (read from host)
//   2. item mapping          registry-aware gameplay IDs, creative inventory, host-1001 ID fix
//   3. chunk translation     block palette mapper + LevelChunk / SubChunk rewrite
//
// Everything is offline: no BDS, no listener, no network. The codecs are the SAME adapters the
// relay uses (resolveProto / registryAwareCodec), driven with a nil *minecraft.Conn — which is
// exactly the offline path clientVersionProtocol takes when conn == nil. The relay's per-session
// bookkeeping (spawn capture, StartGame rebuild, raw-forward) is NOT included here.
//
// Run with:  make bench   (or: go test -buildvcs=false ./... -run '^$' -bench . -benchmem)

import (
	"bytes"
	"encoding/binary"
	"testing"

	"bedrock-cross-version-relay/internal/blocktranslate"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	v1_26_30 "github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
	v1_26_44 "github.com/shawtymarco/go-multiversion/protocols/v1_26_44"
	v1_26_45 "github.com/shawtymarco/go-multiversion/protocols/v1_26_45"
)

// Package-level sinks keep the compiler from proving the conversion result unused. Assigning a
// slice/map header costs no allocation, so it does not distort allocs/op.
var (
	benchPacketSink   []packet.Packet
	benchMapperSink   *blocktranslate.Mapper
	benchIDByIDSink   map[int32]string
	benchNameByIDSink map[string]int32
)

const benchInventorySlots = 36 // a vanilla player inventory window

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// benchInventory builds an InventoryContent whose slots carry REAL runtime IDs from `entries`.
// IDs are cycled so every slot is a valid item, not a hard-coded guess.
func benchInventory(entries []protocol.ItemEntry, slots int) *packet.InventoryContent {
	content := make([]protocol.ItemInstance, slots)
	for i := range content {
		id := int32(entries[i%len(entries)].RuntimeID)
		content[i] = protocol.ItemInstance{
			Stack: protocol.ItemStack{
				ItemType: protocol.ItemType{NetworkID: id},
				Count:    1,
			},
		}
	}
	return &packet.InventoryContent{Content: content}
}

// benchCreative builds a full CreativeContent (one group) from REAL runtime IDs.
func benchCreative(entries []protocol.ItemEntry) *packet.CreativeContent {
	groups := []protocol.CreativeGroup{{
		Category: 1,
		Name:     "itemGroup.name.all",
		Icon: protocol.ItemStack{
			ItemType: protocol.ItemType{NetworkID: int32(entries[0].RuntimeID)},
			Count:    1,
		},
	}}
	items := make([]protocol.CreativeItem, 0, len(entries))
	for i, e := range entries {
		items = append(items, protocol.CreativeItem{
			CreativeItemNetworkID: uint32(i + 1),
			Item: protocol.ItemStack{
				ItemType: protocol.ItemType{NetworkID: int32(e.RuntimeID)},
				Count:    1,
			},
			GroupIndex: 0,
		})
	}
	return &packet.CreativeContent{Groups: groups, Items: items}
}

// benchPutVarint writes the zigzag varint that rewriteSubChunk's readVar() decodes.
func benchPutVarint(dst *bytes.Buffer, v int32) {
	zigzag := uint64(uint32(v<<1) ^ uint32(v>>31))
	var tmp [10]byte
	n := binary.PutUvarint(tmp[:], zigzag)
	dst.Write(tmp[:n])
}

// benchChunkPayload is a realistic v8 sub-chunk: one layer, 4 bits per block
// (512 packed 32-bit words = 2048 bytes) and a 16-entry network palette.
func benchChunkPayload(paletteSize int) []byte {
	var b bytes.Buffer
	b.WriteByte(8) // version v8 (a 1.26.30 host sends v8)
	b.WriteByte(1) // storage count
	b.WriteByte(9) // bits=4<<1|1 → network palette, 4 bits/block
	b.Write(make([]byte, 512*4))
	benchPutVarint(&b, int32(paletteSize))
	for i := 0; i < paletteSize; i++ {
		benchPutVarint(&b, int32(i+1))
	}
	return b.Bytes()
}

// ---------------------------------------------------------------------------
// Codecs
// ---------------------------------------------------------------------------

type benchCodec struct {
	name string
	p    minecraft.Protocol
	// from: native 2193 IDs → ConvertFromLatest (host → client, WRITING)
	// to:   target protocol IDs → ConvertToLatest (client → host, READING)
	from *packet.InventoryContent
	to   *packet.InventoryContent
}

func benchCodecs(tb testing.TB) []benchCodec {
	tb.Helper()
	reg, err := blocktranslate.NativeRegistry(2193)
	if err != nil {
		tb.Fatalf("NativeRegistry(2193): %v", err)
	}
	native, err := blocktranslate.NativeItems()
	if err != nil {
		tb.Fatalf("NativeItems(): %v", err)
	}
	targets := map[int32][]protocol.ItemEntry{}
	for _, id := range []int32{1001, 2168, 2169, 2193} {
		items, err := blocktranslate.Items(id)
		if err != nil {
			tb.Fatalf("Items(%d): %v", id, err)
		}
		targets[id] = items
	}

	must := func(name string, p minecraft.Protocol, err error, target int32) benchCodec {
		if err != nil {
			tb.Fatalf("build %s codec: %v", name, err)
		}
		return benchCodec{
			name: name,
			p:    p,
			from: benchInventory(native, benchInventorySlots),
			to:   benchInventory(targets[target], benchInventorySlots),
		}
	}
	out := []benchCodec{}
	p30, err := v1_26_30.NewWithRegistries(reg, native)
	out = append(out, must("1001/registry", p30, err, 1001))
	out = append(out, must("1001/wire-only", v1_26_30.New(), nil, 1001))
	p44, err := v1_26_44.NewWithRegistries(reg, native)
	out = append(out, must("2168/registry", p44, err, 2168))
	out = append(out, must("2168/wire-only", v1_26_44.New(), nil, 2168))
	p45, err := v1_26_45.NewWithRegistries(reg, native)
	out = append(out, must("2169/registry", p45, err, 2169))
	out = append(out, must("2169/wire-only", v1_26_45.New(), nil, 2169))
	out = append(out, must("2193/identity", minecraft.DefaultProtocol, nil, 2193))
	return out
}

// benchConvert times one conversion direction over a fixed, immutable input packet.
// The pinned adapters clone their input before mapping (see v1_26_30/gameplay_mapping.go and
// v1_26_45/gameplay_mapping.go), so reusing the same packet across iterations is safe.
func benchConvert(b *testing.B, p minecraft.Protocol, pk packet.Packet, toLatest bool) {
	b.Helper()
	convert := p.ConvertFromLatest
	if toLatest {
		convert = p.ConvertToLatest
	}
	if out := convert(pk, nil); len(out) == 0 {
		b.Fatalf("conversion produced no packets")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchPacketSink = convert(pk, nil)
	}
}

func BenchmarkCodecInventoryConvertFromLatest(b *testing.B) {
	for _, c := range benchCodecs(b) {
		b.Run(c.name, func(b *testing.B) { benchConvert(b, c.p, c.from, false) })
	}
}

func BenchmarkCodecInventoryConvertToLatest(b *testing.B) {
	for _, c := range benchCodecs(b) {
		b.Run(c.name, func(b *testing.B) { benchConvert(b, c.p, c.to, true) })
	}
}

// CreativeContent is the single largest item-carrying packet a session receives: the whole
// creative inventory (2076 native entries) is re-mapped on every spawn.
func BenchmarkCodecCreativeContentConvertFromLatest(b *testing.B) {
	native, err := blocktranslate.NativeItems()
	if err != nil {
		b.Fatalf("NativeItems(): %v", err)
	}
	creative := benchCreative(native)
	for _, c := range benchCodecs(b) {
		b.Run(c.name, func(b *testing.B) { benchConvert(b, c.p, creative, false) })
	}
}

// ---------------------------------------------------------------------------
// Item mapping
// ---------------------------------------------------------------------------

// BenchmarkItemMapHostCreative1001 measures the host-1001 fix: resolve every creative ID by
// NAME through the host ItemRegistry into native 2193 IDs (the bug found live on 2026-11-05).
func BenchmarkItemMapHostCreative1001(b *testing.B) {
	host, err := blocktranslate.Items(1001)
	if err != nil {
		b.Fatalf("Items(1001): %v", err)
	}
	creative := benchCreative(host)
	if _, _, err := mapHostCreative1001(creative, host); err != nil {
		b.Fatalf("mapHostCreative1001: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mapped, _, err := mapHostCreative1001(creative, host)
		if err != nil {
			b.Fatal(err)
		}
		benchPacketSink = []packet.Packet{mapped}
	}
}

// BenchmarkItemIndexBuild measures the per-session cost of indexing the client's item table,
// as cmd/clientclone and the relay's item-audit path do once at spawn.
func BenchmarkItemIndexBuild(b *testing.B) {
	entries, err := blocktranslate.Items(2193)
	if err != nil {
		b.Fatalf("Items(2193): %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		byID := make(map[int32]string, len(entries))
		byName := make(map[string]int32, len(entries))
		for _, e := range entries {
			byID[int32(e.RuntimeID)] = e.Name
			byName[e.Name] = int32(e.RuntimeID)
		}
		benchIDByIDSink, benchNameByIDSink = byID, byName
	}
}

// ---------------------------------------------------------------------------
// Chunk translation
// ---------------------------------------------------------------------------

func benchMapper(b testing.TB) (*blocktranslate.Mapper, []byte) {
	b.Helper()
	m, err := blocktranslate.New(1001, 2193, false, nil)
	if err != nil {
		b.Fatalf("blocktranslate.New(1001, 2193): %v", err)
	}
	payload := benchChunkPayload(16)
	// Sanity: the fixture must actually parse, otherwise the benchmark would just time an
	// early error return (which Translate silently converts into a pass-through).
	lc := &packet.LevelChunk{SubChunkCount: 1, RawPayload: payload}
	out, err := m.Translate(lc)
	if err != nil || out == lc {
		b.Fatalf("benchmark fixture does not translate: err=%v out=%T", err, out)
	}
	return m, payload
}

func BenchmarkBlockMapperNew(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m, err := blocktranslate.New(1001, 2193, false, nil)
		if err != nil {
			b.Fatal(err)
		}
		benchMapperSink = m
	}
}

func BenchmarkBlockMapperMap(b *testing.B) {
	m, _ := benchMapper(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Map(uint32(i % 16)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBlockTranslateLevelChunk(b *testing.B) {
	m, payload := benchMapper(b)
	lc := &packet.LevelChunk{SubChunkCount: 1, RawPayload: payload}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := m.Translate(lc)
		if err != nil {
			b.Fatal(err)
		}
		benchPacketSink = []packet.Packet{out}
	}
}

func BenchmarkBlockTranslateSubChunk(b *testing.B) {
	m, payload := benchMapper(b)
	sc := &packet.SubChunk{
		Position: protocol.SubChunkPos{0, 4, 0},
		SubChunkEntries: []protocol.SubChunkEntry{{
			Result:     protocol.SubChunkResultSuccess,
			RawPayload: protocol.Option(payload),
		}},
	}
	if out, err := m.Translate(sc); err != nil || out == nil {
		b.Fatalf("SubChunk fixture did not translate: err=%v out=%v", err, out)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := m.Translate(sc)
		if err != nil {
			b.Fatal(err)
		}
		benchPacketSink = []packet.Packet{out}
	}
}

func BenchmarkBlockTranslateUpdateBlock(b *testing.B) {
	m, _ := benchMapper(b)
	ub := &packet.UpdateBlock{NewBlockRuntimeID: 1, Flags: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := m.Translate(ub)
		if err != nil {
			b.Fatal(err)
		}
		benchPacketSink = []packet.Packet{out}
	}
}

// TestBenchFixtures keeps the benchmark inputs honest under a plain `go test ./...` (which does
// NOT run benchmarks). A fixture that silently fails to parse would make a benchmark look fast
// while measuring nothing, so CI asserts the same conversion/translation paths really produce
// packets and that the v8 sub-chunk payload is a real, parseable one.
func TestBenchFixtures(t *testing.T) {
	codecs := benchCodecs(t)
	for _, c := range codecs {
		if out := c.p.ConvertFromLatest(c.from, nil); len(out) == 0 {
			t.Errorf("%s: ConvertFromLatest produced no packets", c.name)
		}
		if out := c.p.ConvertToLatest(c.to, nil); len(out) == 0 {
			t.Errorf("%s: ConvertToLatest produced no packets", c.name)
		}
	}

	native, err := blocktranslate.NativeItems()
	if err != nil {
		t.Fatalf("NativeItems(): %v", err)
	}
	creative := benchCreative(native)
	for _, c := range codecs {
		if out := c.p.ConvertFromLatest(creative, nil); len(out) == 0 {
			t.Errorf("%s: CreativeContent ConvertFromLatest produced no packets", c.name)
		}
	}

	host, err := blocktranslate.Items(1001)
	if err != nil {
		t.Fatalf("Items(1001): %v", err)
	}
	mapped, st, err := mapHostCreative1001(benchCreative(host), host)
	if err != nil {
		t.Fatalf("mapHostCreative1001: %v", err)
	}
	if len(mapped.Items) == 0 || st.mapped == 0 {
		t.Errorf("host-1001 creative fixture mapped nothing: items=%d stats=%+v", len(mapped.Items), st)
	}

	// A LevelChunk carrying the v8 fixture must come back as a DIFFERENT packet: Translate returns
	// the input unchanged when rewriteSubChunk rejects the payload.
	m, payload := benchMapper(t)
	lc := &packet.LevelChunk{SubChunkCount: 1, RawPayload: payload}
	out, err := m.Translate(lc)
	if err != nil {
		t.Fatalf("Translate(LevelChunk): %v", err)
	}
	translated, ok := out.(*packet.LevelChunk)
	if !ok || translated == lc || len(translated.RawPayload) == 0 {
		t.Fatalf("v8 LevelChunk fixture was not rewritten: out=%T", out)
	}
	sc := &packet.SubChunk{
		Position: protocol.SubChunkPos{0, 4, 0},
		SubChunkEntries: []protocol.SubChunkEntry{{
			Result:     protocol.SubChunkResultSuccess,
			RawPayload: protocol.Option(payload),
		}},
	}
	if out, err := m.Translate(sc); err != nil || out == nil {
		t.Fatalf("Translate(SubChunk): err=%v out=%v", err, out)
	}
}
