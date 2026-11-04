package blocktranslate

import (
	"os"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// TestDumpTranslateDiff translates a real payload and compares bytes with the original: only the
// palette may change — never version/layers/Y/words/length (except the truncated tail).
func TestDumpTranslateDiff(t *testing.T) {
	const path = "../../log/subchunk-dump.bin"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no dump available: %v", err)
	}
	m, err := New(1001, 2193, true, nil)
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	pk := &packet.SubChunk{
		CacheEnabled: false,
		Dimension:    0,
		Position:     protocol.SubChunkPos{0, 0, 0},
		SubChunkEntries: []protocol.SubChunkEntry{{
			Offset:     protocol.SubChunkOffset{0, 0, 0},
			Result:     protocol.SubChunkResultSuccess,
			RawPayload: protocol.Option(raw),
		}},
	}
	out, err := m.Translate(pk)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out == nil {
		t.Fatalf("translate returned nil (packet dropped)")
	}
	got, ok := out.(*packet.SubChunk)
	if !ok {
		t.Fatalf("type %T", out)
	}
	payload, _ := got.SubChunkEntries[0].RawPayload.Value()
	t.Logf("raw len=%d translated len=%d", len(raw), len(payload))
	if len(payload) == 0 {
		t.Fatalf("empty payload")
	}
	if payload[0] != raw[0] {
		t.Errorf("version byte changed: %d -> %d", raw[0], payload[0])
	}
	if payload[1] != raw[1] {
		t.Errorf("layers byte changed: %d -> %d", raw[1], payload[1])
	}
	n := len(payload)
	if n > len(raw) {
		n = len(raw)
	}
	var diffs int
	firstDiff := -1
	for i := 0; i < n; i++ {
		if payload[i] != raw[i] {
			diffs++
			if firstDiff < 0 {
				firstDiff = i
			}
		}
	}
	t.Logf("differing bytes in the common part: %d (first divergence: %d)", diffs, firstDiff)
	if firstDiff >= 0 && firstDiff < 4 {
		t.Errorf("divergence right in the payload header (offset %d) — the structure changed, not just the palette", firstDiff)
	}
}
