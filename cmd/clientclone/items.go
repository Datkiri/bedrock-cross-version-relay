package main

// Live item visibility. cmd/itemaudit answers "which items WOULD become AIR"
// offline; this file answers "which items did a real headless client actually
// DECODE from the live wire". Each item stack that reaches post-spawn packet
// handlers is looked up in the client's own protocol item table, so an unknown
// or zeroed runtime ID is visible as a named log line instead of a vague
// caveat. It is still a codec-level client: it does not render an inventory.
//
// SCOPE (PROTO-005): stacksFrom() observes SIX packet families, not every
// item-carrying packet. The relay codec remaps TEN stack-carrying families
// (see cmd/xvrelay/semantic_coverage_test.go); this census omits
// AddPlayer.HeldItem, MobArmourEquipment, InventoryTransaction and
// PlayerAuthInput.ItemInteractionData. The live census numbers are therefore
// a lower bound on decoded stacks, not a complete item inventory of the wire.

import (
	"fmt"
	"os"
	"sort"

	"bedrock-cross-version-relay/internal/blocktranslate"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// itemIndex maps a runtime ID from the CLIENT's protocol table back to its
// namespaced name. The relay encodes item IDs in the target protocol, so the
// client must resolve against Items(clientProto), not the native 2193 table.
type itemIndex struct {
	byID   map[int32]string
	byName map[string]int32
	count  int
}

func newItemIndex(proto int32) (itemIndex, error) {
	entries, err := blocktranslate.Items(proto)
	if err != nil {
		return itemIndex{}, err
	}
	idx := itemIndex{
		byID:   make(map[int32]string, len(entries)),
		byName: make(map[string]int32, len(entries)),
		count:  len(entries),
	}
	for _, e := range entries {
		idx.byID[int32(e.RuntimeID)] = e.Name
		idx.byName[e.Name] = int32(e.RuntimeID)
	}
	return idx, nil
}

// itemStats is the per-session wire-level item census.
type itemStats struct {
	Decoded     uint64
	Visible     uint64
	Lost        uint64 // CreativeContent entry that arrived as AIR (id 0)
	Unknown     uint64 // non-zero ID absent from the client's table
	Empty       uint64 // legitimate empty inventory slot
	ByPacket    map[string]uint64
	Samples     []string // first N visible names, for the JSON report
	LostPackets []string // packet families where a lost item was observed
}

func newItemStats() itemStats {
	return itemStats{ByPacket: map[string]uint64{}}
}

// stacksFrom extracts every item stack a canonical packet carries. The relay
// converts host packets to the latest layout before the client sees them, so
// these are plain gophertunnel packet types, not translatedPacket wrappers.
// creative=true marks packets whose entries must never be empty (CreativeContent
// and recipe outputs): an id of 0 there means an item was lost in translation.
func stacksFrom(pk packet.Packet) (stacks []protocol.ItemStack, creative bool) {
	switch p := pk.(type) {
	case *packet.InventoryContent:
		for _, it := range p.Content {
			stacks = append(stacks, it.Stack)
		}
		if p.StorageItem.Stack.NetworkID != 0 {
			stacks = append(stacks, p.StorageItem.Stack)
		}
	case *packet.InventorySlot:
		stacks = append(stacks, p.NewItem.Stack)
		if v, ok := p.StorageItem.Value(); ok {
			stacks = append(stacks, v.Stack)
		}
	case *packet.MobEquipment:
		stacks = append(stacks, p.NewItem.Stack)
	case *packet.AddItemActor:
		stacks = append(stacks, p.Item.Stack)
	case *packet.CreativeContent:
		creative = true
		for _, it := range p.Items {
			stacks = append(stacks, it.Item)
		}
	case *packet.CraftingData:
		creative = true
		for _, r := range p.ShapedRecipes {
			stacks = append(stacks, r.Output...)
		}
		for _, r := range p.ShapelessRecipes {
			stacks = append(stacks, r.Output...)
		}
		for _, r := range p.UserDataShapelessRecipes {
			stacks = append(stacks, r.Output...)
		}
		for _, r := range p.ShapelessChemistryRecipes {
			stacks = append(stacks, r.Output...)
		}
		for _, r := range p.ShapedChemistryRecipes {
			stacks = append(stacks, r.Output...)
		}
	}
	return stacks, creative
}

// observeItems is called for every post-spawn packet. It never mutates the
// connection; it only decodes what the client already received.
func (s *session) observeItems(pk packet.Packet) {
	if !s.itemsReady.Load() {
		return
	}
	stacks, creative := stacksFrom(pk)
	if len(stacks) == 0 {
		return
	}
	family := packetName(pk)
	for _, st := range stacks {
		s.observeStack(family, st.NetworkID, creative)
	}
}

func (s *session) observeStack(family string, id int32, creative bool) {
	s.mu.Lock()
	s.items.Decoded++
	s.items.ByPacket[family]++
	var line string
	var logLine bool
	switch {
	case id == 0 && creative:
		s.items.Lost++
		if len(s.items.LostPackets) < 16 {
			s.items.LostPackets = append(s.items.LostPackets, family)
		}
		line = fmt.Sprintf("clientclone: ITEM_LOST packet=%s id=0 (entry became AIR before it reached the client)", family)
		logLine = true
	case id == 0:
		s.items.Empty++
		// Legitimate empty inventory slot: counted, not logged by default.
	case s.itemsIndex.byID != nil && s.itemsIndex.byID[id] != "":
		name := s.itemsIndex.byID[id]
		s.items.Visible++
		if len(s.items.Samples) < 64 {
			s.items.Samples = append(s.items.Samples, name)
		}
		line = fmt.Sprintf("clientclone: ITEM_VISIBLE packet=%s id=%d name=%s table=%d",
			family, id, name, s.itemsIndex.count)
		logLine = true
	default:
		s.items.Unknown++
		line = fmt.Sprintf("clientclone: ITEM_UNKNOWN_ID packet=%s id=%d (absent from the client's item table)", family, id)
		logLine = true
	}
	logged := s.itemLogCount
	if logLine && (s.cfg.itemLog < 0 || logged < s.cfg.itemLog) {
		s.itemLogCount++
	}
	s.mu.Unlock()
	if logLine && (s.cfg.itemLog < 0 || logged < s.cfg.itemLog) {
		fmt.Fprintln(os.Stderr, line)
	}
}

// itemSummary returns a stable, sorted view for dump/JSON.
func (s *session) itemSummary() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	byPacket := make(map[string]uint64, len(s.items.ByPacket))
	for k, v := range s.items.ByPacket {
		byPacket[k] = v
	}
	samples := append([]string(nil), s.items.Samples...)
	sort.Strings(samples)
	return map[string]any{
		"items_decoded":     s.items.Decoded,
		"items_visible":     s.items.Visible,
		"items_lost":        s.items.Lost,
		"items_unknown":     s.items.Unknown,
		"items_empty_slots": s.items.Empty,
		"items_table_size":  s.itemsIndex.count,
		"items_by_packet":   byPacket,
		"items_visible_samples": func() []string {
			if len(samples) > 24 {
				return samples[:24]
			}
			return samples
		}(),
		"items_lost_families": append([]string(nil), s.items.LostPackets...),
	}
}
