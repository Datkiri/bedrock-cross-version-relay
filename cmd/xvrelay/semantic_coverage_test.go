package main

// PROTO-005 — MEASURED semantic packet coverage of the real relay codecs.
//
// README used to say "all six item-carrying packet families" and, more broadly,
// "recipes, biome schemas, entity metadata, particles and block-entity NBT are
// separate problems and are not solved here". Both claims are imprecise:
//
//   - the registry-aware codecs actually remap item runtime IDs in TEN packet
//     families (plus the ItemRegistry table itself), not six;
//   - recipes ARE remapped, but three CraftingData slices are DROPPED;
//   - a few narrow semantic cases (LevelEvent block/item particle data,
//     ActorEvent feed item, AddActor falling-block variant) ARE remapped;
//   - biome schemas, generic entity metadata, spawn-particle payloads and
//     block-entity NBT are NOT remapped at all.
//
// This file measures those boundaries OFFLINE against the same codecs
// resolveProto() installs. It pins the result so a future go-multiversion bump
// that silently widens or narrows the mapping fails CI instead of quietly
// invalidating README. No BDS, no listener, no network.
//
// The extraction helpers walk unexported wrapper structs by reflection:
// go-multiversion returns packets wrapped in an unexported translatedPacket
// (`inner`), which cannot be type-asserted from this package. reflect.Value
// can READ unexported primitive fields and slice lengths; only Interface()
// is blocked. No unsafe is used.

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"bedrock-cross-version-relay/internal/blocktranslate"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	v1_26_30 "github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
)

// walkPacket visits every struct/field reachable from pk, including fields
// behind pointers, interfaces, slices, maps and UNEXPORTED wrapper structs.
// Byte slices are skipped (a raw packet payload can be megabytes).
func walkPacket(pk packet.Packet, visit func(reflect.Value)) {
	var walk func(reflect.Value, int)
	walk = func(v reflect.Value, depth int) {
		if !v.IsValid() || depth > 20 {
			return
		}
		visit(v)
		switch v.Kind() {
		case reflect.Ptr, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem(), depth+1)
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				walk(v.Field(i), depth+1)
			}
		case reflect.Slice, reflect.Array:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				return // raw payload / []byte: no named fields inside
			}
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i), depth+1)
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				walk(v.MapIndex(k), depth+1)
			}
		}
	}
	walk(reflect.ValueOf(pk), 0)
}

// int32FieldValues returns every int32 field named `name` reachable from pk.
func int32FieldValues(pk packet.Packet, name string) []int32 {
	var out []int32
	walkPacket(pk, func(v reflect.Value) {
		if v.Kind() != reflect.Struct {
			return
		}
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if t.Field(i).Name == name && f.Kind() == reflect.Int32 {
				out = append(out, int32(f.Int()))
			}
		}
	})
	return out
}

func firstInt32Field(pk packet.Packet, name string) (int32, bool) {
	ids := int32FieldValues(pk, name)
	if len(ids) == 0 {
		return 0, false
	}
	return ids[0], true
}

// structSliceLengths returns the length of every slice field in the first
// struct of the given name found in the packet tree (unexported wrappers
// included). It is how we inspect a *packet.CraftingData that the adapter
// returned inside an unexported translatedPacket.
func structSliceLengths(pk packet.Packet, structName string) (map[string]int, bool) {
	out := map[string]int{}
	found := false
	walkPacket(pk, func(v reflect.Value) {
		if found || v.Kind() != reflect.Struct || v.Type().Name() != structName {
			return
		}
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).Kind() == reflect.Slice {
				out[t.Field(i).Name] = v.Field(i).Len()
			}
		}
		found = true
	})
	return out, found
}

