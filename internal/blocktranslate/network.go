package blocktranslate

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// Block translation is best-effort: an unfamiliar payload must NEVER kill the session. A client has
// spawned (log 22:04:25: PlayerAuthInput/SubChunkRequest/Interact) and then died only because of
// "subchunk14 has 58 unparsed bytes". Now an unknown payload is logged (throttled) and the host's
// packet is passed through unchanged — the blocks may be wrong, but the player stays in the world.
// DumpSubchunkPath: when non-empty, write the FIRST sub-chunk payload that does not parse fully to
// this file, so the real v8/v9 layout of the host can be inspected in hex.
var DumpSubchunkPath string

var (
	translateWarnMu    sync.Mutex
	translateWarnLast  time.Time
	translateWarnCount int
	translateDumpMu    sync.Mutex
	translateDumped    bool
)

func warnTranslateThrottled(msg string, args ...any) {
	translateWarnMu.Lock()
	defer translateWarnMu.Unlock()
	translateWarnCount++
	if time.Since(translateWarnLast) < 30*time.Second {
		return
	}
	translateWarnLast = time.Now()
	suppressed := translateWarnCount - 1
	translateWarnCount = 0
	slog.Warn(msg, append([]any{"suppressed", suppressed}, args...)...)
}

// TranslateInteractions maps the clicked block ID back towards the host. Item
// network IDs and inventory registries are a separate concern and stay intact.
func (m *Mapper) TranslateInteractions(pk packet.Packet) (packet.Packet, error) {
	translate := func(tx protocol.UseItemTransactionData) (protocol.UseItemTransactionData, error) {
		if tx.ActionType != protocol.UseItemActionClickBlock && tx.ActionType != protocol.UseItemActionBreakBlock {
			return tx, nil
		}
		id, err := m.Map(tx.BlockRuntimeID)
		if err != nil {
			return tx, err
		}
		tx.BlockRuntimeID = id
		return tx, nil
	}
	switch p := pk.(type) {
	case *packet.InventoryTransaction:
		if tx, ok := p.TransactionData.(*protocol.UseItemTransactionData); ok {
			mapped, err := translate(*tx)
			if err != nil {
				return nil, err
			}
			x := *p
			x.TransactionData = &mapped
			return &x, nil
		}
	case *packet.PlayerAuthInput:
		if tx, ok := p.ItemInteractionData.Value(); ok {
			mapped, err := translate(tx)
			if err != nil {
				return nil, err
			}
			x := *p
			x.ItemInteractionData = protocol.Option(mapped)
			return &x, nil
		}
	}
	return pk, nil
}

// rewriteSubChunk changes palette values, retaining the packed indices and
// layer/Y metadata. Returns consumed bytes so inline LevelChunk tails remain
// untouched. Both v8 and v9 have a storage count; only v9 has a Y byte.
// consumeTail: in a `SubChunk` packet payload, the layers are followed by **NBT block entities**
// (measured on a real host: 510 bytes of "Name=minecraft:wheat_seeds … id=Barrel") which must be
// copied verbatim. For a sub-chunk inlined in a LevelChunk the tail must NOT be consumed — that
// tail belongs to the whole chunk (biomes/border/tile entities) and the caller handles it.
func rewriteSubChunk(data []byte, mapID func(uint32) (uint32, error), consumeTail bool) ([]byte, int, error) {
	r := bytes.NewReader(data)
	var out bytes.Buffer
	v, err := r.ReadByte()
	if err != nil || (v != 8 && v != 9) {
		return nil, 0, fmt.Errorf("unsupported subchunk version%d", v)
	}
	out.WriteByte(v)
	layers, err := r.ReadByte()
	if err != nil {
		return nil, 0, err
	}
	out.WriteByte(layers)
	if v == 9 {
		y, err := r.ReadByte()
		if err != nil {
			return nil, 0, err
		}
		out.WriteByte(y)
	}
	readVar := func() (uint32, error) {
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return 0, err
		}
		if n > 0xffffffff {
			return 0, fmt.Errorf("varint32 overflow")
		}
		u := uint32(n)
		return uint32(int32(u>>1) ^ -int32(u&1)), nil
	}
	for layer := 0; layer < int(layers); layer++ {
		h, err := r.ReadByte()
		if err != nil {
			return nil, 0, err
		}
		out.WriteByte(h)
		bits := h >> 1
		if bits == 127 {
			continue
		}
		if h&1 == 0 {
			return nil, 0, fmt.Errorf("non-network block palette")
		}
		words := 0
		switch bits {
		case 0:
		case 1, 2, 3, 4, 5, 6, 8, 16:
			perWord := 4096 / (32 / int(bits))
			if 4096%(32/int(bits)) != 0 {
				perWord++
			}
			words = perWord
		default:
			return nil, 0, fmt.Errorf("unsupported palette bit width%d", bits)
		}
		wordBytes := words * 4
		if wordBytes > r.Len() {
			return nil, 0, fmt.Errorf("truncated packed indices")
		}
		start := len(data) - r.Len()
		out.Write(data[start : start+wordBytes])
		_, _ = r.Seek(int64(wordBytes), 1)
		count := uint32(1)
		if bits != 0 {
			count, err = readVar()
			if err != nil || count == 0 || count > 1<<bits {
				return nil, 0, fmt.Errorf("invalid palette count%d: %v", count, err)
			}
			_ = protocol.WriteVarint32(&out, int32(count))
		}
		for i := uint32(0); i < count; i++ {
			id, err := readVar()
			if err != nil {
				return nil, 0, err
			}
			mapped, err := mapID(id)
			if err != nil {
				return nil, 0, err
			}
			_ = protocol.WriteVarint32(&out, int32(mapped))
		}
	}
	if consumeTail {
		// A SubChunk payload includes trailing NetworkLittleEndian block entity
		// compounds (Dragonfly session/chunk.go). Palette rewriting must retain
		// those bytes; parser success must not silently erase world data.
		consumed := len(data) - r.Len()
		out.Write(data[consumed:])
		return out.Bytes(), len(data), nil
	}
	return out.Bytes(), len(data) - r.Len(), nil
}

