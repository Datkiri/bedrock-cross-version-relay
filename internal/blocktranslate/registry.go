// Package blocktranslate maps block states and network palettes between the
// protocol families supported by xvrelay. No runtime-ID ordering is inferred.
package blocktranslate

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"sync"

	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/shawtymarco/go-multiversion/mapping"
)

//go:embed data/*.nbt data/items-2193.json
var palettes embed.FS

type registry struct {
	states []mapping.BlockState
	hashes []uint32
	byHash map[uint32]uint32
	byKey  map[string]uint32
	air    uint32
}

func (r *registry) BlockCount() int      { return len(r.states) }
func (r *registry) AirRuntimeID() uint32 { return r.air }
func (r *registry) RuntimeIDToState(id uint32) (string, map[string]any, bool) {
	if id >= uint32(len(r.states)) {
		return "", nil, false
	}
	s := r.states[id]
	return s.Name, s.Properties, true
}

var catalog struct {
	sync.Once
	registries map[int32]*registry
	mappers    map[int32]*mapping.BlockMapper
	err        error
}

func loadRegistry(id int32) (*registry, error) {
	data, err := palettes.ReadFile(fmt.Sprintf("data/%d.nbt", id))
	if err != nil {
		return nil, err
	}
	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer z.Close()
	var root struct {
		Blocks []struct {
			BlockID    int32          `nbt:"block_id"`
			NameHash   int64          `nbt:"name_hash"`
			NetworkID  int32          `nbt:"network_id"`
			Name       string         `nbt:"name"`
			Properties map[string]any `nbt:"states"`
			Version    int32          `nbt:"version"`
		} `nbt:"blocks"`
	}
	if err := nbt.NewDecoderWithEncoding(z, nbt.BigEndian).Decode(&root); err != nil {
		return nil, err
	}
	if len(root.Blocks) == 0 {
		return nil, fmt.Errorf("palette%d has no blocks", id)
	}
	r := &registry{byHash: map[uint32]uint32{}, byKey: map[string]uint32{}}
	for i, s := range root.Blocks {
		state := mapping.BlockState{Name: s.Name, Properties: s.Properties, Version: s.Version}
		key, err := mapping.StateKey(s.Name, s.Properties)
		if err != nil {
			return nil, err
		}
		hash, err := StateHash(s.Name, s.Properties)
		if err != nil {
			return nil, err
		}
		if hash != uint32(s.NetworkID) {
			return nil, fmt.Errorf("palette%d %s computed hash %#x differs from pinned network_id %#x", id, s.Name, hash, uint32(s.NetworkID))
		}
		if previous, exists := r.byHash[hash]; exists {
			return nil, fmt.Errorf("palette%d hash collision at%d and%d", id, previous, i)
		}
		if _, exists := r.byKey[key]; exists {
			return nil, fmt.Errorf("palette%d duplicate state %s", id, s.Name)
		}
		r.states = append(r.states, state)
		r.hashes = append(r.hashes, hash)
		r.byHash[hash] = uint32(i)
		r.byKey[key] = uint32(i)
		if s.Name == "minecraft:air" {
			r.air = uint32(i)
		}
	}
	return r, nil
}

func initCatalog() {
	catalog.registries = map[int32]*registry{}
	catalog.mappers = map[int32]*mapping.BlockMapper{}
	for _, id := range []int32{1001, 2168, 2193} {
		r, err := loadRegistry(id)
		if err != nil {
			catalog.err = err
			return
		}
		catalog.registries[id] = r
	}
	// These protocol families share the 1.26.40–45 block schema, as in the
	// pinned v1_26_44 adapter. Keep the snapshot provenance explicit.
	catalog.registries[2169] = catalog.registries[2168]
	for _, id := range []int32{1001, 2168} {
		m, err := mapping.NewBlockMapperWithTargetOrder(catalog.registries[2193], catalog.registries[id].states)
		if err != nil {
			catalog.err = fmt.Errorf("registry%d to2193: %w", id, err)
			return
		}
		catalog.mappers[id] = m
	}
	catalog.mappers[2169] = catalog.mappers[2168]
}

// NativeRegistry returns a protocol's block palette as a mapping.BlockRegistry, so the
// go-multiversion adapter (NewWithRegistries) can drive its own item/creative/update-block
// conversion. Returns the cached registry; it does not rebuild.
func NativeRegistry(id int32) (mapping.BlockRegistry, error) {
	catalog.Do(initCatalog)
	if catalog.err != nil {
		return nil, catalog.err
	}
	r := catalog.registries[id]
	if r == nil {
		return nil, fmt.Errorf("unsupported block registry%d", id)
	}
	return r, nil
}

// KnownStateID checks a wire ID against the pinned vanilla palette in its
// negotiated representation. Custom pack states may be absent from this table.
func KnownStateID(protocolID int32, id uint32, hashed bool) (bool, error) {
	catalog.Do(initCatalog)
	if catalog.err != nil {
		return false, catalog.err
	}
	r := catalog.registries[protocolID]
	if r == nil {
		return false, fmt.Errorf("unsupported block registry%d", protocolID)
	}
	if hashed {
		_, ok := r.byHash[id]
		return ok, nil
	}
	return id < uint32(len(r.states)), nil
}

