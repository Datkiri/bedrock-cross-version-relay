package blocktranslate

import (
	"reflect"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
	"github.com/shawtymarco/go-multiversion/protocols/v1_26_44"
	"github.com/shawtymarco/go-multiversion/protocols/v1_26_45"
)

// TestItemRegistries pins the shape of every table the relay can advertise. The counts come from
// the upstream snapshots: Cloudburst 2193 = 2076, dragonfly v1001 = 1933, v2169 = 1976.
func TestItemRegistries(t *testing.T) {
	want := map[int32]int{1001: 1933, 2168: 1976, 2169: 1976, 2193: 2076}
	for id, count := range want {
		entries, err := Items(id)
		if err != nil {
			t.Fatalf("Items(%d): %v", id, err)
		}
		if len(entries) != count {
			t.Errorf("Items(%d) has %d entries, want %d", id, len(entries), count)
		}
		byName, byID := map[string]int16{}, map[int16]string{}
		for _, e := range entries {
			if e.Name == "" {
				t.Fatalf("Items(%d) has an entry with an empty name", id)
			}
			if prev, ok := byName[e.Name]; ok {
				t.Errorf("Items(%d) duplicate name %q (IDs %d and %d)", id, e.Name, prev, e.RuntimeID)
			}
			if prev, ok := byID[e.RuntimeID]; ok {
				t.Errorf("Items(%d) runtime ID %d shared by %q and %q", id, e.RuntimeID, prev, e.Name)
			}
			byName[e.Name], byID[e.RuntimeID] = e.RuntimeID, e.Name
		}
		t.Logf("Items(%d) entries=%d first=%q/%d", id, len(entries), entries[0].Name, entries[0].RuntimeID)
	}

	a, _ := Items(2168)
	b, _ := Items(2169)
	if !reflect.DeepEqual(a, b) {
		t.Error("2168 and 2169 must share the same item table")
	}
	if _, err := Items(1234); err == nil {
		t.Error("Items() must error for an unknown protocol")
	}
}

// convertRegistry runs a codec's ConvertFromLatest on an ItemRegistry and returns the entries the
// client would actually receive. conn=nil is safe for this packet in all four codecs: the
// ItemRegistry branch is handled before any conn dereference, and downgradePacket guards nil.
func convertRegistry(t *testing.T, p minecraft.Protocol, items []protocol.ItemEntry) []protocol.ItemEntry {
	t.Helper()
	out := p.ConvertFromLatest(&packet.ItemRegistry{Items: items}, nil)
	if len(out) != 1 {
		t.Fatalf("ConvertFromLatest returned %d packets, want 1", len(out))
	}
	reg, ok := out[0].(*packet.ItemRegistry)
	if !ok {
		t.Fatalf("ConvertFromLatest returned %T, want *packet.ItemRegistry", out[0])
	}
	return reg.Items
}

// TestItemRegistryMatchesCodec proves Items(id) is exactly the table a REGISTRY-AWARE codec
// emits, and that a plain codec passes GameData.Items through unchanged. That distinction is what
// the relay's GameData.Items selection is based on.
func TestItemRegistryMatchesCodec(t *testing.T) {
	native, err := NativeItems()
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := NativeRegistry(2193)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		id    int32
		build func() (minecraft.Protocol, error)
	}{
		{1001, func() (minecraft.Protocol, error) { return v1_26_30.NewWithRegistries(blocks, native) }},
		{2168, func() (minecraft.Protocol, error) { return v1_26_44.NewWithRegistries(blocks, native) }},
		{2169, func() (minecraft.Protocol, error) { return v1_26_45.NewWithRegistries(blocks, native) }},
	}
	for _, c := range cases {
		p, err := c.build()
		if err != nil {
			t.Fatalf("NewWithRegistries(%d): %v", c.id, err)
		}
		got := convertRegistry(t, p, native)
		want, err := Items(c.id)
		if err != nil {
			t.Fatal(err)
		}
		if !sameRegistry(got, want) {
			t.Errorf("codec %d emitted a registry that differs from Items(%d): got %d entries, want %d",
				c.id, c.id, len(got), len(want))
		} else {
			t.Logf("codec %d -> Items(%d): %d entries, identical name/ID mapping", c.id, c.id, len(got))
		}
	}

	// A plain (registry-less) codec is a pass-through. Feeding it the host's native table would
	// give a 2169 client the wrong IDs, so the relay must hand it Items(2169) instead.
	plain := v1_26_45.New()
	if got := convertRegistry(t, plain, native); !sameRegistry(got, native) {
		t.Error("v1_26_45.New() must pass ItemRegistry through unchanged")
	} else {
		t.Logf("v1_26_45.New() passes through %d native entries unchanged", len(got))
	}
}

func sameRegistry(a, b []protocol.ItemEntry) bool {
	if len(a) != len(b) {
		return false
	}
	ids := make(map[string]int16, len(b))
	for _, e := range b {
		ids[e.Name] = e.RuntimeID
	}
	for _, e := range a {
		if id, ok := ids[e.Name]; !ok || id != e.RuntimeID {
			return false
		}
	}
	return true
}
