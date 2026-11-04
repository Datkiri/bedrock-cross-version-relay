package blocktranslate

import (
	"bytes"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestPaletteRewriterV8V9AndInlineTail(t *testing.T) {
	m, _ := New(2193, 1001, true, nil)
	id, ok := m.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone missing")
	}
	wantID, _ := m.Map(id)
	for _, version := range []byte{8, 9} {
		raw := []byte{version, 1}
		if version == 9 {
			raw = append(raw, 0xfc)
		}
		raw = append(raw, 1)
		var b bytes.Buffer
		v := int32(id)
		protocol.WriteVarint32(&b, v)
		raw = append(raw, b.Bytes()...)
		got, used, err := rewriteSubChunk(raw, m.Map, false)
		if err != nil {
			t.Fatal(err)
		}
		if used != len(raw) {
			t.Fatal("partial rewrite")
		}
		want := raw[:len(raw)-b.Len()]
		var tail bytes.Buffer
		protocol.WriteVarint32(&tail, int32(wantID))
		want = append(append([]byte(nil), want...), tail.Bytes()...)
		if !bytes.Equal(got, want) {
			t.Fatal("palette translation changed unrelated data")
		}
		inline := &packet.LevelChunk{SubChunkCount: 1, RawPayload: append(append([]byte(nil), raw...), 0xaa, 0xbb)}
		pk, err := m.Translate(inline)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pk.(*packet.LevelChunk).RawPayload, append(append([]byte(nil), want...), 0xaa, 0xbb)) {
			t.Fatal("inline biome/tile tail changed")
		}
	}
}

// A malformed payload is still an ERROR at the Map/rewriteSubChunk layer, but Translate (the
// production path) must NOT kill the session: it returns the original packet so the player stays
// in the world. Evidence: at log 22:04:25 the client had spawned and then died with
// "subchunk14 has 58 unparsed bytes".
func TestTranslationRejectsUnknownAndPassesMalformedThrough(t *testing.T) {
	m, _ := New(2193, 1001, true, nil)
	// An unknown ID (a custom block from a pack) maps to a visible placeholder, NOT an error —
	// erroring here used to hand the client a garbage ID and it disconnected.
	if got, err := m.Map(0x01234567); err != nil {
		t.Fatalf("unknown source hash must return the placeholder, got error: %v", err)
	} else if got != m.fallbackID {
		t.Fatalf("unknown source hash must map to placeholder %#x, got %#x", m.fallbackID, got)
	}
	if _, _, err := rewriteSubChunk([]byte{9, 1, 0, 1, 0x80}, m.Map, false); err == nil {
		t.Fatal("truncated varint accepted")
	}
	cached := &packet.LevelChunk{CacheEnabled: true}
	got, err := m.Translate(cached)
	if err != nil {
		t.Fatalf("cached chunk must pass through, got error: %v", err)
	}
	if got != packet.Packet(cached) {
		t.Fatal("cached chunk must return the original packet unchanged")
	}
}

func TestUpdateBlockUsesSameMappingAsChunk(t *testing.T) {
	m, _ := New(2193, 1001, false, nil)
	id, _ := m.SourceID("minecraft:stone", nil)
	want, _ := m.Map(id)
	src := &packet.UpdateBlock{NewBlockRuntimeID: id, Flags: 3, Layer: 1}
	got, err := m.Translate(src)
	if err != nil {
		t.Fatal(err)
	}
	x := got.(*packet.UpdateBlock)
	if x.NewBlockRuntimeID != want || x.Flags != 3 || x.Layer != 1 {
		t.Fatal("block update mismatch")
	}
	if src.NewBlockRuntimeID != id {
		t.Fatal("source mutated")
	}
}

func TestClickedBlockReverseTranslation(t *testing.T) {
	m, _ := New(1001, 2193, false, nil)
	id, ok := m.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("stone missing")
	}
	want, _ := m.Map(id)
	tx := &protocol.UseItemTransactionData{ActionType: protocol.UseItemActionClickBlock, BlockRuntimeID: id}
	pk := &packet.InventoryTransaction{TransactionData: tx}
	got, err := m.TranslateInteractions(pk)
	if err != nil {
		t.Fatal(err)
	}
	if got.(*packet.InventoryTransaction).TransactionData.(*protocol.UseItemTransactionData).BlockRuntimeID != want {
		t.Fatal("clicked block not translated")
	}
	if tx.BlockRuntimeID != id {
		t.Fatal("source mutated")
	}
	auth := &packet.PlayerAuthInput{ItemInteractionData: protocol.Option(*tx)}
	got, err = m.TranslateInteractions(auth)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := got.(*packet.PlayerAuthInput).ItemInteractionData.Value()
	if x.BlockRuntimeID != want {
		t.Fatal("auth-input clicked block not translated")
	}
}
