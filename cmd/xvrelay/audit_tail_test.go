package main

import (
	"bytes"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"log/slog"
	"testing"
)

func TestAuditAcceptsValidBlockEntityNBT(t *testing.T) {
	oldLogger, oldOn := slog.Default(), auditOn
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	auditOn = true
	defer func() { slog.SetDefault(oldLogger); auditOn = oldOn }()
	tail, err := nbt.MarshalEncoding(map[string]any{"id": "CommandBlock", "x": int32(0), "y": int32(-64), "z": int32(0)}, nbt.NetworkLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	raw := append([]byte{9, 0, 0xfc}, tail...)
	auditSubChunk(&packet.SubChunk{SubChunkEntries: []protocol.SubChunkEntry{{Result: protocol.SubChunkResultSuccess, RawPayload: protocol.Option(raw)}}}, 2193, true)
	if bytes.Contains(logs.Bytes(), []byte("level=WARN")) || !bytes.Contains(logs.Bytes(), []byte("blockEntities=1")) {
		t.Fatalf("valid NBT misclassified: %s", logs.String())
	}
}
