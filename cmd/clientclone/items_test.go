package main

import (
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func stack(id int32, count uint16) protocol.ItemInstance {
	return protocol.ItemInstance{
		Stack: protocol.ItemStack{
			ItemType: protocol.ItemType{NetworkID: id},
			Count:    count,
		},
	}
}

func TestItemIndexUsesClientProtocolTable(t *testing.T) {
	// The relay writes TARGET-protocol IDs on the wire, so the client's own
	// table (not native 2193) is the correct lookup. apple is a measured case
	// where 2193 and 2169 disagree (882 vs 878).
	for id, want := range map[int32]int{1001: 1933, 2168: 1976, 2169: 1976, 2193: 2076} {
		idx, err := newItemIndex(id)
		if err != nil {
			t.Fatalf("newItemIndex(%d): %v", id, err)
		}
		if idx.count != want {
			t.Errorf("proto=%d entries=%d, want %d", id, idx.count, want)
		}
		apple, ok := idx.byName["minecraft:apple"]
		if !ok {
			t.Fatalf("proto=%d has no minecraft:apple", id)
		}
		if idx.byID[apple] != "minecraft:apple" {
			t.Errorf("proto=%d apple id=%d does not round-trip", id, apple)
		}
	}
	target, _ := newItemIndex(2169)
	native, _ := newItemIndex(2193)
	if target.byName["minecraft:apple"] == native.byName["minecraft:apple"] {
		t.Fatal("2169 and 2193 apple runtime IDs must differ; the fixture no longer exercises cross-version mapping")
	}
}

func TestStacksFromCoversEveryItemFamily(t *testing.T) {
	cases := []struct {
		name     string
		pk       packet.Packet
		want     int
		creative bool
	}{
		{"InventoryContent", &packet.InventoryContent{
			Content:     []protocol.ItemInstance{stack(1, 1), stack(2, 3)},
			StorageItem: stack(3, 1),
		}, 3, false},
		{"InventorySlot", &packet.InventorySlot{
			NewItem:     stack(4, 1),
			StorageItem: protocol.Option(stack(5, 1)),
		}, 2, false},
		{"MobEquipment", &packet.MobEquipment{NewItem: stack(6, 1)}, 1, false},
		{"AddItemActor", &packet.AddItemActor{Item: stack(7, 1)}, 1, false},
		{"CreativeContent", &packet.CreativeContent{
			Items: []protocol.CreativeItem{{Item: stack(8, 1).Stack}, {Item: stack(9, 1).Stack}},
		}, 2, true},
		{"CraftingData", &packet.CraftingData{
			ShapedRecipes:    []protocol.ShapedRecipe{{Output: []protocol.ItemStack{stack(10, 1).Stack}}},
			ShapelessRecipes: []protocol.ShapelessRecipe{{Output: []protocol.ItemStack{stack(11, 1).Stack}}},
		}, 2, true},
	}
	for _, c := range cases {
		stacks, creative := stacksFrom(c.pk)
		if len(stacks) != c.want {
			t.Errorf("%s: got %d stacks, want %d", c.name, len(stacks), c.want)
		}
		if creative != c.creative {
			t.Errorf("%s: creative=%t, want %t", c.name, creative, c.creative)
		}
		for i, st := range stacks {
			if st.NetworkID == 0 {
				t.Errorf("%s: stack %d has id 0", c.name, i)
			}
		}
	}
	if stacks, _ := stacksFrom(&packet.LevelChunk{}); len(stacks) != 0 {
		t.Fatal("a packet without item stacks must not observe anything")
	}
}

func TestObserveStackClassifiesLiveWireIDs(t *testing.T) {
	s := newSession(config{proto: 1001, itemLog: 0})
	s.itemsIndex = itemIndex{
		byID:   map[int32]string{7: "minecraft:apple", 8: "minecraft:stone"},
		byName: map[string]int32{"minecraft:apple": 7, "minecraft:stone": 8},
		count:  2,
	}
	s.itemsReady.Store(true)

	s.observeStack("CreativeContent", 7, true)   // named item
	s.observeStack("CreativeContent", 0, true)   // became AIR
	s.observeStack("InventoryContent", 0, false) // legitimate empty slot
	s.observeStack("InventorySlot", 9999, false) // unknown ID

	got := s.itemSummary()
	checks := map[string]uint64{
		"items_decoded":     4,
		"items_visible":     1,
		"items_lost":        1,
		"items_empty_slots": 1,
		"items_unknown":     1,
	}
	for k, want := range checks {
		v, ok := got[k].(uint64)
		if !ok {
			t.Fatalf("%s is %T, want uint64", k, got[k])
		}
		if v != want {
			t.Errorf("%s=%d, want %d", k, v, want)
		}
	}
	if got["items_table_size"] != 2 {
		t.Errorf("items_table_size=%v, want 2", got["items_table_size"])
	}
	families, _ := got["items_lost_families"].([]string)
	if len(families) != 1 || families[0] != "CreativeContent" {
		t.Errorf("items_lost_families=%v, want [CreativeContent]", families)
	}
	samples, _ := got["items_visible_samples"].([]string)
	if len(samples) != 1 || samples[0] != "minecraft:apple" {
		t.Errorf("items_visible_samples=%v, want [minecraft:apple]", samples)
	}
}

func TestObserveItemsIsNoopBeforeIndexReady(t *testing.T) {
	s := newSession(config{proto: 1001, itemLog: 0})
	s.observeItems(&packet.CreativeContent{Items: []protocol.CreativeItem{{Item: stack(7, 1).Stack}}})
	if got := s.itemSummary()["items_decoded"].(uint64); got != 0 {
		t.Fatalf("observed %d items before the index was ready", got)
	}
}
