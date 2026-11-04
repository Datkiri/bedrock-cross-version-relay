package blocktranslate

import (
	"crypto/sha256"
	"fmt"
	data2169 "github.com/shawtymarco/go-multiversion/data/v2169"
	"github.com/shawtymarco/go-multiversion/mapping"
	"testing"
)

func TestPinnedRegistriesAndAllPairs(t *testing.T) {
	for _, source := range []int32{1001, 2168, 2169, 2193} {
		for _, target := range []int32{1001, 2168, 2169, 2193} {
			m, err := New(source, target, true, nil)
			if err != nil {
				t.Fatalf("%d -> %d: %v", source, target, err)
			}
			for _, name := range []string{"minecraft:air", "minecraft:stone", "minecraft:grass_block", "minecraft:tnt"} {
				id, ok := m.SourceID(name, nil)
				if !ok {
					continue
				} // Some blocks need explicit properties.
				got, err := m.Map(id)
				if err != nil {
					t.Fatal(err)
				}
				if source == target && got != id {
					t.Fatalf("identity changed %s", name)
				}
			}
			t.Logf("%d -> %d states=%d substituted=%d", source, target, m.SourceCount(), m.FallbackCount())
		}
	}
}

func TestSnapshotHashesAnd2169SharedSchema(t *testing.T) {
	for id, want := range map[int32]string{1001: "73ef6dadfa16dd52319493c8112ecb13a020989cf04452f76ff4a8a250a29fea", 2168: "26826e766aa0916d757706adbbb53da0f548308a14ab0ff389db9dec9b297aca", 2193: "54c9bed0bef30aadc316b21c6e82b5ac407c5c6e351b9d9690aa4e9a49fec912"} {
		data, err := palettes.ReadFile(fmt.Sprintf("data/%d.nbt", id))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
			t.Fatalf("snapshot%d checksum mismatch", id)
		}
	}
	r, err := loadRegistry(2168)
	if err != nil {
		t.Fatal(err)
	}
	states, err := data2169.BlockStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != len(r.states) {
		t.Fatalf("2168/2169 block schema counts differ%d/%d", len(r.states), len(states))
	}
	for _, s := range states {
		key, _ := mapping.StateKey(s.Name, s.Properties)
		if _, ok := r.byKey[key]; !ok {
			t.Fatalf("2169 state absent from2168 baseline: %s", key)
		}
	}
}

func TestEveryIdentityAndVisibleFallback(t *testing.T) {
	for _, id := range []int32{1001, 2168, 2169, 2193} {
		for _, hashed := range []bool{false, true} {
			m, err := New(id, id, hashed, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i := range m.source.states {
				in := uint32(i)
				if hashed {
					in = m.source.hashes[i]
				}
				out, err := m.Map(in)
				if err != nil || out != in {
					t.Fatalf("identity%d changed state%d", id, i)
				}
			}
		}
	}
	m, _ := New(2193, 1001, true, nil)
	seen := 0
	for in, out := range m.ids {
		src := m.source.states[m.source.byHash[in]]
		dst := m.target.states[m.target.byHash[out]]
		if dst.Name == "minecraft:info_update" && src.Name != "minecraft:info_update" {
			seen++
		}
	}
	if seen == 0 {
		t.Fatal("no visible fallback found for unsupported new states")
	}
}
