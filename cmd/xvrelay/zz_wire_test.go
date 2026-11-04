package main

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestWireShape(t *testing.T) {
	lc := &packet.LevelChunk{
		Position:      protocol.ChunkPos{0, 0},
		Dimension:     0,
		SubChunkCount: 0,
		SubChunkLimit: protocol.Option(int32(9)),
	}
	var b bytes.Buffer
	lc.Marshal(minecraft.DefaultProtocol.NewWriter(&b, 0))
	fmt.Printf("FORK LevelChunk  : %x\n", b.Bytes())

	sc := &packet.SubChunk{
		Dimension: 0,
		Position:  protocol.SubChunkPos{0, 4, 0},
		SubChunkEntries: []protocol.SubChunkEntry{{
			Offset:        protocol.SubChunkOffset{0, 0, 0},
			Result:        protocol.SubChunkResultSuccess,
			RawPayload:    protocol.Option([]byte{0x09, 0x01, 0x04}),
			HeightMapType: protocol.HeightMapDataNone,
		}},
	}
	var b2 bytes.Buffer
	sc.Marshal(minecraft.DefaultProtocol.NewWriter(&b2, 0))
	fmt.Printf("FORK SubChunk    : %x\n", b2.Bytes())
}
