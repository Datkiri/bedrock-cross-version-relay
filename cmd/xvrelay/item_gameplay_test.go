package main

import (
	"fmt"
	"testing"

	"bedrock-cross-version-relay/internal/blocktranslate"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	v1_26_44 "github.com/shawtymarco/go-multiversion/protocols/v1_26_44"
	v1_26_45 "github.com/shawtymarco/go-multiversion/protocols/v1_26_45"
)

// These tests pin down what enabling NewWithRegistries for 2168/2169 actually
// does to item runtime IDs inside gameplay packets. They are entirely offline:
// no BDS, no listener, no network. Item IDs are NEVER hard-coded — they are
// looked up by NAME in the very registries the codecs consume.

// gameplayItemSample is one identifier present in BOTH the native 2193 table and
// the shared 2168/2169 target table, carrying its two real runtime IDs.
type gameplayItemSample struct {
	name     string
	id2193   int32
	idTarget int32
}

func gameplayLoadTables(t *testing.T) (native, target []protocol.ItemEntry) {
	t.Helper()
	native, err := blocktranslate.NativeItems()
	if err != nil {
		t.Fatalf("NativeItems(): %v", err)
	}
	target, err = blocktranslate.Items(2169)
	if err != nil {
		t.Fatalf("Items(2169): %v", err)
	}
	return native, target
}

func gameplayIDByName(entries []protocol.ItemEntry) map[string]int32 {
	out := make(map[string]int32, len(entries))
	for _, e := range entries {
		if _, exists := out[e.Name]; !exists {
			out[e.Name] = int32(e.RuntimeID)
		}
	}
	return out
}

// gameplaySamples picks `want` REAL items present in both tables by NAME. It
// strongly PREFERS items whose 2193 and 2169 runtime IDs DIFFER: only such an
// item can prove a genuine translation, because an item whose ID is identical
// in both tables would also survive an unmodified pass-through codec. Preferred
// names are only hints and IDs are never hard-coded. When both kinds exist the
// result contains at least one differing-ID sample AND one same-ID sample.
func gameplaySamples(t *testing.T, native, target []protocol.ItemEntry, want int) []gameplayItemSample {
	t.Helper()
	n := gameplayIDByName(native)
	g := gameplayIDByName(target)
	preferred := []string{
		"minecraft:stone", "minecraft:dirt", "minecraft:diamond_pickaxe",
		"minecraft:cobblestone", "minecraft:oak_planks", "minecraft:iron_ingot",
		"minecraft:apple", "minecraft:stick", "minecraft:coal",
	}
	var divergent, stable []gameplayItemSample
	seen := make(map[string]bool)
	add := func(name string, a, b int32) {
		if seen[name] {
			return
		}
		seen[name] = true
		s := gameplayItemSample{name, a, b}
		if a != b {
			divergent = append(divergent, s)
		} else {
			stable = append(stable, s)
		}
	}
	for _, name := range preferred {
		a, ok1 := n[name]
		b, ok2 := g[name]
		if ok1 && ok2 {
			add(name, a, b)
		}
	}
	for _, e := range native {
		if b, ok := g[e.Name]; ok {
			add(e.Name, int32(e.RuntimeID), b)
		}
	}
	t.Logf("items common by NAME: %d with DIFFERENT 2193/2169 IDs, %d with identical IDs (total %d)",
		len(divergent), len(stable), len(divergent)+len(stable))

	// Take differing-ID items first (these prove mapping), reserving one slot
	// for a same-ID item so the identity case is documented too.
	limit := want
	if len(stable) > 0 && want > 1 {
		limit = want - 1
	}
	out := append([]gameplayItemSample{}, divergent[:min(limit, len(divergent))]...)
	if len(stable) > 0 {
		out = append(out, stable[0])
	}
	used := make(map[string]bool)
	for _, s := range out {
		used[s.name] = true
	}
	for _, s := range append(append([]gameplayItemSample{}, divergent...), stable...) {
		if len(out) >= want {
			break
		}
		if !used[s.name] {
			out = append(out, s)
			used[s.name] = true
		}
	}
	if len(out) < want {
		t.Fatalf("only %d item names are common to the 2193 and 2169 registries; need %d", len(out), want)
	}
	return out
}