// upgradeSubChunkV9 inserts the Y byte after the storage count, turning a v8 payload into v9.
// v8 = [8][layers][layer...]  ·  v9 = [9][layers][Y][layer...]
func upgradeSubChunkV9(p []byte, y int8) []byte {
	if len(p) < 2 || p[0] != 8 {
		return p
	}
	out := make([]byte, 0, len(p)+1)
	out = append(out, 9, p[1], byte(y))
	out = append(out, p[2:]...)
	return out
}

// Translate handles the block palette and dynamic block update surfaces.
// It never mutates an input packet. Cache-backed data is rejected rather than
// forwarding stale blob hashes; the host dialer must negotiate cache disabled.
func (m *Mapper) Translate(pk packet.Packet) (packet.Packet, error) {
	switch p := pk.(type) {
	case *packet.LevelChunk:
		if p.CacheEnabled {
			warnTranslateThrottled("xvrelay: LevelChunk has a blob cache - skipping translation, keeping the host payload")
			return pk, nil
		}
		if _, requestMode := p.SubChunkLimit.Value(); requestMode || p.SubChunkCount >= 0xfffffffe {
			return pk, nil
		}
		if p.SubChunkCount > 64 {
			warnTranslateThrottled("xvrelay: abnormal inline subchunk count - skipping translation", "count", p.SubChunkCount)
			return pk, nil
		}
		x := *p
		var out bytes.Buffer
		offset := 0
		for i := uint32(0); i < p.SubChunkCount; i++ {
			converted, n, err := rewriteSubChunk(p.RawPayload[offset:], m.Map, false)
			if err != nil {
				warnTranslateThrottled("xvrelay: unexpected inline subchunk - skipping translation, keeping the host payload", "index", i, "error", err)
				return pk, nil
			}
			offset += n
			out.Write(converted)
		}
		out.Write(p.RawPayload[offset:])
		x.RawPayload = out.Bytes()
		return &x, nil
	case *packet.SubChunk:
		if p.CacheEnabled {
			warnTranslateThrottled("xvrelay: SubChunk has a blob cache - skipping translation, keeping the host payload")
			return pk, nil
		}
		x := *p
		x.SubChunkEntries = append([]protocol.SubChunkEntry(nil), p.SubChunkEntries...)
		for i := range x.SubChunkEntries {
			e := &x.SubChunkEntries[i]
			if e.Result != protocol.SubChunkResultSuccess {
				continue
			}
			raw, ok := e.RawPayload.Value()
			if !ok {
				warnTranslateThrottled("xvrelay: subchunk has no payload - dropping packet", "index", i)
				return nil, nil
			}
			converted, n, err := rewriteSubChunk(raw, m.Map, true)
			if err != nil {
				warnTranslateThrottled("xvrelay: unexpected subchunk - dropping packet, host IDs not sent",
					"index", i, "len", len(raw), "error", err)
				return nil, nil
			}
			if n != len(raw) {
				// ★ Do NOT send the host payload to the client: 1.26.30 block IDs do not exist
				// in the 1.26.51 registry, so the client disconnects (log 23:10:06: died 0.13 s
				// after spawn, right after subchunk index=7 with 179 bytes left over).
				// Dropping the whole packet leaves that area as air, but the session survives.
				warnTranslateThrottled("xvrelay: subchunk has leftover bytes - DROPPING packet, host IDs not sent",
					"index", i, "len", len(raw), "parsed", n, "unparsed", len(raw)-n)
				return nil, nil
			}
			// ★ Upgrade v8 -> v9 for a newer client.
			// A 1.26.30 host sends sub-chunks as **v8** (no Y byte); a 1.26.51 client needs **v9**
			// (a Y byte right after the storage count). Bedrock can read v1/v8/v9 but always writes
			// v9 back, so newer clients only accept v9.
			if m.targetProto == 2193 {
				y := int8(int32(p.Position[1]) + int32(e.Offset[1]))
				converted = upgradeSubChunkV9(converted, y)
			}
			captureFirstSubChunk(raw, converted)
			e.RawPayload = protocol.Option(converted)
		}
		return &x, nil
	case *packet.UpdateBlock:
		x := *p
		id, err := m.Map(p.NewBlockRuntimeID)
		if err != nil {
			return nil, err
		}
		x.NewBlockRuntimeID = id
		return &x, nil
	case *packet.UpdateBlockSynced:
		x := *p
		id, err := m.Map(p.NewBlockRuntimeID)
		if err != nil {
			return nil, err
		}
		x.NewBlockRuntimeID = id
		return &x, nil
	case *packet.UpdateSubChunkBlocks:
		x := *p
		x.Blocks = append([]protocol.BlockChangeEntry(nil), p.Blocks...)
		x.Extra = append([]protocol.BlockChangeEntry(nil), p.Extra...)
		for _, entries := range [][]protocol.BlockChangeEntry{x.Blocks, x.Extra} {
			for i := range entries {
				id, err := m.Map(entries[i].BlockRuntimeID)
				if err != nil {
					return nil, err
				}
				entries[i].BlockRuntimeID = id
			}
		}
		return &x, nil
	default:
		return pk, nil
	}
}
