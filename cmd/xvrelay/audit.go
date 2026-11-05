package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"bedrock-cross-version-relay/internal/blocktranslate"
)

// audit.go — measure the terrain payload GOING OUT to the client instead of guessing.
//
// Background: a 1.26.51 client (protocol 2193) spawns and then dies exactly 0.48 s after spawn
// under EVERY configuration tried (log 23:29:56 died at 0.489 s; 23:37:01 died at 0.483 s and
// 0.483 s), while a 1.26.32 client (1001, same host, same relay) plays for 1m24s normally.
// Auditor parses payload independently of the translator and checks IDs using
// the session's negotiated hash/runtime representation. IDs absent from the
// pinned vanilla snapshot may be custom states; this alone cannot prove a crash.

var (
	auditMu     sync.Mutex
	auditSeen   = map[string]int{}
	auditBadIDs int
)

func auditEnabled() bool { return auditOn }

func auditKey(k string) bool {
	auditMu.Lock()
	defer auditMu.Unlock()
	if auditSeen[k] >= 3 {
		return false
	}
	auditSeen[k]++
	return true
}

// subChunkFacts is the parse result of one subchunk payload.
type subChunkFacts struct {
	Version  byte
	Layers   byte
	Y        int8
	Bits     []byte
	Counts   []int
	IDs      []uint32
	Parsed   int
	Total    int
	HasYByte bool
	Err      error
}

// parseSubChunkPayload reads the payload using the real layout:
//
//	u8  version (8 or 9)
//	u8  storage count
//	i8  Y  (ONLY when version == 9)
//	per storage:
//	  u8  header  -> bits = header>>1, bit0 = network palette
//	  u32 x words  (words = ceil(4096 / floor(32/bits)))
//	  varint zigzag palette count
//	  palette count × varint zigzag state ID
//
// Whatever remains after the storages is NBT block-entity data (1.26.30 hosts have it; we cut it).
func parseSubChunkPayload(p []byte) subChunkFacts {
	f := subChunkFacts{Total: len(p)}
	i := 0
	if len(p) < 3 {
		f.Err = fmt.Errorf("too short (%d bytes)", len(p))
		return f
	}
	f.Version = p[i]
	i++
	f.Layers = p[i]
	i++
	if f.Version == 9 {
		if i >= len(p) {
			f.Err = fmt.Errorf("missing Y byte")
			return f
		}
		f.Y = int8(p[i])
		f.HasYByte = true
		i++
	}
	zig := func() (uint32, error) {
		u, n := binary.Uvarint(p[i:])
		if n <= 0 {
			return 0, fmt.Errorf("malformed varint @%d", i)
		}
		i += n
		if u > 0xffffffff {
			return 0, fmt.Errorf("varint overflow @%d", i)
		}
		v := uint32(u)
		return uint32(int32(v>>1) ^ -int32(v&1)), nil
	}
	for l := 0; l < int(f.Layers); l++ {
		if i >= len(p) {
			f.Err = fmt.Errorf("ran out of bytes at layer %d header", l)
			return f
		}
		h := p[i]
		i++
		bits := h >> 1
		f.Bits = append(f.Bits, bits)
		if bits == 127 {
			f.Counts = append(f.Counts, 0)
			continue
		}
		words := 0
		switch bits {
		case 0:
		case 1, 2, 3, 4, 5, 6, 8, 16:
			per := 32 / int(bits)
			words = 4096 / per
			if 4096%per != 0 {
				words++
			}
		default:
			f.Err = fmt.Errorf("bits=%d is not valid", bits)
			return f
		}
		i += words * 4
		if i > len(p) {
			f.Err = fmt.Errorf("packed indices overflow (need %d, have %d)", words*4, len(p)-(i-words*4))
			return f
		}
		count := uint32(1)
		if bits != 0 {
			c, err := zig()
			if err != nil {
				f.Err = err
				return f
			}
			count = c
		}
		f.Counts = append(f.Counts, int(count))
		if count > 1<<bits && bits != 0 {
			f.Err = fmt.Errorf("palette count %d > 2^%d", count, bits)
			return f
		}
		for k := uint32(0); k < count; k++ {
			id, err := zig()
			if err != nil {
				f.Err = err
				return f
			}
			f.IDs = append(f.IDs, id)
		}
	}
	f.Parsed = i
	return f
}