func gameplayRegistryCodec(id int32, native []protocol.ItemEntry) (minecraft.Protocol, error) {
	reg, err := blocktranslate.NativeRegistry(2193)
	if err != nil {
		return nil, err
	}
	switch id {
	case 2168:
		return v1_26_44.NewWithRegistries(reg, native)
	case 2169:
		return v1_26_45.NewWithRegistries(reg, native)
	}
	return nil, fmt.Errorf("no registry-aware adapter for protocol %d", id)
}

func gameplayPlainCodec(id int32) minecraft.Protocol {
	if id == 2168 {
		return v1_26_44.New()
	}
	return v1_26_45.New()
}

func gameplayInventory(ids ...int32) *packet.InventoryContent {
	content := make([]protocol.ItemInstance, len(ids))
	for i, id := range ids {
		content[i] = protocol.ItemInstance{
			Stack: protocol.ItemStack{
				ItemType: protocol.ItemType{NetworkID: id},
				Count:    1,
			},
		}
	}
	return &packet.InventoryContent{Content: content}
}

func gameplayContentOf(t *testing.T, out []packet.Packet, label string) *packet.InventoryContent {
	t.Helper()
	if len(out) != 1 {
		t.Fatalf("%s: got %d packets, want exactly 1", label, len(out))
	}
	ic, ok := out[0].(*packet.InventoryContent)
	if !ok {
		t.Fatalf("%s: got %T, want *packet.InventoryContent", label, out[0])
	}
	return ic
}

// TestItemGameplayMappingDirection: 2193 (native) ID -> the SAME NAME's ID in
// Items(2169). A plain New() codec must keep the native ID untouched.
func TestItemGameplayMappingDirection(t *testing.T) {
	native, target := gameplayLoadTables(t)
	samples := gameplaySamples(t, native, target, 3)
	t.Logf("registries: 2193 native=%d entries, 2169 target=%d entries", len(native), len(target))

	for _, id := range []int32{2168, 2169} {
		p, err := gameplayRegistryCodec(id, native)
		if err != nil {
			t.Fatalf("NewWithRegistries(%d): %v", id, err)
		}
		plain := gameplayPlainCodec(id)
		for _, s := range samples {
			mapped := gameplayContentOf(t,
				p.ConvertFromLatest(gameplayInventory(s.id2193), (*minecraft.Conn)(nil)),
				fmt.Sprintf("proto %d %s ConvertFromLatest", id, s.name))
			got := mapped.Content[0].Stack.NetworkID

			if got != s.idTarget {
				t.Errorf("proto %d %s: ConvertFromLatest mapped 2193 id=%d to %d, want the Items(2169) id of the same name %d",
					id, s.name, s.id2193, got, s.idTarget)
			}
			if got == 0 {
				t.Errorf("proto %d %s: item was silently replaced by AIR (NetworkID=0)", id, s.name)
			}
			if got == s.id2193 && s.id2193 != s.idTarget {
				t.Errorf("proto %d %s: ID was NOT translated at all (still %d)", id, s.name, got)
			}

			control := gameplayContentOf(t,
				plain.ConvertFromLatest(gameplayInventory(s.id2193), (*minecraft.Conn)(nil)),
				fmt.Sprintf("proto %d %s plain New() control", id, s.name))
			plainID := control.Content[0].Stack.NetworkID
			if plainID != s.id2193 {
				t.Errorf("proto %d plain New() changed %s id %d -> %d; expected pass-through", id, s.name, s.id2193, plainID)
			}
			t.Logf("proto %d %-32s 2193 id=%-5d -> mapped id=%-5d (want %-5d) | plain New() keeps %d",
				id, s.name, s.id2193, got, s.idTarget, plainID)
		}
	}
}

