package main

import (
	"testing"

	"bedrock-cross-version-relay/internal/blocktranslate"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
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

	// 2168/2169: with -map-gameplay-items (default) the relay now uses
	// NewWithRegistries, so they behave exactly like 1001: native 2193 input,
	// translated output, and a non-native registry is DROPPED. With the flag off
	// they are plain New() pass-throughs and need their OWN table.
	for _, c := range []struct {
		id   int32
		code func([]protocol.ItemEntry) []packet.Packet
	}{
		{2168, func(items []protocol.ItemEntry) []packet.Packet {
			return proto2168().ConvertFromLatest(&packet.ItemRegistry{Items: items}, nil)
		}},
		{2169, func(items []protocol.ItemEntry) []packet.Packet {
			return proto2169().ConvertFromLatest(&packet.ItemRegistry{Items: items}, nil)
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
		if adapterHasRegistry(c.id) {
			if !sameItemIDs(items, native) {
				t.Errorf("client %d was handed a %d-entry table instead of the %d native entries", c.id, len(items), len(native))
			}
			if dropped := c.code(want); len(dropped) != 0 {
				t.Errorf("codec %d did NOT drop a non-native ItemRegistry: got %d packets", c.id, len(dropped))
			} else {
				t.Logf("codec %d correctly drops a non-native ItemRegistry (0 packets)", c.id)
			}
		} else if !sameItemIDs(items, want) {
			t.Errorf("client %d was not handed its own table", c.id)
		}
		packets := c.code(items)
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
		t.Logf("client %d: GameData.Items = %d entries -> codec emits %d (%s)",
			c.id, len(items), len(got.Items), source)
	}

	// 2193 native client: identity.
	items2193, _, err := clientItemRegistry(2193)
	if err != nil || !sameItemIDs(items2193, native) {
		t.Errorf("client 2193 did not receive the native table: err=%v", err)
	}
}

// TestSameWireGameDataItemsNeverDropped is the regression for the 2026-11-05
// same-wire bug: a 1001 client into a 1001 host used to keep the host's own
// 1933-entry table, which the registry-aware 1001 codec silently DROPPED. The
// client then received no ItemRegistry at all and never finished spawning.
// gameDataItemsFor() must replace gd.Items for EVERY session, not only for
// cross-version ones.
func TestSameWireGameDataItemsNeverDropped(t *testing.T) {
	native, err := blocktranslate.NativeItems()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int32{1001, 2168, 2169, 2193} {
		hostItems, err := blocktranslate.Items(id)
		if err != nil {
			t.Fatalf("Items(%d): %v", id, err)
		}
		codec, ok := resolveProto(id)
		if !ok {
			t.Fatalf("resolveProto(%d) returned no codec", id)
		}

		items, source, replaced := gameDataItemsFor(id, hostItems, false)
		if !replaced {
			t.Fatalf("same-wire %d: gameDataItemsFor did NOT replace the host table (%s)", id, source)
		}
		out := codec.ConvertFromLatest(&packet.ItemRegistry{Items: items}, nil)
		if len(out) != 1 {
			t.Errorf("same-wire %d: ItemRegistry DROPPED (%d packets) — the client would never get an item table", id, len(out))
			continue
		}
		reg, ok := out[0].(*packet.ItemRegistry)
		if !ok {
			t.Errorf("same-wire %d: codec returned %T", id, out[0])
			continue
		}
		if !sameItemIDs(reg.Items, hostItems) {
			t.Errorf("same-wire %d: client received the wrong item table", id)
		}
		t.Logf("same-wire %d: host table %d entries -> replaced with %d -> codec emits %d (%s)",
			id, len(hostItems), len(items), len(reg.Items), source)

		// Prove the bug was real: feeding the codec the HOST's table directly is
		// fatal for registry-aware adapters. Keeping that table (the old same-wire
		// behaviour, or -keepitems) is only safe for pass-through codecs.
		if adapterHasRegistry(id) {
			if dropped := codec.ConvertFromLatest(&packet.ItemRegistry{Items: hostItems}, nil); len(dropped) != 0 {
				t.Errorf("same-wire %d: expected the host table to be dropped, got %d packets", id, len(dropped))
			}
		}
		// -keepitems must bypass the replacement.
		if kept, _, rep := gameDataItemsFor(id, hostItems, true); rep || !sameItemIDs(kept, hostItems) {
			t.Errorf("same-wire %d: -keepitems did not keep the host table (replaced=%v)", id, rep)
		}
		// The native table must be the input for every registry-aware adapter.
		if adapterHasRegistry(id) && !sameItemIDs(items, native) {
			t.Errorf("same-wire %d: registry-aware codec was not handed the %d native entries", id, len(native))
		}
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
