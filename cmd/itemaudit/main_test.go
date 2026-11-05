package main

import (
	"strings"
	"testing"

	"bedrock-cross-version-relay/internal/blocktranslate"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func quietLogger(string, ...any) {}

// TestAuditAllProtocols pins the measured invisible-item set. These numbers are
// derived from the pinned registries, so a change here means either the data
// changed or the codec stopped translating — both need an explicit review.
func TestAuditAllProtocols(t *testing.T) {
	want := map[int32]struct {
		lost, visible   int
		crossCheck      string
		mapperFallbacks int
	}{
		1001: {143, 1933, "OK", 143},
		2168: {100, 1976, "not available", 0},
		2169: {100, 1976, "OK", 100},
		2193: {0, 2076, "not applicable", 0},
	}

	for _, id := range allProtocols {
		t.Run(protocolName(id), func(t *testing.T) {
			r := auditProtocol(id, quietLogger, quietLogger)
			w := want[id]
			if r.Error != "" {
				t.Fatalf("audit error: %s", r.Error)
			}
			if r.PacketDropped {
				t.Fatal("InventoryContent was dropped")
			}
			if r.Sent != r.NativeItems || r.Sent != 2076 {
				t.Fatalf("sent=%d native=%d, want 2076", r.Sent, r.NativeItems)
			}
			if r.Visible+r.Lost != r.Sent {
				t.Errorf("visible=%d lost=%d sent=%d: items unaccounted for", r.Visible, r.Lost, r.Sent)
			}
			if r.Visible != w.visible || r.Lost != w.lost {
				t.Errorf("visible=%d lost=%d, want %d/%d", r.Visible, r.Lost, w.visible, w.lost)
			}
			if r.Lost != r.NativeItems-r.TargetItems {
				t.Errorf("lost=%d but native-target=%d: an item became AIR without a missing target entry",
					r.Lost, r.NativeItems-r.TargetItems)
			}
			if r.WrongID != 0 {
				t.Errorf("wrong_id=%d: a round trip changed a runtime ID", r.WrongID)
			}
			if len(r.TargetOnlyNames) != 0 {
				t.Errorf("target_only=%d: historical registry has names absent from native", len(r.TargetOnlyNames))
			}
			if !strings.Contains(r.CrossCheck, w.crossCheck) {
				t.Errorf("cross_check=%q, want it to contain %q", r.CrossCheck, w.crossCheck)
			}
			if r.MapperFallbacks != w.mapperFallbacks {
				t.Errorf("mapper_fallbacks=%d, want %d", r.MapperFallbacks, w.mapperFallbacks)
			}
			if r.WireBytes <= 0 {
				t.Error("wire_bytes=0: the packet was never encoded")
			}

			// Registry-aware codecs hide unknown IDs as AIR; the identity codec
			// keeps them (they are still inside the int16 wire range).
			for i, p := range r.UnknownProbes {
				wantAir := id != 2193
				if p.BecameAir != wantAir {
					t.Errorf("unknown probe %d: became_air=%v, want %v (observed=%d)",
						i, p.BecameAir, wantAir, p.ObservedNetworkID)
				}
			}
			if r.RangeProbe == nil {
				t.Fatal("range probe missing")
			}
			if r.RangeProbe.Truncated != (id == 2193) {
				t.Errorf("range probe truncated=%v, want %v", r.RangeProbe.Truncated, id == 2193)
			}
		})
	}
}

// TestWireContentUsesProtocolPool pins the trap that made the first version of
// this tool fail on protocol 1001: ConvertFromLatest returns a
// *v1_26_30.translatedPacket whose wire layout differs from the latest one.
// Decoding it into a plain packet.InventoryContent misaligns (30219 unread
// bytes measured); decoding through codec.Packets(false) does not.
func TestWireContentUsesProtocolPool(t *testing.T) {
	native, err := blocktranslate.NativeItems()
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
	stacks := make([]protocol.ItemInstance, n)
	for i := 0; i < n; i++ {
		stacks[i] = itemStack(int32(native[i].RuntimeID))
	}

	for _, id := range allProtocols {
		codec, _, err := codecFor(id)
		if err != nil {
			t.Fatal(err)
		}
		out := codec.ConvertFromLatest(&packet.InventoryContent{Content: stacks}, (*minecraft.Conn)(nil))
		if len(out) != 1 {
			t.Fatalf("proto=%d: ConvertFromLatest returned %d packets", id, len(out))
		}
		if id == 1001 {
			if _, plain := out[0].(*packet.InventoryContent); plain {
				t.Fatal("proto=1001 unexpectedly returned a plain InventoryContent; the wrapper trap is gone — update the audit")
			}
		}
		content, wireBytes, err := wireContent(codec, out[0], n)
		if err != nil {
			t.Fatalf("proto=%d: wire round trip: %v", id, err)
		}
		if len(content) != n || wireBytes <= 0 {
			t.Fatalf("proto=%d: content=%d wire_bytes=%d", id, len(content), wireBytes)
		}
	}
}

func protocolName(id int32) string {
	switch id {
	case 1001:
		return "1001"
	case 2168:
		return "2168"
	case 2169:
		return "2169"
	case 2193:
		return "2193"
	}
	return "unknown"
}
