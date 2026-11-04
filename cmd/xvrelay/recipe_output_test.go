package main

import (
	"bytes"
	"bedrock-cross-version-relay/internal/blocktranslate"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	data1001 "github.com/shawtymarco/go-multiversion/data/v1001"
	legacy "github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
	"testing"
)

func TestHostRecipeOutputsUseNativeItemRegistry(t *testing.T) {
	host, err := data1001.Items()
	if err != nil {
		t.Fatal(err)
	}
	native, err := blocktranslate.Items(2193)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]int32{}
	for _, e := range native {
		targets[e.Name] = int32(e.RuntimeID)
	}
	var hostEntries []protocol.ItemEntry
	for name, e := range host {
		hostEntries = append(hostEntries, protocol.ItemEntry{Name: name, RuntimeID: int16(e.RuntimeID)})
	}
	for _, name := range []string{"minecraft:black_dye", "minecraft:ink_sac", "minecraft:diamond_sword", "minecraft:apple"} {
		source, ok := host[name]
		if !ok {
			t.Fatal(name)
		}
		pk := &packet.CraftingData{ShapelessRecipes: []protocol.ShapelessRecipe{{RecipeID: name, Block: "crafting_table", Output: []protocol.ItemStack{{ItemType: protocol.ItemType{NetworkID: source.RuntimeID}, Count: 1}}}}}
		codec := legacy.New()
		encoded := codec.ConvertFromLatest(pk, nil)
		if len(encoded) != 1 {
			t.Fatal("missing wire recipe")
		}
		var raw bytes.Buffer
		encoded[0].Marshal(codec.NewWriter(&raw, 0))
		got, err := decodeHostRecipes1001(raw.Bytes(), hostEntries, 0)
		if err != nil {
			t.Fatal(err)
		}
		output := got.ShapelessRecipes[0].Output[0]
		t.Logf("%s host=%d native=%d decoded=%d count=%d", name, source.RuntimeID, targets[name], output.NetworkID, output.Count)
		if output.NetworkID != targets[name] || output.Count != 1 {
			t.Errorf("%s output registry not translated: %+v", name, output)
		}
	}
}

func TestHostBrewingAndReducerUseNativeItemRegistry(t *testing.T) {
	host, err := data1001.Items()
	if err != nil {
		t.Fatal(err)
	}
	native, err := blocktranslate.Items(2193)
	if err != nil {
		t.Fatal(err)
	}
	sourceID := func(name string) int32 { return host["minecraft:"+name].RuntimeID }
	targets := map[string]int32{}
	var entries []protocol.ItemEntry
	for name, e := range host {
		entries = append(entries, protocol.ItemEntry{Name: name, RuntimeID: int16(e.RuntimeID)})
	}
	for _, e := range native {
		targets[e.Name] = int32(e.RuntimeID)
	}
	targetID := func(name string) int32 { return targets["minecraft:"+name] }
	pk := &packet.CraftingData{
		PotionRecipes:                []protocol.PotionRecipe{{InputPotionID: sourceID("potion"), InputPotionMetadata: 1, ReagentItemID: sourceID("gunpowder"), OutputPotionID: sourceID("splash_potion"), OutputPotionMetadata: 2}},
		PotionContainerChangeRecipes: []protocol.PotionContainerChangeRecipe{{InputItemID: sourceID("splash_potion"), ReagentItemID: sourceID("dragon_breath"), OutputItemID: sourceID("lingering_potion")}},
		MaterialReducers:             []protocol.MaterialReducer{{InputItem: protocol.ItemType{NetworkID: sourceID("diamond_sword"), MetadataValue: 3}, Outputs: []protocol.MaterialReducerOutput{{NetworkID: sourceID("black_dye"), Count: 2}}}},
	}
	codec := legacy.New()
	var raw bytes.Buffer
	out := codec.ConvertFromLatest(pk, nil)
	if len(out) != 1 {
		t.Fatal("missing wire recipe")
	}
	out[0].Marshal(codec.NewWriter(&raw, 0))
	got, err := decodeHostRecipes1001(raw.Bytes(), entries, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := got.PotionRecipes[0]
	if p.InputPotionID != targetID("potion") || p.ReagentItemID != targetID("gunpowder") || p.OutputPotionID != targetID("splash_potion") || p.InputPotionMetadata != 1 || p.OutputPotionMetadata != 2 {
		t.Errorf("brewing IDs/metadata: %+v", p)
	}
	c := got.PotionContainerChangeRecipes[0]
	if c.InputItemID != targetID("splash_potion") || c.ReagentItemID != targetID("dragon_breath") || c.OutputItemID != targetID("lingering_potion") {
		t.Errorf("container IDs: %+v", c)
	}
	r := got.MaterialReducers[0]
	if r.InputItem.NetworkID != targetID("diamond_sword") || r.InputItem.MetadataValue != 3 || r.Outputs[0].NetworkID != targetID("black_dye") || r.Outputs[0].Count != 2 {
		t.Errorf("reducer IDs/count/meta: %+v", r)
	}
}

// TestHostRecipeDropsAddonItemWithoutFailingDecode is a regression test for the bug measured at
// log 13:29:22 (world mrzerovn, host1001):
//
//	decode host1001 CraftingData: recipe item dl:copper_lantern has no native item mapping
//	read packet: ... link=host   ⇒ host conn closed ⇒ client session died 3.33 s later
//
// A host addon item missing from the native registry must NOT break the whole packet: only the
// recipes containing it are dropped, the rest still reach the client.
func TestHostRecipeDropsAddonItemWithoutFailingDecode(t *testing.T) {
	host, err := data1001.Items()
	if err != nil {
		t.Fatal(err)
	}
	var entries []protocol.ItemEntry
	for name, e := range host {
		entries = append(entries, protocol.ItemEntry{Name: name, RuntimeID: int16(e.RuntimeID)})
	}
	// An addon-only item: the host knows it, the native 2193 registry does not.
	const addonID int16 = 6000
	entries = append(entries, protocol.ItemEntry{Name: "dl:copper_lantern", RuntimeID: addonID})

	apple, ok := host["minecraft:apple"]
	if !ok {
		t.Fatal("minecraft:apple missing from 1001 registry")
	}
	pk := &packet.CraftingData{ShapelessRecipes: []protocol.ShapelessRecipe{
		{
			RecipeID: "vanilla_apple", Block: "crafting_table",
			Output: []protocol.ItemStack{{ItemType: protocol.ItemType{NetworkID: apple.RuntimeID}, Count: 1}},
		},
		{
			RecipeID: "addon_lantern", Block: "crafting_table",
			Output: []protocol.ItemStack{{ItemType: protocol.ItemType{NetworkID: int32(addonID)}, Count: 1}},
		},
	}}
	codec := legacy.New()
	encoded := codec.ConvertFromLatest(pk, nil)
	if len(encoded) != 1 {
		t.Fatal("missing wire recipe")
	}
	var raw bytes.Buffer
	encoded[0].Marshal(codec.NewWriter(&raw, 0))

	got, err := decodeHostRecipes1001(raw.Bytes(), entries, 0)
	if err != nil {
		t.Fatalf("decode must tolerate the addon item, but failed: %v", err)
	}
	if len(got.ShapelessRecipes) != 1 {
		t.Fatalf("expected 1 surviving recipe, got %d", len(got.ShapelessRecipes))
	}
	if got.ShapelessRecipes[0].RecipeID != "vanilla_apple" {
		t.Errorf("wrong recipe survived: %q", got.ShapelessRecipes[0].RecipeID)
	}
}