// auditSubChunk logs the structure of each entry in the SubChunk packet sent to the client.
func auditSubChunk(pk *packet.SubChunk, targetProto int32, hashed bool) {
	if !auditEnabled() {
		return
	}
	var regErr error
	slog.Info("xvrelay: AUDIT SubChunk",
		"cache", pk.CacheEnabled, "dim", pk.Dimension, "pos", pk.Position,
		"entries", len(pk.SubChunkEntries), "hashIDs", hashed)
	for idx, e := range pk.SubChunkEntries {
		raw, ok := e.RawPayload.Value()
		line := []any{
			"idx", idx, "off", e.Offset, "result", e.Result,
			"hm", e.HeightMapType, "rhm", e.RenderHeightMapType,
			"payload", len(raw), "has", ok,
		}
		// Abnormally large payload (the column record at offset 24 returned a 1.9 MB entry)
		// ⇒ dump it to disk once for offline analysis instead of guessing from the log.
		if len(raw) > 100_000 {
			line = append(line, "dumped", dumpBigPayload(raw, idx))
		}
		if !ok || e.Result != protocol.SubChunkResultSuccess {
			slog.Info("xvrelay: AUDIT entry", line...)
			continue
		}
		f := parseSubChunkPayload(raw)
		line = append(line, "ver", f.Version, "layers", f.Layers, "y", f.Y,
			"bits", f.Bits, "counts", f.Counts, "parsed", f.Parsed,
			"tail", f.Total-f.Parsed, "ids", len(f.IDs))
		if f.Err != nil {
			slog.Warn("xvrelay: AUDIT entry PARSE ERROR", append(line, "error", f.Err)...)
			continue
		}
		if f.Parsed != f.Total {
			tail := bytes.NewBuffer(raw[f.Parsed:])
			entities := 0
			var tailErr error
			var entitySizes []int
			var entityIDs []string
			for tail.Len() > 0 {
				var entity map[string]any
				start := tail.Len()
				tailErr = nbt.NewDecoderWithEncoding(tail, nbt.NetworkLittleEndian).Decode(&entity)
				if tailErr != nil {
					break
				}
				entities++
				entitySizes = append(entitySizes, start-tail.Len())
				if len(entityIDs) < 4 {
					if s, ok := entity["id"].(string); ok {
						entityIDs = append(entityIDs, s)
					} else {
						entityIDs = append(entityIDs, "<no id>")
					}
				}
			}
			if tailErr != nil {
				slog.Warn("xvrelay: AUDIT invalid block entity NBT", append(line, "error", tailErr)...)
				continue
			}
			line = append(line, "blockEntities", entities,
				"entitySizes", entitySizes, "entityIDs", entityIDs,
				"tailHeadHex", hexHead(raw[f.Parsed:], 48))
		}
		minID, maxID := uint32(0xffffffff), uint32(0)
		bad := 0
		for _, id := range f.IDs {
			if id < minID {
				minID = id
			}
			if id > maxID {
				maxID = id
			}
			found, err := blocktranslate.KnownStateID(targetProto, id, hashed)
			if err != nil {
				regErr = err
			} else if !found {
				bad++
			}
		}
		if len(f.IDs) == 0 {
			minID = 0
		}
		line = append(line, "minID", minID, "maxID", maxID, "badID", bad)
		if bad > 0 || regErr != nil {
			auditMu.Lock()
			auditBadIDs += bad
			auditMu.Unlock()
			slog.Warn("xvrelay: AUDIT palette contains IDs absent from pinned vanilla registry",
				append(line, "registryErr", regErr)...)
		} else {
			slog.Info("xvrelay: AUDIT entry OK", line...)
		}
	}
}

// hexHead returns the hex of the first n bytes — a quick fingerprint for the NBT tail.
func hexHead(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return hex.EncodeToString(b)
}

var bigDumpOnce sync.Once

// dumpBigPayload writes a >100KB SubChunk payload to log/ ONCE for offline analysis.
func dumpBigPayload(raw []byte, idx int) string {
	path := ""
	bigDumpOnce.Do(func() {
		dir := "log"
		path = filepath.Join(dir, fmt.Sprintf("big-subchunk-%s-idx%d.bin", time.Now().Format("150405"), idx))
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			path = "write error: " + err.Error()
		}
	})
	return path
}

