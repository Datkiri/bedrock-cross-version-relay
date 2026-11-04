package main

import (
	"log/slog"

	"bedrock-cross-version-relay/internal/blocktranslate"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// resolveProto deliberately uses wire-only New() adapters. If those adapters
// gain registry-aware conversion, keep block IDs mapped in exactly one layer.
func configureBlockTranslation(s *session, clientProtocol int32, gd minecraft.GameData) error {
	if s.hostProto == clientProtocol {
		return nil
	}
	m, err := blocktranslate.New(s.hostProto, clientProtocol, gd.UseBlockNetworkIDHashes, gd.CustomBlocks)
	if err != nil {
		return err
	}
	s.blocks = m
	if adapter1001HasRegistry && s.hostProto == 1001 {
		// The 1001 adapter already maps blocks for the C→H direction
		// (PlayerAuthInput/InventoryTransaction). Building a reverse mapper here would double-map.
		slog.Info("xvrelay: block palette translation enabled (one-way)",
			"hostProtocol", s.hostProto, "clientProtocol", clientProtocol,
			"hashIDs", gd.UseBlockNetworkIDHashes, "sourceStates", m.SourceCount(),
			"placeholderStates", m.FallbackCount(), "skippedCustom", m.SkippedCustom(),
			"placeholder", "minecraft:info_update", "reverse", "adapter1001")
		return nil
	}
	reverse, err := blocktranslate.New(clientProtocol, s.hostProto, gd.UseBlockNetworkIDHashes, gd.CustomBlocks)
	if err != nil {
		return err
	}
	s.reverseBlocks = reverse
	slog.Info("xvrelay: block palette translation enabled", "hostProtocol", s.hostProto, "clientProtocol", clientProtocol,
		"hashIDs", gd.UseBlockNetworkIDHashes, "sourceStates", m.SourceCount(), "placeholderStates", m.FallbackCount(), "placeholder", "minecraft:info_update")
	return nil
}

// adapterMapsBlocks reports whether the 1001 adapter already maps blocks for this packet.
// It does NOT map chunk payloads (header only), so we still have to translate those.
func adapterMapsBlocks(clientProto int32, pk packet.Packet) bool {
	if !adapter1001HasRegistry || clientProto != 1001 {
		return false
	}
	switch pk.(type) {
	case *packet.LevelChunk, *packet.SubChunk:
		return false
	}
	return true
}

func translateSessionPacket(s *session, pk packet.Packet) (packet.Packet, error) {
	if s == nil || s.blocks == nil {
		return pk, nil
	}
	return s.blocks.Translate(pk)
}
