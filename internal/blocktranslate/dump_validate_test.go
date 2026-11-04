package blocktranslate

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"testing"
)

// TestDumpPaletteMapsInto2193 verifies offline that every source hash (1001) in a real payload
// maps to a hash that actually EXISTS in the 2193 registry. If not, the client cannot resolve the
// block ⇒ suspected disconnect right after spawn.
func TestDumpPaletteMapsInto2193(t *testing.T) {
	const path = "../../log/subchunk-dump.bin"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no dump available: %v", err)
	}
	if len(data) < 8 {
		t.Skip("dump too short")
	}
	r := bytes.NewReader(data)
	ver, _ := r.ReadByte()
	layers, _ := r.ReadByte()
	if ver == 9 {
		_, _ = r.ReadByte() // Y
	}
	_ = layers
	head, _ := r.ReadByte()
	bits := head >> 1
	per := 4096 / (32 / int(bits))
	if 4096%(32/int(bits)) != 0 {
		per++
	}
	_, _ = r.Seek(int64(per*4), 1)
	readVar := func() (uint32, error) {
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return 0, err
		}
		u := uint32(n)
		return uint32(int32(u>>1) ^ -int32(u&1)), nil
	}
	cnt, err := readVar()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	m, err := New(1001, 2193, true, nil)
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	dst := m.target
	var missing, ok int
	var samples []string
	for i := uint32(0); i < cnt; i++ {
		src, err := readVar()
		if err != nil {
			t.Fatalf("palette %d: %v", i, err)
		}
		out, err := m.Map(src)
		if err != nil {
			missing++
			continue
		}
		if _, found := dst.byHash[out]; found {
			ok++
		} else {
			missing++
			if len(samples) < 5 {
				samples = append(samples, fmt.Sprintf("%d->%#x", src, out))
			}
		}
	}
	t.Logf("palette=%d ok=%d missing=%d samples=%v", cnt, ok, missing, samples)
	if missing > 0 {
		t.Errorf("%d/%d translated hashes are NOT in the 2193 registry", missing, cnt)
	}
}