// auditLevelChunk logs the LevelChunk shell sent to the client (request-mode or inline).
func auditLevelChunk(pk *packet.LevelChunk, targetProto int32) {
	if !auditEnabled() {
		return
	}
	lim, hasLim := pk.SubChunkLimit.Value()
	k := fmt.Sprintf("lc-%d-%v-%d", pk.SubChunkCount, hasLim, len(pk.RawPayload))
	if !auditKey(k) {
		return
	}
	slog.Info("xvrelay: AUDIT LevelChunk",
		"pos", pk.Position, "dim", pk.Dimension,
		"subChunkCount", pk.SubChunkCount, "limit", lim, "hasLimit", hasLim,
		"cache", pk.CacheEnabled, "blobs", len(pk.BlobHashes),
		"payload", len(pk.RawPayload), "target", targetProto)
}

var auditOn bool

// ── Round-trip check: encode, then decode again with the VERY codec that sends to the client ──
//
// Why: the Windows Event Log has NO crash record around 23:37:21 ⇒ the 1.26.51 client did not
// crash, it DISCONNECTED on purpose. A Bedrock client does that when it receives a packet it
// fails to decode (an ID it knows, a payload it cannot read). The relay encodes with the 2193
// codec so it never sees the error itself — but if decoding again leaves bytes over or short,
// the client is off by exactly the same amount.
//
// This is a real oracle: the same Marshal, run in both directions.

// targetShieldID returns the runtime ID of minecraft:shield in the client's
// protocol table. Protocol adapters append an 8-byte BlockingTick for items
// whose NetworkID equals ShieldID(), so a verify round-trip that hard-codes 0
// is blind to the exact packets a real client fails to decode.
func targetShieldID(proto minecraft.Protocol) int32 {
	// Keep one source of truth with the relay's listener-side seeding.
	return clientShieldID(proto.ID())
}

func verifyRoundTrip(pk packet.Packet, proto minecraft.Protocol) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("decode panic: %v", r)
		}
	}()
	shield := targetShieldID(proto)
	// Mirror the request-mode boundary before testing the target codec. Do not
	// mutate the canonical packet which the real client converter will also use.
	var encoded []packet.Packet
	if chunk, ok := pk.(*packet.LevelChunk); ok && proto.ID() == 1001 {
		encoded = []packet.Packet{legacy1001LevelChunkPacket(chunk)}
	} else {
		if chunk, ok := pk.(*packet.LevelChunk); ok && chunk.SubChunkCount >= 0xfffffffe {
			cloned := *chunk
			cloned.SubChunkCount = 0
			pk = &cloned
		}
		encoded = proto.ConvertFromLatest(pk, nil)
	}
	for _, wire := range encoded {
		var buf bytes.Buffer
		wire.Marshal(proto.NewWriter(&buf, shield))
		raw := buf.Bytes()
		// A sentinel avoids the Reader.ByteSlice zero-length EOF artifact.
		probe := make([]byte, len(raw)+1)
		copy(probe, raw)
		rd := bytes.NewReader(probe)
		ctor := proto.Packets(false)[wire.ID()]
		if ctor == nil {
			return fmt.Errorf("codec%d has no packet%d", proto.ID(), wire.ID())
		}
		clone := ctor()
		clone.Marshal(proto.NewReader(rd, shield, false))
		if rd.Len() != 1 {
			return fmt.Errorf("decode consumed %d/%d bytes (%d left over)", len(raw)+1-rd.Len(), len(raw), 1-rd.Len())
		}
	}
	return nil
}

var verifyOn bool

var (
	verifyMu    sync.Mutex
	verifyFails = map[string]int{}
)

func verifyPacket(pk packet.Packet, proto minecraft.Protocol) bool {
	if !verifyOn {
		return true
	}
	// ★ 5 packets use protocol.IO.Bytes(): they WRITE raw bytes and READ to the end of the
	// buffer ⇒ the round-trip always has exactly the sentinel left over. This is a Go codec
	// artifact, NOT a broken payload (the client parses NBT with its own parser). Skip them so
	// the oracle does not raise false alarms.
	switch pk.(type) {
	case *packet.LevelEventGeneric, *packet.AvailableActorIdentifiers,
		*packet.UpdateEquip, *packet.UpdateTrade, *packet.Unknown:
		return true
	}
	if err := verifyRoundTrip(pk, proto); err != nil {
		name := fmt.Sprintf("%T", pk)
		verifyMu.Lock()
		verifyFails[name]++
		n := verifyFails[name]
		verifyMu.Unlock()
		if n <= 5 {
			slog.Error("xvrelay: VERIFY target codec round-trip failed",
				"type", name, "id", pk.ID(), "error", err, "count", n)
		}
		return false
	}
	return true
}