// Mapper is immutable after construction and belongs to one host/client pair.
// Unknown wire IDs are errors; known states absent on the client use the
// visible info_update placeholder and are counted, never silently called exact.
type Mapper struct {
	source, target *registry
	hashed         bool
	ids            map[uint32]uint32
	fallbacks      int
	// fallbackID: the block shown in place of an unknown ID (usually a custom block from the
	// host's resource pack). Returning a placeholder instead of an error keeps garbage IDs away
	// from the client.
	fallbackID uint32
	// skippedCustom: number of pack-custom blocks that could not be hashed (nested properties);
	// they render as the placeholder rather than killing the session.
	skippedCustom int
	// targetProto: protocol of the destination codec. Needed to pick the destination sub-chunk
	// format (2193 needs v9; a 1.26.30 host sends v8).
	targetProto int32
}

func New(source, target int32, hashed bool, custom []protocol.BlockEntry) (*Mapper, error) {
	catalog.Do(initCatalog)
	if catalog.err != nil {
		return nil, catalog.err
	}
	src, dst := catalog.registries[source], catalog.registries[target]
	if src == nil || dst == nil {
		return nil, fmt.Errorf("unsupported block registry%d ->%d", source, target)
	}
	if !hashed && len(custom) > 0 {
		return nil, fmt.Errorf("custom block runtime-ID ordering cannot be inferred")
	}
	m := &Mapper{source: src, target: dst, hashed: hashed, ids: map[uint32]uint32{}, targetProto: target}
	fallbackKey, _ := mapping.StateKey("minecraft:info_update", nil)
	fallback, ok := dst.byKey[fallbackKey]
	if !ok {
		return nil, fmt.Errorf("target registry has no info_update placeholder")
	}
	m.fallbackID = fallback
	if hashed {
		m.fallbackID = dst.hashes[fallback]
	}
	for i := range src.states {
		canonical := uint32(i)
		if source != 2193 {
			var ok bool
			canonical, ok = catalog.mappers[source].TargetToNative(uint32(i))
			if !ok {
				return nil, fmt.Errorf("unmapped source block%d", i)
			}
		}
		out, exact := canonical, true
		if target != 2193 {
			var valid bool
			out, valid, exact = catalog.mappers[target].MapNative(canonical)
			if !valid {
				return nil, fmt.Errorf("canonical block%d out of range", canonical)
			}
		}
		if !exact {
			out = fallback
			m.fallbacks++
		}
		in := uint32(i)
		if hashed {
			in = src.hashes[i]
			out = dst.hashes[out]
		}
		m.ids[in] = out
	}
	skippedCustom := 0
	for _, entry := range custom {
		hash, err := StateHash(entry.Name, entry.Properties)
		if err != nil {
			// ★ Resource-pack custom blocks often carry nested properties (`components` is a map)
			// that StateHash does not support yet. This error used to kill the WHOLE session
			// ("configure block translation: unsupported state value components").
			// Now the entry is skipped, so that block falls through to the placeholder via Map().
			skippedCustom++
			continue
		}
		key, err := mapping.StateKey(entry.Name, entry.Properties)
		if err != nil {
			skippedCustom++
			continue
		}
		if existing, ok := src.byHash[hash]; ok {
			state := src.states[existing]
			existingKey, _ := mapping.StateKey(state.Name, state.Properties)
			if key != existingKey {
				return nil, fmt.Errorf("custom state hash collision with source vanilla registry")
			}
			continue // Vanilla declarations do not override the validated mapping.
		}
		if existing, ok := dst.byHash[hash]; ok {
			state := dst.states[existing]
			existingKey, _ := mapping.StateKey(state.Name, state.Properties)
			if key != existingKey {
				return nil, fmt.Errorf("custom state hash collision with target vanilla registry")
			}
		}
		// The same explicit typed state definition is forwarded in StartGame.
		// Preserve its hash in both directions; resource-pack rendering support
		// remains a client capability, not a reason to silently replace the ID.
		m.ids[hash] = hash
	}
	if skippedCustom > 0 {
		m.skippedCustom = skippedCustom
	}
	return m, nil
}
func (m *Mapper) Map(id uint32) (uint32, error) {
	v, ok := m.ids[id]
	if ok {
		return v, nil
	}
	// ★ Unknown ID = a custom block from the host's resource pack (absent from the vanilla palette).
	// This used to return an error, so the packet was passed through with the host's ID and a
	// 1.26.51 client received an ID that does not exist and disconnected. Now it maps to a visible
	// placeholder (info_update).
	m.fallbacks++
	return m.fallbackID, nil
}
func (m *Mapper) SourceCount() int   { return len(m.source.states) }
func (m *Mapper) FallbackCount() int { return m.fallbacks }

// SkippedCustom returns the number of pack-custom blocks skipped because their properties could
// not be hashed.
func (m *Mapper) SkippedCustom() int { return m.skippedCustom }
func (m *Mapper) SourceID(name string, props map[string]any) (uint32, bool) {
	key, err := mapping.StateKey(name, props)
	if err != nil {
		return 0, false
	}
	id, ok := m.source.byKey[key]
	if ok && m.hashed {
		id = m.source.hashes[id]
	}
	return id, ok
}