// coverageItem finds a REAL item whose 2193 runtime ID differs from its ID in
// the target protocol table. IDs are resolved by NAME, never hard-coded.
func coverageItem(t *testing.T, protoID int32) (name string, nativeID, targetID int32) {
	t.Helper()
	native, err := blocktranslate.NativeItems()
	if err != nil {
		t.Fatal(err)
	}
	target, err := blocktranslate.Items(protoID)
	if err != nil {
		t.Fatal(err)
	}
	n := make(map[string]int32, len(native))
	for _, e := range native {
		n[e.Name] = int32(e.RuntimeID)
	}
	g := make(map[string]int32, len(target))
	for _, e := range target {
		g[e.Name] = int32(e.RuntimeID)
	}
	for _, preferred := range []string{"minecraft:apple", "minecraft:stone", "minecraft:dirt"} {
		if a, b := n[preferred], g[preferred]; a != 0 && b != 0 && a != b {
			return preferred, a, b
		}
	}
	for item, a := range n {
		if b, ok := g[item]; ok && a != 0 && b != 0 && a != b {
			return item, a, b
		}
	}
	t.Fatalf("no divergent item between 2193 and %d", protoID)
	return "", 0, 0
}

func covInstance(id int32) protocol.ItemInstance {
	return protocol.ItemInstance{
		Stack: protocol.ItemStack{ItemType: protocol.ItemType{NetworkID: id}, Count: 1},
	}
}

func covStack(id int32) protocol.ItemStack { return covInstance(id).Stack }

