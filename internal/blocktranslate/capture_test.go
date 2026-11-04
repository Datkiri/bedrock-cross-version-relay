package blocktranslate

import (
	"bytes"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"os"
	"path/filepath"
	"testing"
)

func TestDumpCapturesSuccessfulSubChunk(t *testing.T) {
	oldPath, oldDumped := DumpSubchunkPath, translateDumped
	DumpSubchunkPath, translateDumped = filepath.Join(t.TempDir(), "host.bin"), false
	defer func() { DumpSubchunkPath, translateDumped = oldPath, oldDumped }()
	mapper, err := New(1001, 2193, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := mapper.SourceID("minecraft:stone", nil)
	if !ok {
		t.Fatal("missing stone")
	}
	var raw bytes.Buffer
	raw.Write([]byte{9, 1, 0, 1})
	_ = protocol.WriteVarint32(&raw, int32(id))
	pk := &packet.SubChunk{SubChunkEntries: []protocol.SubChunkEntry{{Result: protocol.SubChunkResultSuccess, RawPayload: protocol.Option(raw.Bytes())}}}
	out, err := mapper.Translate(pk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(DumpSubchunkPath)
	if err != nil {
		t.Fatalf("successful payload not captured: %v", err)
	}
	if !bytes.Equal(got, raw.Bytes()) {
		t.Fatal("host capture changed")
	}
	translated, err := os.ReadFile(DumpSubchunkPath + ".translated.bin")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := out.(*packet.SubChunk).SubChunkEntries[0].RawPayload.Value()
	if !bytes.Equal(translated, want) {
		t.Fatal("translated capture does not match emitted payload")
	}
	if _, err = mapper.Translate(pk); err != nil {
		t.Fatal(err)
	}
}