// TestItemGameplayReverseDirection: the 2169 ID -> 2193 ID path
// (ConvertToLatest, i.e. C->H packets read by the relay). If the adapter does
// not map this direction, that is a REAL finding and must be reported loudly,
// never skipped in silence.
func TestItemGameplayReverseDirection(t *testing.T) {
	native, target := gameplayLoadTables(t)
	samples := gameplaySamples(t, native, target, 3)

	for _, id := range []int32{2168, 2169} {
		p, err := gameplayRegistryCodec(id, native)
		if err != nil {
			t.Fatalf("NewWithRegistries(%d): %v", id, err)
		}
		ids := make([]int32, len(samples))
		for i, s := range samples {
			ids[i] = s.idTarget
		}
		out := p.ConvertToLatest(gameplayInventory(ids...), (*minecraft.Conn)(nil))
		if len(out) != 1 {
			t.Errorf("proto %d ConvertToLatest(InventoryContent) returned %d packets (want 1): the adapter does NOT map the reverse/target->native direction, so item IDs arriving from a %d client are not translated back to 2193",
				id, len(out), id)
			continue
		}
		ic, ok := out[0].(*packet.InventoryContent)
		if !ok {
			t.Errorf("proto %d ConvertToLatest returned %T, want *packet.InventoryContent", id, out[0])
			continue
		}
		for i, s := range samples {
			got := ic.Content[i].Stack.NetworkID
			if got != s.id2193 {
				t.Errorf("proto %d ConvertToLatest did NOT map %s: target id=%d -> got %d, want native 2193 id=%d. This means the C->H item path is NOT covered by NewWithRegistries and would need a separate mapper",
					id, s.name, s.idTarget, got, s.id2193)
			}
			t.Logf("proto %d %-32s 2169 id=%-5d -> 2193 id=%-5d (want %-5d) reverse %s",
				id, s.name, s.idTarget, got, s.id2193, map[bool]string{true: "OK", false: "MISMATCH"}[got == s.id2193])
		}
	}
}

// TestItemGameplayNoDropFamilies: the six canonical 2193 item-carrying packet
// families must not be silently dropped by ConvertFromLatest.
func TestItemGameplayNoDropFamilies(t *testing.T) {
	native, target := gameplayLoadTables(t)
	samples := gameplaySamples(t, native, target, 3)
	stack := protocol.ItemInstance{
		Stack: protocol.ItemStack{ItemType: protocol.ItemType{NetworkID: samples[0].id2193}, Count: 1},
	}

	families := []struct {
		name string
		pk   packet.Packet
	}{
		{"ItemRegistry", &packet.ItemRegistry{Items: native}},
		{"InventoryContent", gameplayInventory(samples[0].id2193)},
		{"InventorySlot", &packet.InventorySlot{NewItem: stack}},
		{"MobEquipment", &packet.MobEquipment{NewItem: stack}},
		{"CraftingData", &packet.CraftingData{}},
		{"CreativeContent", &packet.CreativeContent{}},
	}

	for _, id := range []int32{2168, 2169} {
		p, err := gameplayRegistryCodec(id, native)
		if err != nil {
			t.Fatalf("NewWithRegistries(%d): %v", id, err)
		}
		for _, f := range families {
			out := p.ConvertFromLatest(f.pk, (*minecraft.Conn)(nil))
			if len(out) < 1 {
				t.Errorf("proto %d SILENTLY DROPPED %s (out=0)", id, f.name)
				continue
			}
			t.Logf("proto %d %-18s out=%d -> %T", id, f.name, len(out), out[0])
			if reg, ok := out[0].(*packet.ItemRegistry); ok {
				if len(out) != 1 {
					t.Errorf("proto %d ItemRegistry out=%d, want 1", id, len(out))
				}
				if len(reg.Items) != 1976 {
					t.Errorf("proto %d ItemRegistry emitted %d entries, want 1976", id, len(reg.Items))
				}
				if len(reg.Items) != len(target) {
					t.Errorf("proto %d ItemRegistry emitted %d entries, Items(2169) has %d", id, len(reg.Items), len(target))
				}
				t.Logf("proto %d ItemRegistry: native %d entries -> %d entries (Items(2169)=%d)",
					id, len(native), len(reg.Items), len(target))
			}
		}

		// Unknown runtime ID: must NOT drop the packet, but mapItemInstances uses
		// hideUnknown=true on the clientbound path, so the item is replaced by AIR.
		const unknownID int32 = 999999
		out := p.ConvertFromLatest(gameplayInventory(unknownID), (*minecraft.Conn)(nil))
		if len(out) < 1 {
			t.Errorf("proto %d DROPPED InventoryContent with unknown NetworkID=%d", id, unknownID)
		} else if ic, ok := out[0].(*packet.InventoryContent); !ok {
			t.Errorf("proto %d unknown-ID case returned %T", id, out[0])
		} else {
			got := ic.Content[0].Stack.NetworkID
			if got == 0 {
				t.Logf("proto %d unknown NetworkID=%d: packet KEPT (out=%d) but item replaced by AIR (NetworkID=0) — hideUnknown=true in mapItemInstances", id, unknownID, len(out))
			} else {
				t.Logf("proto %d unknown NetworkID=%d: packet KEPT (out=%d), item survived as NetworkID=%d", id, unknownID, len(out), got)
			}
		}
	}
}

