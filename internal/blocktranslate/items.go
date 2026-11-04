package blocktranslate

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// ItemRegistry is per-protocol — same principle as the block palette.
//
// ★ Why this is needed: the listener sends the client an `ItemRegistry` taken from the HOST's
// `GameData.Items` (1.26.30/1001). A 1.26.51/2193 client then receives the old item table, so item
// network IDs drift (2193 has 2076 items, 1001 has 1933; 143 items exist only in 2193, and 587
// share a name but not an ID) — holding, placing or dropping items renders wrong or vanishes.
// Amber (`ItemRegistryPacket::create($typeConverter->getItemTypeDictionary()->getEntries())`) and
// Ouranos (`ItemTypeDictionary.getInstance(downstreamProtocolId)`) both REPLACE the client-side
// registry with the client protocol's own table.
//
// Source: CloudburstMC/Data `runtime_item_states.json` at the commit recorded in manifest.json
// (2193 = a8a4341d7763d6eb8547cff3ca46b4153d60163d, Apache-2.0).
type itemState struct {
	Name           string `json:"name"`
	ID             int16  `json:"id"`
	ComponentBased bool   `json:"componentBased"`
	Version        int32  `json:"version"`
}

var itemCatalog struct {
	sync.Once
	items map[int32][]protocol.ItemEntry
	err   error
}

func loadItems() {
	itemCatalog.items = map[int32][]protocol.ItemEntry{}
	for _, id := range []int32{2193} {
		name := fmt.Sprintf("data/items-%d.json", id)
		raw, err := palettes.ReadFile(name)
		if err != nil {
			itemCatalog.err = err
			return
		}
		var states []itemState
		if err := json.Unmarshal(raw, &states); err != nil {
			itemCatalog.err = fmt.Errorf("%s: %w", name, err)
			return
		}
		entries := make([]protocol.ItemEntry, 0, len(states))
		for _, s := range states {
			entries = append(entries, protocol.ItemEntry{
				Name:           s.Name,
				RuntimeID:      s.ID,
				ComponentBased: s.ComponentBased,
				Version:        s.Version,
			})
		}
		itemCatalog.items[id] = entries
	}
}

// Items returns the embedded item registry for a protocol. It errors when no table exists for that
// protocol, so the caller keeps the host's registry (the previous behaviour) instead of inventing one.
func Items(id int32) ([]protocol.ItemEntry, error) {
	itemCatalog.Do(loadItems)
	if itemCatalog.err != nil {
		return nil, itemCatalog.err
	}
	items, ok := itemCatalog.items[id]
	if !ok || len(items) == 0 {
		return nil, fmt.Errorf("no item registry for protocol %d", id)
	}
	return items, nil
}
