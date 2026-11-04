package main

import (
	"testing"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// TestVerifyRoundTripSanity — the oracle must NOT raise a false alarm on a valid packet.
// If this test is red, every conclusion of the form "the client disconnected because of packet X"
// is meaningless.
func TestVerifyRoundTripSanity(t *testing.T) {
	proto := minecraft.DefaultProtocol

	cases := []packet.Packet{
		&packet.LevelChunk{
			Position:      protocol.ChunkPos{3, -7},
			Dimension:     0,
			SubChunkCount: 0,
			SubChunkLimit: protocol.Option(int32(9)),
			RawPayload:    []byte{},
		},
		&packet.NetworkChunkPublisherUpdate{
			Position: protocol.BlockPos{0, 64, 0},
			Radius:   64,
		},
		&packet.SubChunk{
			CacheEnabled: false,
			Dimension:    0,
			Position:     protocol.SubChunkPos{0, 4, 0},
			SubChunkEntries: []protocol.SubChunkEntry{{
				Offset:        protocol.SubChunkOffset{0, 0, 0},
				Result:        protocol.SubChunkResultSuccessAllAir,
				HeightMapType: protocol.HeightMapDataNone,
			}},
		},
		&packet.SetTime{Time: 6000},
		&packet.Text{TextType: packet.TextTypeRaw, Message: "xvrelay"},
	}
	for _, pk := range cases {
		if err := verifyRoundTrip(pk, proto); err != nil {
			t.Errorf("%T does not round-trip: %v", pk, err)
		}
	}
}
