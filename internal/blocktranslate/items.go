package blocktranslate

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	v1001 "github.com/shawtymarco/go-multiversion/data/v1001"
	v2169 "github.com/shawtymarco/go-multiversion/data/v2169"
	"github.com/shawtymarco/go-multiversion/mapping"
)

// ItemRegistry is per-protocol — same principle as the block palette.
//
// ★ Why this is needed: the listener sends the client an `ItemRegistry` taken from the HOST's
// `GameData.Items`. A client on a different protocol then receives the wrong item table, so item
// network IDs drift (2193 has 2076 items, 1001 has 1933, 2169 has 1976) — holding, placing or
// dropping items renders wrong or vanishes.
// Amber (`ItemRegistryPacket::create($typeConverter->getItemTypeDictionary()->getEntries())`) and
// Ouranos (`ItemTypeDictionary.getInstance(downstreamProtocolId)`) both REPLACE the client-side
// registry with the client protocol's own table.
//
// Provenance and — the important part — consistency:
//   - 2193 is the CloudburstMC/Data `runtime_item_states.json` snapshot pinned in manifest.json
//     (a8a4341d7763d6eb8547cff3ca46b4153d60163d, Apache-2.0). It is also the NATIVE table the
//     go-multiversion adapters expect as input.
//   - 1001 and 2169 are built from the very same `go-multiversion/data/v1001` and `/v2169`
//     `vanilla_items.nbt` snapshots that `protocols/v1_26_30` and `/v1_26_45` feed into
//     `mapping.NewItemMapper(...).TargetEntries()`. Using the exact same constructor here means
//     `Items(id)` is the same table the registry-aware codec emits for that protocol, instead of
//     a second, independently pinned copy that could drift.
//   - 2168 shares 2169's table. This is not a guess: `protocols/v1_26_44.NewWithRegistries`
//     delegates to `v1_26_45.NewWithRegistries`, documented as "the shared 1.26.40–1.26.45
//     registry baseline". There is no `data/v2168` package because the family is one snapshot.
//
// Caller contract (see `cmd/xvrelay/main.go`): a registry-AWARE client codec translates
// `ItemRegistry` itself and expects the NATIVE (2193) table in GameData.Items; a registry-LESS
// codec passes GameData.Items straight through and must be handed the client's OWN table.
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

// targetItems converts one historical dragonfly item snapshot into the mapping.TargetItem shape
// expected by mapping.NewItemMapper. Both data packages define their own ItemEntry type, hence the
// generic field accessor.
func targetItems[T any](src map[string]T, fields func(T) (int32, bool, int32, map[string]any)) map[string]mapping.TargetItem {
	out := make(map[string]mapping.TargetItem, len(src))
	for name, item := range src {
		id, componentBased, version, data := fields(item)
		out[name] = mapping.TargetItem{
			RuntimeID:      id,
			ComponentBased: componentBased,
			Version:        version,
			Data:           data,
		}
	}
	return out
}

func loadItems() {
	itemCatalog.items = map[int32][]protocol.ItemEntry{}

	// 1. Native (2193) table — also the input the adapters translate FROM.
	raw, err := palettes.ReadFile("data/items-2193.json")
	if err != nil {
		itemCatalog.err = err
		return
	}
	var states []itemState
	if err := json.Unmarshal(raw, &states); err != nil {
		itemCatalog.err = fmt.Errorf("data/items-2193.json: %w", err)
		return
	}
	native := make([]protocol.ItemEntry, 0, len(states))
	for _, s := range states {
		native = append(native, protocol.ItemEntry{
			Name:           s.Name,
			RuntimeID:      s.ID,
			ComponentBased: s.ComponentBased,
			Version:        s.Version,
		})
	}
	itemCatalog.items[2193] = native

	// 2. Historical tables, built by the same mapper the codecs use.
	items1001, err := v1001.Items()
	if err != nil {
		itemCatalog.err = fmt.Errorf("v1001 item snapshot: %w", err)
		return
	}
	target1001 := targetItems(items1001, func(e v1001.ItemEntry) (int32, bool, int32, map[string]any) {
		return e.RuntimeID, e.ComponentBased, e.Version, e.Data
	})
	m1001, err := mapping.NewItemMapper(native, target1001)
	if err != nil {
		itemCatalog.err = fmt.Errorf("build 1001 item registry: %w", err)
		return
	}
	itemCatalog.items[1001] = m1001.TargetEntries()

	items2169, err := v2169.Items()
	if err != nil {
		itemCatalog.err = fmt.Errorf("v2169 item snapshot: %w", err)
		return
	}
	target2169 := targetItems(items2169, func(e v2169.ItemEntry) (int32, bool, int32, map[string]any) {
		return e.RuntimeID, e.ComponentBased, e.Version, e.Data
	})
	m2169, err := mapping.NewItemMapper(native, target2169)
	if err != nil {
		itemCatalog.err = fmt.Errorf("build 2169 item registry: %w", err)
		return
	}
	entries2169 := m2169.TargetEntries()
	itemCatalog.items[2169] = entries2169
	itemCatalog.items[2168] = entries2169
}

// Items returns the item registry advertised to a client of protocol id. It errors when no table
// exists for that protocol, so the caller keeps the host's registry instead of inventing one.
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

// NativeItems is the 2193 table the registry-aware codecs expect as their INPUT. Keeping it as a
// named helper makes the caller's intent explicit and avoids a magic number at the call site.
func NativeItems() ([]protocol.ItemEntry, error) { return Items(2193) }
