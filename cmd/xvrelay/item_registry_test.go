package main

import (
	"testing"

	"bedrock-cross-version-relay/internal/blocktranslate"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	v1_26_44 "github.com/shawtymarco/go-multiversion/protocols/v1_26_44"
	v1_26_45 "github.com/shawtymarco/go-multiversion/protocols/v1_26_45"
)

// TestClientItemRegistryWiring locks down the caller-side rule: a registry-aware codec must be
// fed the NATIVE table (it translates), a registry-less codec must be fed the client's OWN table
// (it passes through). Getting this backwards drops the whole ItemRegistry packet.
func TestClientItemRegistryWiring(t *testing.T) {
	native, err := blocktranslate.NativeItems()
	if err != nil {
		t.Fatal(err)
	}

	// 1001: the relay's cached adapter is registry-aware, so GameData.Items must be native.
	items1001, source1001, err := clientItemRegistry(1001)
	if err != nil {
		t.Fatal(err)
	}
	if !sameItemIDs(items1001, native) {
		t.Fatalf("client 1001 was handed a %d-entry table instead of the %d native entries (%s)",
			len(items1001), len(native), source1001)
	}
	out := proto1001().ConvertFromLatest(&packet.ItemRegistry{Items: items1001}, nil)
	if len(out) != 1 {
		t.Fatalf("1001 codec dropped the ItemRegistry: got %d packets", len(out))
	}
	reg, ok := out[0].(*packet.ItemRegistry)
	if !ok {
		t.Fatalf("1001 codec returned %T", out[0])
	}
	want1001, _ := blocktranslate.Items(1001)
	if !sameItemIDs(reg.Items, want1001) {
		t.Error("1001 client did not receive the 1001 item table")
	}
	t.Logf("client 1001: GameData.Items = native %d -> codec emits %d (%s)", len(items1001), len(reg.Items), source1001)

	// The inverse: handing the 1001 adapter its OWN table makes ValidateNativeEntries fail and
	// the packet is dropped. This is the regression the old unconditional Items(clientProto) hit.
	dropped := proto1001().ConvertFromLatest(&packet.ItemRegistry{Items: want1001}, nil)
	if len(dropped) != 0 {
		t.Errorf("expected the 1001 codec to DROP a non-native registry, got %d packets", len(dropped))
	} else {
		t.Log("1001 codec correctly drops a non-native ItemRegistry (0 packets)")
	}

	// 2168/2169: the relay uses plain New() codecs, which are pass-through.
	for _, c := range []struct {
		id   int32
		code func() []packet.Packet
	}{
		{2168, func() []packet.Packet {
			items, _, _ := clientItemRegistry(2168)
			return v1_26_44.New().ConvertFromLatest(&packet.ItemRegistry{Items: items}, nil)
		}},
		{2169, func() []packet.Packet {
			items, _, _ := clientItemRegistry(2169)
			return v1_26_45.New().ConvertFromLatest(&packet.ItemRegistry{Items: items}, nil)
		}},
	} {
		items, source, err := clientItemRegistry(c.id)
		if err != nil {
			t.Fatalf("clientItemRegistry(%d): %v", c.id, err)
		}
		want, err := blocktranslate.Items(c.id)
		if err != nil {
			t.Fatal(err)
		}
		if !sameItemIDs(items, want) {
			t.Errorf("client %d was not handed its own table", c.id)
		}
		packets := c.code()
		if len(packets) != 1 {
			t.Fatalf("codec %d returned %d packets", c.id, len(packets))
		}
		got, ok := packets[0].(*packet.ItemRegistry)
		if !ok {
			t.Fatalf("codec %d returned %T", c.id, packets[0])
		}
		if !sameItemIDs(got.Items, want) {
			t.Errorf("client %d did not receive its own item table", c.id)
		}
		t.Logf("client %d: GameData.Items = %d entries -> codec emits %d unchanged (%s)",
			c.id, len(items), len(got.Items), source)
	}

	// 2193 native client: identity.
	items2193, _, err := clientItemRegistry(2193)
	if err != nil || !sameItemIDs(items2193, native) {
		t.Errorf("client 2193 did not receive the native table: err=%v", err)
	}
}

func sameItemIDs(a, b []protocol.ItemEntry) bool {
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