// TestItemRegistryAwareInputRule: the "feed NATIVE 2193, not the client's own
// table" rule must still hold for 2168/2169, exactly as it does for 1001.
func TestItemRegistryAwareInputRule(t *testing.T) {
	native, target := gameplayLoadTables(t)
	for _, id := range []int32{2168, 2169} {
		p, err := gameplayRegistryCodec(id, native)
		if err != nil {
			t.Fatalf("NewWithRegistries(%d): %v", id, err)
		}

		// Non-native input (the client's own 2169 table) must be DROPPED.
		dropped := p.ConvertFromLatest(&packet.ItemRegistry{Items: target}, (*minecraft.Conn)(nil))
		if len(dropped) != 0 {
			t.Errorf("proto %d accepted a NON-native ItemRegistry (%d entries) and returned %d packets; ValidateNativeEntries should have failed and dropped it",
				id, len(target), len(dropped))
		} else {
			t.Logf("proto %d: non-native ItemRegistry (%d entries) DROPPED (0 packets) — rule holds", id, len(target))
		}

		// Native input must translate to the target table, not pass through.
		out := p.ConvertFromLatest(&packet.ItemRegistry{Items: native}, (*minecraft.Conn)(nil))
		if len(out) != 1 {
			t.Fatalf("proto %d native ItemRegistry out=%d, want 1", id, len(out))
		}
		reg, ok := out[0].(*packet.ItemRegistry)
		if !ok {
			t.Fatalf("proto %d returned %T", id, out[0])
		}
		if len(reg.Items) != 1976 {
			t.Errorf("proto %d native ItemRegistry emitted %d entries, want 1976", id, len(reg.Items))
		}
		if !sameItemIDs(reg.Items, target) {
			t.Errorf("proto %d emitted ItemRegistry does not match Items(2169)", id)
		}
		t.Logf("proto %d: native %d entries -> emitted %d entries == Items(2169) (%d)", id, len(native), len(reg.Items), len(target))
	}
}

// TestItemGameplayRelayCodecIsRegistryAware exercises the REAL relay codec, not
// a locally built one: resolveProto(2168/2169) must return the registry-aware
// adapter that main.go actually installs.
func TestItemGameplayRelayCodecIsRegistryAware(t *testing.T) {
	native, target := gameplayLoadTables(t)
	for _, id := range []int32{2168, 2169} {
		p, ok := resolveProto(id)
		if !ok {
			t.Fatalf("resolveProto(%d) returned no codec", id)
		}
		if p.ID() != id {
			t.Errorf("resolveProto(%d).ID() = %d", id, p.ID())
		}
		if !adapterHasRegistry(id) {
			t.Errorf("resolveProto(%d) is NOT registry-aware: mapGameplayItems=%v, adapterHasRegistry(%d)=false. A plain New() codec would leave 2193 item IDs in gameplay packets",
				id, mapGameplayItems, id)
			continue
		}

		out := p.ConvertFromLatest(&packet.ItemRegistry{Items: native}, (*minecraft.Conn)(nil))
		if len(out) != 1 {
			t.Errorf("relay codec %d native ItemRegistry out=%d, want 1", id, len(out))
			continue
		}
		reg, ok := out[0].(*packet.ItemRegistry)
		if !ok {
			t.Errorf("relay codec %d returned %T", id, out[0])
			continue
		}
		if len(reg.Items) != 1976 {
			t.Errorf("relay codec %d emitted %d entries, want 1976", id, len(reg.Items))
		}
		if !sameItemIDs(reg.Items, target) {
			t.Errorf("relay codec %d emitted ItemRegistry does not match Items(2169)", id)
		}

		// Only a registry-aware codec drops a non-native registry.
		dropped := p.ConvertFromLatest(&packet.ItemRegistry{Items: target}, (*minecraft.Conn)(nil))
		if len(dropped) != 0 {
			t.Errorf("relay codec %d did NOT drop a non-native ItemRegistry (%d packets) — it is a plain pass-through, not registry-aware",
				id, len(dropped))
		}
		t.Logf("resolveProto(%d) -> %T registry-aware: native %d -> %d entries; non-native %d entries dropped (%d packets)",
			id, p, len(native), len(reg.Items), len(target), len(dropped))
	}
}
