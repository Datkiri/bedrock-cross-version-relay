package main

import (
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
