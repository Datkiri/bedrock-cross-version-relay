package blocktranslate

import (
	"bytes"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"os"
	"testing"
)

func TestSubChunkPreservesBlockEntityTail(t *testing.T) {
	tail, err := nbt.MarshalEncoding(map[string]any{"id": "Chest", "x": int32(1), "y": int32(-64), "z": int32(-1)}, nbt.NetworkLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	raw := append([]byte{9, 0, 0xfc}, tail...)
	out, n, err := rewriteSubChunk(raw, func(id uint32) (uint32, error) { return id, nil }, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(raw) || !bytes.Equal(raw, out) {
		t.Fatalf("valid block entity data lost: input=%d output=%d consumed=%d", len(raw), len(out), n)
	}
}

func TestCapturedSubChunkPreservesBlockEntities(t *testing.T) {
	path := os.Getenv("XVER_TERRAIN_CAPTURE")
	if path == "" {
		t.Skip("set XVER_TERRAIN_CAPTURE to inspect actual host payload")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, used, err := rewriteSubChunk(raw, func(id uint32) (uint32, error) { return id, nil }, false)
	if err != nil {
		t.Fatal(err)
	}
	tail := bytes.NewBuffer(raw[used:])
	count := 0
	for tail.Len() > 0 {
		var entity map[string]any
		if err = nbt.NewDecoderWithEncoding(tail, nbt.NetworkLittleEndian).Decode(&entity); err != nil {
			t.Fatalf("invalid source block entity NBT: %v", err)
		}
		count++
		t.Logf("host blockEntity%d type=%v fields=%d", count, entity["id"], len(entity))
	}
	t.Logf("host payload=%d palette=%d validNBTtail=%d entities=%d", len(raw), used, len(raw)-used, count)
	mapper, err := New(1001, 2193, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := mapper.Translate(&packet.SubChunk{SubChunkEntries: []protocol.SubChunkEntry{{Result: protocol.SubChunkResultSuccess, RawPayload: protocol.Option(raw)}}})
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("capture discarded")
	}
	converted, _ := out.(*packet.SubChunk).SubChunkEntries[0].RawPayload.Value()
	_, outUsed, err := rewriteSubChunk(converted, func(id uint32) (uint32, error) { return id, nil }, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(converted[outUsed:], raw[used:]) {
		t.Fatalf("captured valid NBT lost: sourceTail=%d targetTail=%d", len(raw)-used, len(converted)-outUsed)
	}
}