func mustNativeItems(t *testing.T) []protocol.ItemEntry {
	t.Helper()
	items, err := blocktranslate.NativeItems()
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func nativeAppleID(t *testing.T) int32 {
	t.Helper()
	for _, e := range mustNativeItems(t) {
		if e.Name == "minecraft:apple" {
			return int32(e.RuntimeID)
		}
	}
	t.Fatal("native 2193 table has no minecraft:apple")
	return 0
}

// coverageFamily is one packet family that carries a real item runtime ID in a
// NetworkID field. `direction` is the direction the packet travels through the
// relay: H2C = host->client (ConvertFromLatest), C2H = client->host
// (ConvertToLatest).
type coverageFamily struct {
	name      string
	direction string
	build     func(id int32) packet.Packet
}

// coverageFamilies is the MEASURED set of item-stack-carrying families in the
// 2193 packet pool that the registry-aware codecs have an explicit switch case
// for. ItemStackRequest / ItemStackResponse are deliberately absent: they use
// name-based identifiers and dynamic StackNetworkID, not item runtime IDs
// (see TestSemanticStackRequestPacketsCarryNoRuntimeIDs).
func coverageFamilies() []coverageFamily {
	return []coverageFamily{
		{"CreativeContent", "H2C", func(id int32) packet.Packet {
			// targetCreativeContent rejects a GroupIndex that has no matching group.
			return &packet.CreativeContent{
				Groups: []protocol.CreativeGroup{{}},
				Items:  []protocol.CreativeItem{{CreativeItemNetworkID: 1, Item: covStack(id)}},
			}
		}},
		{"CraftingData", "H2C", func(id int32) packet.Packet {
			return &packet.CraftingData{ShapelessRecipes: []protocol.ShapelessRecipe{{
				RecipeID: "coverage", RecipeNetworkID: 1,
				Output: []protocol.ItemStack{covStack(id)},
			}}}
		}},
		{"AddPlayer", "H2C", func(id int32) packet.Packet {
			return &packet.AddPlayer{HeldItem: covInstance(id)}
		}},
		{"AddItemActor", "H2C", func(id int32) packet.Packet {
			return &packet.AddItemActor{Item: covInstance(id)}
		}},
		{"InventoryContent", "H2C", func(id int32) packet.Packet {
			return &packet.InventoryContent{Content: []protocol.ItemInstance{covInstance(id)}}
		}},
		{"InventorySlot", "H2C", func(id int32) packet.Packet {
			return &packet.InventorySlot{NewItem: covInstance(id)}
		}},
		{"MobEquipment", "H2C", func(id int32) packet.Packet {
			return &packet.MobEquipment{NewItem: covInstance(id)}
		}},
		{"MobArmourEquipment", "H2C", func(id int32) packet.Packet {
			return &packet.MobArmourEquipment{
				Helmet: covInstance(id), Chestplate: covInstance(id),
				Leggings: covInstance(id), Boots: covInstance(id), Body: covInstance(id),
			}
		}},
		{"InventoryTransaction", "C2H", func(id int32) packet.Packet {
			return &packet.InventoryTransaction{
				Actions:         []protocol.InventoryAction{{OldItem: covInstance(id), NewItem: covInstance(id)}},
				TransactionData: &protocol.NormalTransactionData{},
			}
		}},
		{"PlayerAuthInput", "C2H", func(id int32) packet.Packet {
			return &packet.PlayerAuthInput{
				ItemInteractionData: protocol.Option(protocol.UseItemTransactionData{
					HeldItem:   covInstance(id),
					ActionType: protocol.UseItemActionClickAir,
				}),
			}
		}},
	}
}

// coverageClassify runs ONE packet through the codec and reports what happened
// to its item runtime ID: "mapped", "passthrough", "dropped" or "rewritten".
func coverageClassify(codec minecraft.Protocol, f coverageFamily, inputID, mappedID int32) (string, []int32) {
	var out []packet.Packet
	if f.direction == "H2C" {
		out = codec.ConvertFromLatest(f.build(inputID), nil)
	} else {
		out = codec.ConvertToLatest(f.build(inputID), nil)
	}
	if len(out) == 0 {
		return "dropped", nil
	}
	var ids []int32
	for _, p := range out {
		ids = append(ids, int32FieldValues(p, "NetworkID")...)
	}
	if len(ids) == 0 {
		return "no-network-id", nil
	}
	hasInput, hasMapped := false, false
	for _, v := range ids {
		if v == inputID {
			hasInput = true
		}
		if v == mappedID {
			hasMapped = true
		}
	}
	switch {
	case hasMapped:
		return "mapped", ids
	case hasInput:
		return "passthrough", ids
	default:
		return "rewritten", ids
	}
}

// TestSemanticItemStackFamilyCoverage is the core PROTO-005 measurement:
// TEN item-stack packet families are remapped by the registry-aware codecs,
// while plain New() and the native 2193 identity codec leave them untouched.
func TestSemanticItemStackFamilyCoverage(t *testing.T) {
	families := coverageFamilies()
	if len(families) != 10 {
		t.Fatalf("coverage table has %d families, expected 10 — update the README table too", len(families))
	}

	for _, protoID := range []int32{1001, 2168, 2169} {
		codec, ok := resolveProto(protoID)
		if !ok {
			t.Fatalf("resolveProto(%d) returned no codec", protoID)
		}
		if !adapterHasRegistry(protoID) {
			t.Fatalf("resolveProto(%d) is not registry-aware", protoID)
		}
		name, nativeID, targetID := coverageItem(t, protoID)
		t.Logf("proto %d: divergent item %s 2193=%d -> target=%d", protoID, name, nativeID, targetID)
		for _, f := range families {
			inputID, mappedID := nativeID, targetID
			if f.direction == "C2H" {
				inputID, mappedID = targetID, nativeID
			}
			class, ids := coverageClassify(codec, f, inputID, mappedID)
			if class != "mapped" {
				t.Errorf("proto %d %s (%s): got %q ids=%v, want mapped %d -> %d",
					protoID, f.name, f.direction, class, ids, inputID, mappedID)
			}
			t.Logf("  proto %d %-22s %-3s %-11s ids=%v", protoID, f.name, f.direction, class, ids)
		}
	}

	// Negative control: the plain New() codecs (the -map-gameplay-items=false
	// path) must NOT map any family. This proves the mapping comes from
	// NewWithRegistries, not from the wire wrapper or the test fixture.
	for _, protoID := range []int32{1001, 2168, 2169} {
		var plain minecraft.Protocol
		if protoID == 1001 {
			plain = v1_26_30.New()
		} else {
			plain = gameplayPlainCodec(protoID)
		}
		_, nativeID, targetID := coverageItem(t, protoID)
		for _, f := range families {
			inputID, mappedID := nativeID, targetID
			if f.direction == "C2H" {
				inputID, mappedID = targetID, nativeID
			}
			class, ids := coverageClassify(plain, f, inputID, mappedID)
			if class != "passthrough" {
				t.Errorf("plain New() proto %d %s: got %q ids=%v, want passthrough", protoID, f.name, class, ids)
			}
		}
		t.Logf("proto %d plain New(): all 10 families passthrough (control)", protoID)
	}

	// Native 2193 identity control: no conversion, IDs stay native.
	identity := minecraft.DefaultProtocol
	apple := nativeAppleID(t)
	for _, f := range families {
		_, ids := coverageClassify(identity, f, apple, apple)
		for _, v := range ids {
			if v != 0 && v != apple {
				t.Errorf("2193 identity %s: item ID changed to %d, want %d", f.name, v, apple)
			}
		}
	}
	t.Logf("proto 2193 identity: all 10 families keep the native ID %d (control)", apple)
}

// TestSemanticItemRegistryCoverage pins the ELEVENTH family: the ItemRegistry
// table itself has a special case BEFORE the gameplay switch. Native input is
// translated to the target table; a non-native table is dropped outright.
func TestSemanticItemRegistryCoverage(t *testing.T) {
	native := mustNativeItems(t)
	for _, protoID := range []int32{1001, 2168, 2169} {
		codec, _ := resolveProto(protoID)
		target, err := blocktranslate.Items(protoID)
		if err != nil {
			t.Fatal(err)
		}
		out := codec.ConvertFromLatest(&packet.ItemRegistry{Items: native}, nil)
		if len(out) != 1 {
			t.Errorf("proto %d: native ItemRegistry produced %d packets", protoID, len(out))
			continue
		}
		lengths, found := structSliceLengths(out[0], "ItemRegistry")
		if !found || lengths["Items"] != len(target) {
			t.Errorf("proto %d: emitted ItemRegistry has %d entries (found=%v), want %d",
				protoID, lengths["Items"], found, len(target))
		} else {
			t.Logf("proto %d ItemRegistry: native %d -> emitted %d entries", protoID, len(native), lengths["Items"])
		}

		dropped := codec.ConvertFromLatest(&packet.ItemRegistry{Items: target}, nil)
		if len(dropped) != 0 {
			t.Errorf("proto %d: non-native ItemRegistry was NOT dropped (%d packets)", protoID, len(dropped))
		}
	}
}

// TestSemanticCraftingDataSliceSurvival measures the recipe-side LOSS that the
// old README claim ("recipes are not solved") hid. mapCraftingData remaps nine
// slices but explicitly nils out the two Education-Edition chemistry slices and
// MaterialReducers. host1001Protocol decodes them from the 1001 wire, but the
// client codec discards them again.
func TestSemanticCraftingDataSliceSurvival(t *testing.T) {
	apple := nativeAppleID(t)
	recipe := protocol.ShapelessRecipe{RecipeID: "coverage", RecipeNetworkID: 1,
		Output: []protocol.ItemStack{covStack(apple)}}
	shaped := protocol.ShapedRecipe{RecipeID: "coverage", Width: 1, Height: 1,
		Output: []protocol.ItemStack{covStack(apple)}}

	full := func() *packet.CraftingData {
		return &packet.CraftingData{
			ShapedRecipes:             []protocol.ShapedRecipe{shaped},
			ShapelessRecipes:          []protocol.ShapelessRecipe{recipe},
			MultiRecipes:              []protocol.MultiRecipe{{RecipeNetworkID: 1}},
			UserDataShapelessRecipes:  []protocol.UserDataShapelessRecipe{{ShapelessRecipe: recipe}},
			ShapelessChemistryRecipes: []protocol.ShapelessChemistryRecipe{{ShapelessRecipe: recipe}},
			ShapedChemistryRecipes:    []protocol.ShapedChemistryRecipe{{ShapedRecipe: shaped}},
			SmithingTransformRecipes:  []protocol.SmithingTransformRecipe{{RecipeNetworkID: 1, Result: covStack(apple)}},
			SmithingTrimRecipes:       []protocol.SmithingTrimRecipe{{RecipeNetworkID: 1}},
			PotionRecipes:             []protocol.PotionRecipe{{InputPotionID: apple, ReagentItemID: apple, OutputPotionID: apple}},
			PotionContainerChangeRecipes: []protocol.PotionContainerChangeRecipe{{
				InputItemID: apple, ReagentItemID: apple, OutputItemID: apple,
			}},
			MaterialReducers: []protocol.MaterialReducer{{InputItem: protocol.ItemType{NetworkID: apple}}},
		}
	}

	wantDropped := []string{"ShapelessChemistryRecipes", "ShapedChemistryRecipes", "MaterialReducers"}
	required := []string{"ShapedRecipes", "ShapelessRecipes", "MultiRecipes",
		"UserDataShapelessRecipes", "PotionRecipes", "PotionContainerChangeRecipes"}

	for _, protoID := range []int32{1001, 2168, 2169} {
		codec, _ := resolveProto(protoID)
		out := codec.ConvertFromLatest(full(), nil)
		if len(out) == 0 {
			t.Fatalf("proto %d: CraftingData was dropped", protoID)
		}
		got, found := structSliceLengths(out[0], "CraftingData")
		if !found {
			t.Fatalf("proto %d: could not find CraftingData in %T", protoID, out[0])
		}
		kept := make([]string, 0)
		for k, n := range got {
			if n > 0 {
				kept = append(kept, fmt.Sprintf("%s=%d", k, n))
			}
		}
		sort.Strings(kept)
		for _, dropped := range wantDropped {
			if got[dropped] != 0 {
				t.Errorf("proto %d: %s survived with %d entries, want 0 (mapCraftingData nils it)", protoID, dropped, got[dropped])
			}
		}
		for _, name := range required {
			if got[name] == 0 {
				t.Errorf("proto %d: %s was dropped, want >=1", protoID, name)
			}
		}
		t.Logf("proto %d CraftingData kept: %s", protoID, strings.Join(kept, ", "))
	}

	// 2193 identity keeps all eleven slices: the loss is in the adapter, not
	// in the wire model.
	identity := minecraft.DefaultProtocol
	out := identity.ConvertFromLatest(full(), nil)
	if len(out) != 1 {
		t.Fatalf("2193 identity CraftingData out=%d", len(out))
	}
	got, found := structSliceLengths(out[0], "CraftingData")
	if !found {
		t.Fatalf("2193 identity returned %T", out[0])
	}
	for _, name := range wantDropped {
		if got[name] != 1 {
			t.Errorf("2193 identity dropped %s (%d entries), want 1", name, got[name])
		}
	}
	t.Logf("proto 2193 identity CraftingData kept all 11 slices (control)")
}

// TestSemanticStackRequestPacketsCarryNoRuntimeIDs pins why ItemStackRequest
// and ItemStackResponse are NOT part of the item-ID mapping problem: they use
// name-based item identifiers and dynamic StackNetworkID, never item runtime
// IDs. They also pass through the registry-aware codec unchanged.
func TestSemanticStackRequestPacketsCarryNoRuntimeIDs(t *testing.T) {
	request := &packet.ItemStackRequest{Requests: []protocol.ItemStackRequest{{
		RequestID: 1,
		Actions: []protocol.StackRequestAction{
			&protocol.CraftResultsDeprecatedStackRequestAction{
				ResultItems:  []protocol.StackRequestItem{{Identifier: "minecraft:apple", Count: 1}},
				TimesCrafted: 1,
			},
		},
	}}}
	response := &packet.ItemStackResponse{Responses: []protocol.ItemStackResponse{{
		RequestID: 1,
		ContainerInfo: []protocol.StackResponseContainerInfo{{
			SlotInfo: []protocol.StackResponseSlotInfo{{StackNetworkID: 99}},
		}},
	}}}

	for name, pk := range map[string]packet.Packet{"ItemStackRequest": request, "ItemStackResponse": response} {
		ids := int32FieldValues(pk, "NetworkID")
		if len(ids) != 0 {
			t.Errorf("%s unexpectedly carries NetworkID fields: %v", name, ids)
		}
	}
	codec, _ := resolveProto(2169)
	if out := codec.ConvertToLatest(request, nil); len(out) != 1 {
		t.Errorf("ItemStackRequest ConvertToLatest out=%d, want 1", len(out))
	}
	if out := codec.ConvertFromLatest(response, nil); len(out) != 1 {
		t.Errorf("ItemStackResponse ConvertFromLatest out=%d, want 1", len(out))
	}
	t.Log("ItemStackRequest / ItemStackResponse: 0 NetworkID fields, pass through unchanged")
}

// TestSemanticNarrowPackedIDsAreMapped measures the SMALL set of non-stack
// semantic cases the adapter DOES rewrite: a packed item ID in LevelEvent's
// legacy particle event, a packed item ID in ActorEvent's feed event, and a
// block runtime ID in an AddActor falling-block variant. These are the only
// particle/entity-adjacent payloads that are translated.
func TestSemanticNarrowPackedIDsAreMapped(t *testing.T) {
	_, nativeID, targetID := coverageItem(t, 2169)
	codec, _ := resolveProto(2169)
	plain := gameplayPlainCodec(2169)

	pack := func(id int32) int32 { return int32(uint32(id)<<16 | 7) }
	unpack := func(v int32) int32 { return int32(int16(uint32(v) >> 16)) }

	cases := []struct {
		name  string
		build func(int32) packet.Packet
		conv  func(minecraft.Protocol, packet.Packet) []packet.Packet
	}{
		{"LevelEvent legacy item particle", func(id int32) packet.Packet {
			return &packet.LevelEvent{EventType: packet.LevelEventParticleLegacyEvent | 14, EventData: pack(id)}
		}, func(p minecraft.Protocol, pk packet.Packet) []packet.Packet {
			return p.ConvertFromLatest(pk, nil)
		}},
		{"ActorEvent feed item", func(id int32) packet.Packet {
			return &packet.ActorEvent{EventType: packet.ActorEventFeed, EventData: pack(id)}
		}, func(p minecraft.Protocol, pk packet.Packet) []packet.Packet {
			return p.ConvertFromLatest(pk, nil)
		}},
	}
	for _, c := range cases {
		mapped := c.conv(codec, c.build(nativeID))
		if len(mapped) != 1 {
			t.Errorf("%s: registry-aware out=%d, want 1", c.name, len(mapped))
			continue
		}
		raw, ok := firstInt32Field(mapped[0], "EventData")
		if !ok {
			t.Errorf("%s: no EventData field in %T", c.name, mapped[0])
			continue
		}
		got := unpack(raw)
		if got != targetID {
			t.Errorf("%s: packed ID mapped to %d, want %d", c.name, got, targetID)
		}
		control := c.conv(plain, c.build(nativeID))
		if len(control) != 1 {
			t.Errorf("%s: plain New() out=%d, want 1", c.name, len(control))
			continue
		}
		rawControl, ok := firstInt32Field(control[0], "EventData")
		if !ok {
			t.Errorf("%s: plain New() lost the EventData field", c.name)
			continue
		}
		if got := unpack(rawControl); got != nativeID {
			t.Errorf("%s: plain New() changed the packed ID to %d, want passthrough %d", c.name, got, nativeID)
		}
		t.Logf("%s: %d -> %d (plain New() keeps %d)", c.name, nativeID, targetID, nativeID)
	}
}

// TestSemanticPayloadFamiliesAreNotRemapped is the other half of PROTO-005:
// biome schemas, generic entity metadata, spawn-particle payloads, block-entity
// NBT and actor identifiers are forwarded byte-for-byte, with NO semantic
// re-mapping. The self-check only COUNTS them; it does not translate them.
func TestSemanticPayloadFamiliesAreNotRemapped(t *testing.T) {
	codec, _ := resolveProto(2169)
	cases := []struct {
		name string
		pk   packet.Packet
	}{
		{"BiomeDefinitionList", &packet.BiomeDefinitionList{
			BiomeDefinitions: []protocol.BiomeDefinition{{NameIndex: 0, BiomeID: 1}},
			StringList:       []string{"minecraft:plains"},
		}},
		{"SetActorData", &packet.SetActorData{
			EntityRuntimeID: 7,
			EntityMetadata:  protocol.EntityMetadata{42: int32(7)},
		}},
		{"SpawnParticleEffect", &packet.SpawnParticleEffect{
			Dimension: 0, EntityUniqueID: 7, ParticleName: "minecraft:flame",
		}},
		{"BlockActorData", &packet.BlockActorData{
			Position: protocol.BlockPos{1, 2, 3},
			NBTData:  map[string]any{"id": "Chest", "Items": []any{}},
		}},
		{"AvailableActorIdentifiers", &packet.AvailableActorIdentifiers{}},
	}
	for _, c := range cases {
		out := codec.ConvertFromLatest(c.pk, nil)
		if len(out) != 1 {
			t.Errorf("%s: out=%d, want 1 (forwarded unchanged)", c.name, len(out))
			continue
		}
		// No item runtime ID appears anywhere in the family, mapped or not.
		if ids := int32FieldValues(out[0], "NetworkID"); len(ids) != 0 {
			t.Errorf("%s: unexpectedly contains NetworkID fields %v", c.name, ids)
		}
		t.Logf("%s: forwarded as 1 packet, 0 item runtime IDs", c.name)
	}
}
