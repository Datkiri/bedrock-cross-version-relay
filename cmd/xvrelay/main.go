// Command xvrelay is an EXPERIMENTAL CROSS-VERSION RELAY.
//
// Goal: relay and TRANSLATE the protocol between a client and a LAN world, in both directions:
//
//	OLD client  (1.26.33 = 1001, 1.26.45 = 2169) -> NEW host (1.26.51 = 2193)
//	NEW client  (1.26.51 = 2193)                 -> OLD host (1.26.33 = 1001)
//
// Selected with `-host-proto` (default **auto** = read the host protocol from LAN discovery). If the
// ServerData is v5/v6 — no Protocol field — then `dialHost` probes 2193 -> 2169 -> 1001 -> 2168 on the
// first client (a host on the wrong protocol answers PlayStatusLoginFailedServer almost immediately,
// so the probe is cheap).
//
// Architecture (same shape as a working bridge; unlike a headless client, which copies raw bytes):
//
//	client --(client protocol)--> [listener] --(host protocol)--> LAN world
//
//	gophertunnel calls ConvertToLatest when READING and ConvertFromLatest when WRITING,
//	so the relay really TRANSLATES instead of copying bytes.
//
// MULTIPLE SIMULTANEOUS CLIENTS: every session has its own `spawnCapture` + `spawnPrefixGate`
// (maps keyed by *minecraft.Conn and client address), so several versions can enter the same host at
// the same time without stepping on each other. These used to be global, which made two clients
// fight over the gate and the raw payload.
//
// Provenance: the relay/spawn code below was verified against a real client before being moved here;
// logging and diagnostics that were not needed have been dropped.
//
// KNOWN LIMITS (read before judging any result):
//   - Cross-version terrain uses internal/blocktranslate with pinned vanilla palettes.
//     Unsupported known states use a visible placeholder. Item REGISTRIES are pinned for all four
//     protocol families, and the 1001/2168/2169 codecs (used on BOTH the client and host legs)
//     are registry-aware BY DEFAULT
//     (NewWithRegistries fed the native 2193 table), so item runtime IDs inside gameplay packets
//     (InventoryContent / InventorySlot / MobEquipment / CraftingData / CreativeContent / ItemRegistry)
//     are translated in BOTH directions. `-map-gameplay-items=false` restores the old plain New()
//     codecs for A/B bisects. Measured: 259 item names have DIFFERENT 2193/2169 runtime IDs;
//     apple 882->878, and the reverse path maps 878->882. Recipes / biome schemas / entity metadata /
//     arbitrary addon/resource-pack content remain separate concerns.
//   - Registry-aware codecs expect the NATIVE 2193 item table as input. Handing them the client's
//     own table makes ValidateNativeEntries fail and the ItemRegistry packet is DROPPED.
//     `clientItemRegistry()` implements that rule and `gameDataItemsFor()` MUST run for EVERY
//     session, including same-wire ones — gating the replacement on clientProto != hostProto
//     silently killed 1001 -> 1001 sessions (measured 2026-11-05, now fixed + regression test).
//   - `-host-raknet` bypasses LAN discovery and dials the host directly over RakNet. It exists for
//     a BDS with `transport=raknet`, where a NetherNet WebRTC dial never receives CONNECTRESPONSE.
//     The client ingress is STILL RakNet-only (see README).
//   - The fork in `third_party/gophertunnel-2193` does NOT carry the `rescueDisplayName` patch the
//     bridge fork had, so the client must be signed into Xbox Live (must have a XUID). An offline
//     client is rejected at Login with "DisplayName must not be empty".
package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"bedrock-cross-version-relay/internal/blocktranslate"
	"bedrock-cross-version-relay/internal/lansignal"
	"bedrock-cross-version-relay/internal/mcport"

	"github.com/df-mc/go-nethernet"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/resource"
	v1_26_30 "github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
	v1_26_44 "github.com/shawtymarco/go-multiversion/protocols/v1_26_44"
	v1_26_45 "github.com/shawtymarco/go-multiversion/protocols/v1_26_45"
)

// ── Global state ───────────────────────────────────────────────────────────────────

var (
	// hostProtoID is the LAN world's protocol.
	//   - `-host-proto <number>` ⇒ known up front, hostProtoKnown = true
	//   - `-host-proto auto` + ServerData v7 (has Protocol) ⇒ known up front
	//   - `-host-proto auto` + ServerData v5/v6 (NO Protocol field) ⇒ unknown;
	//     dialHost probes 2193 → 2169 → 1001 → 2168 on the first client.
	hostProtoID    atomic.Int32
	hostProtoKnown atomic.Bool
	hostProtoAuto  bool
	// hostProtoMu serialises probing so two concurrent clients do not probe on top of each other.
	hostProtoMu sync.Mutex
	// hostGameVersion is the EXACT version string the host advertises (ServerData v7+).
	// Empty for older ServerData ⇒ dialHost falls back to proto.Ver().
	hostGameVersion string
	// startGameMode: "raw" = forward the host's bytes untouched (only valid when the client
	// speaks the same wire format), "rebuild" = reconstruct from GameData.
	startGameMode = "raw"
	// Vanilla hosts may omit DimensionData. Auto preserves that absence: the
	// data-driven definitions cannot override the built-in dimensions (IDs 0–2).
	dimDataMode = "auto" // auto | off | force
	// identityMode: "auto" = forward the client's REAL IdentityData to the host (name + XUID),
	// "bot" = legacy behaviour (gophertunnel fills in "Steve") — A/B testing only.
	identityMode = "auto"
	// dropChunks: drop LevelChunk/SubChunk on the way to the client — used to BISECT whether
	// the client dies because of chunks or because of another packet.
	dropChunks bool
	// dropSubChunks: drop only SubChunk (keep the LevelChunk shell) — bisect the terrain payload.
	dropSubChunks bool
	// noBlockTranslate: disable just the block palette translator (chunk framing untouched)
	// to separate "the palette rewrite is wrong" from every other failure.
	noBlockTranslate bool
	// emptyShellPayload: request-mode LevelChunk shells sent to a different-version client
	// carry an EMPTY payload.
	emptyShellPayload bool

	// noPushSubChunks: do NOT prefetch terrain on the client's behalf, let it send its own
	// SubChunkRequest. Evidence: log 23-24-31 shows C->H `SubChunkRequest=16` (the client
	// asking itself) while we still push 203 `SubChunk` ⇒ the client receives data it never
	// requested.
	noPushSubChunks bool

	// noItems: wipe GameData.Items before the listener sends StartGame ⇒ EMPTY ItemRegistry.
	// Needs its own flag because the listener emits the registry itself (conn.go startGame),
	// NOT through the relay ⇒ -dropids=162 can never reach it.
	noItems bool
	// keepHostItems: do NOT replace the ItemRegistry, keep the host's table (diagnosing maps
	// with custom items: the vanilla 2193 table has no custom items, so the client may refuse).
	keepHostItems bool

	// hostPacks: packs taken from the host, served back to the client via FetchResourcePacks.
	// A 1.26.51 client needs the map's pack (textures/models/sounds); without it a map that
	// ships a pack renders wrong or gets disconnected.
	hostPacks atomic.Pointer[[]*resource.Pack]
	// packCfg is the CONN-005 policy, set once from flags. Default is skip + no prefetch,
	// i.e. exactly the behaviour before this guard existed.
	packCfg = defaultPackPolicy()

	// dropIDs: set of packet IDs dropped on the way to the client — bisect one packet type
	// at a time.
	dropIDs map[uint32]bool
	// dumpWireIDs: diagnostics — packet IDs whose SENT-TO-CLIENT payload is written to disk
	// (so it can be decoded with the client's own codec; this is the technique that uncovered
	// the SubChunk enum bug).
	dumpWireIDs map[uint32]bool
	// dialTimeout: a host with its own resource packs must finish downloading BEFORE StartGame;
	// 30 s is not enough (log 21:35: three packs, then deadline exceeded).
	dialTimeout = 180 * time.Second

	// onlyPrefix: drop EVERY relayed H->C packet, letting only the listener's spawn prefix
	// (StartGame/DimensionData/ItemRegistry/Jigsaw/VoxelShapes/PlayStatus/…) reach the client.
	onlyPrefix bool
	// dumpStartPath: when non-empty, write the StartGame payload SENT TO THE CLIENT to a file
	// (diagnostics).
	dumpStartPath string

	// sessions maps *minecraft.Conn (the CLIENT side) → *session.
	// A map instead of a global so that CONCURRENT clients do not step on each other.
	sessions sync.Map

	// clientGates maps client address (string) → *clientGate (the spawn-prefix gate plus the
	// session's OWN log counters). The listener's PacketFunc has no *Conn argument, so the
	// lookup has to go through the address.
	//
	// ★ This race actually happened (log 13-33-24, session at 13:40): the same client
	// reconnected from the SAME address [::1]:61287 while the old session was still running.
	// The OLD session's `defer clientGates.Delete(addr)` ran after the NEW session had already
	// Store()d over the same key ⇒ it deleted the new session's gate and logger ⇒ every
	// "client sent" line for the new session vanished (the 13:40 session only kept n=1..6,
	// nothing after 13:41:39 was logged) ⇒ id=113 could not be proven from the log. The value
	// is now *clientGate and Delete only runs while it is still this session's gate.
	clientGates sync.Map
)

// clientGate is the value stored in clientGates.
type clientGate struct {
	gate *spawnPrefixGate
	// count counts client-sent packets for THIS session only (this used to be a global atomic,
	// and sessions sharing an address skewed the n= sequence in the log).
	count atomic.Int64
	// saw113 records whether the client has sent SetLocalPlayerAsInitialised — per session.
	saw113 atomic.Bool
}

// contextCause returns the root cause of a minecraft.Conn's ctx (a readable string), or
// "<not cancelled>" when the conn is still alive. Used to tell apart: go-raknet closing on its
// own (context.Canceled), our own Close (net.ErrClosed), a Disconnect packet (DisconnectError).
func contextCause(conn *minecraft.Conn) string {
	if conn == nil {
		return "<nil conn>"
	}
	c := conn.Context()
	if c == nil {
		return "<no ctx>"
	}
	if err := context.Cause(c); err != nil {
		return err.Error()
	}
	return "<not cancelled>"
}

// rhmFixedLogged: log the RenderHeightMap mirror exactly once.
var rhmFixedLogged atomic.Bool

// init sets the default host protocol (2193 = 1.26.51).
func init() { hostProtoID.Store(2193) }

// spawnCapture holds the host's RAW payloads for ONE session.
//
// GameData does NOT model enough StartGame fields (LevelID, ServerID, ScenarioID, WorldID,
// OwnerID, WorldTemplateID, EnchantmentSeed, SpawnBiomeType, UserDefinedBiomeName,
// EducationProductID, ServerAuthoritativeSound…) ⇒ rebuilding it LOSES data. Only
// raw-forwarding is faithful.
type spawnCapture struct {
	startGame       atomic.Pointer[[]byte]
	dimensionData   atomic.Pointer[[]byte]
	itemRegistry    atomic.Pointer[[]byte]
	jigsaw          atomic.Pointer[[]byte]
	voxelShapes     atomic.Pointer[[]byte]
	jigsawSent      atomic.Bool
	voxelSent       atomic.Bool
	chunkLogged     atomic.Bool
	publisherLogged atomic.Bool
	// packs is the PER-DIAL resource-pack guard (CONN-005). It is an atomic pointer
	// because dialHostWith installs it while the handshake is already running.
	packs atomic.Pointer[packGuard]
}

// reset wipes the data from the previous dial (used when auto-probing another protocol).
func (c *spawnCapture) reset() {
	c.startGame.Store(nil)
	c.dimensionData.Store(nil)
	c.itemRegistry.Store(nil)
	c.jigsaw.Store(nil)
	c.voxelShapes.Store(nil)
	c.jigsawSent.Store(false)
	c.voxelSent.Store(false)
	c.chunkLogged.Store(false)
	c.publisherLogged.Store(false)
	c.packs.Store(nil)
}

// session is the per-client state of one relayed connection.
type session struct {
	reverseBlocks *blocktranslate.Mapper
	blocks        *blocktranslate.Mapper
	capture       *spawnCapture
	gate          *spawnPrefixGate
	reqs          *subChunkRequests
	// hostProto is the REAL host protocol for this session (taken from serverConn after dial).
	// Never read the global on the hot path: two sessions may probe different results.
	hostProto int32
	// reqLogged throttles client SubChunkRequest offset logging (first 12 requests per session).
	reqLogged atomic.Int64
	// clientRequested: has the client ALREADY sent its own SubChunkRequest (id 175)?
	// Live evidence (log 17-03-14): a 2193 client asked for 16 requests × 3 neighbouring
	// offsets within 0.6 s, yet 0 of the 138 replies were for those offsets — every 24-entry
	// push was self-generated by the relay ⇒ the client was queued behind hundreds of push
	// requests, never received the ground around its feet, and took 138 packets (some 1–2.4 MB
	// because of large chest block-entity NBT) in under a second ⇒ it dropped RakNet itself.
	// Once the client can ask for itself, we must stop pushing on its behalf.
	clientRequested atomic.Bool
}

// ── GameVersion override wrapper ────────────────────────────────────────────────────

// clientVersionProtocol wraps a codec and OVERRIDES StartGame.GameVersion/BaseGameVersion with
// the client's REAL version, read from ClientData at encode time.
//
// Why this is needed: `Conn.startGame()` hardcodes `GameVersion: protocol.CurrentVersion`, and
// `protocol.CurrentVersion` is a **const** ("1.26.51") ⇒ it cannot be overridden from outside,
// and GameData has no such field. A Bedrock client refuses a server whose GameVersion is OLDER
// than its own (measured: a 1.26.52 client receiving "1.26.50" crashes itself after 0.3 s).
type clientVersionProtocol struct {
	minecraft.Protocol
	enabled bool
}

// wrapClientVersion attaches the version-override layer to a codec.
func wrapClientVersion(p minecraft.Protocol, enabled bool) minecraft.Protocol {
	return clientVersionProtocol{Protocol: p, enabled: enabled}
}

// gameVersionOverrider lets us ASK a Protocol whether it is the wrapper.
//
// Why this is needed: `listener.go` always does `append(AcceptedProtocols, proto{})` and
// `selectProtocol` picks the FIRST candidate whose ID matches. If 2193 is not among
// AcceptedProtocols, a 2193 client matches the bare `proto{}` ⇒ the wrapper disappears
// SILENTLY.
type gameVersionOverrider interface {
	OverridesGameVersion() bool
}

func (p clientVersionProtocol) OverridesGameVersion() bool { return p.enabled }

// ── Spawn-prefix gate ──────────────────────────────────────────────────────────────

// spawnPrefixGate opens the H->C stream once the client has answered RequestChunkRadius.
//
// ★ Why this is needed: `Conn.StartGame` BLOCKS until the client sends
// `SetLocalPlayerAsInitialised`. The client only sends that AFTER the world has loaded, and
// the world only reaches the client THROUGH the relay. If the H->C relay only starts after
// StartGame returns ⇒ DEADLOCK (this really crashed: the host pumped ~180 LevelChunks while
// the client received 0, then gave up after 0.35 s).
//
// But the H->C relay must not start too early either: the host's deferredPackets queue already
// holds SetSpawnPosition/LevelChunk from dial time, and sending those before StartGame kicks
// the client out.
type spawnPrefixGate struct {
	ch   chan struct{}
	once sync.Once
}

func newSpawnPrefixGate() *spawnPrefixGate {
	return &spawnPrefixGate{ch: make(chan struct{})}
}

// signal closes the gate exactly ONCE. Safe to call on a nil pointer.
func (g *spawnPrefixGate) signal() {
	if g == nil {
		return
	}
	g.once.Do(func() { close(g.ch) })
}

// done returns a channel that closes when the H->C relay may run. Nil ⇒ treated as open.
func (g *spawnPrefixGate) done() <-chan struct{} {
	if g == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return g.ch
}

// ── Raw payloads ───────────────────────────────────────────────────────────────────

// storeRaw keeps a COPY of the raw payload.
func storeRaw(dst *atomic.Pointer[[]byte], payload []byte) {
	cp := make([]byte, len(payload))
	copy(cp, payload)
	dst.Store(&cp)
}

// decodeRawStartGame decodes a raw StartGame payload.
//
// ★ `packet.Packet` has only ONE method, `Marshal(io protocol.IO)`, used for both encode and
// decode, and it does NOT return an error — the reader panics on corrupt data. `recover()` is
// mandatory.
func decodeRawStartGame(payload []byte) (sg *packet.StartGame, err error) {
	return decodeRawStartGameWith(minecraft.DefaultProtocol, payload)
}

// decodeRawStartGameWith decodes a StartGame payload with the SPECIFIED codec.
//
// ★ The sender's codec must be used: decoding a 2169/1001 payload with the native 2193 layout
// can panic or produce garbage (same reason as legacyPreludeFromHost).
func decodeRawStartGameWith(codec minecraft.Protocol, payload []byte) (sg *packet.StartGame, err error) {
	defer func() {
		if r := recover(); r != nil {
			sg, err = nil, fmt.Errorf("panic while decoding: %v", r)
		}
	}()
	mk, ok := codec.Packets(false)[packet.IDStartGame]
	if !ok {
		return nil, errors.New("packet pool has no StartGame")
	}
	pk := mk()
	pk.Marshal(codec.NewReader(bytes.NewBuffer(payload), 0, true))
	out, ok := unwrapWirePacket(pk).(*packet.StartGame)
	if !ok {
		return nil, fmt.Errorf("pool returned %T, not *packet.StartGame", pk)
	}
	return out, nil
}

// unwrapWirePacket peels go-multiversion's wrapper to get the native packet inside.
//
// ★ Why this is needed: the adapter's `Packets()` wraps EVERY packet that needs translating in
// `*translatedPacket` (field `inner`, unexported, no accessor). The wrapper's `Marshal`
// reads/writes that version's layout into `inner` — so after decoding the data IS in `inner`,
// but a type assertion to `*packet.StartGame` fails.
//
// `v1_26_45.UnwrapWirePacket` handles the 2169 wrapper; 1001's own wrapper
// (`v1_26_30.translatedPacket`) has to be read through reflect+unsafe because the field is
// unexported.
func unwrapWirePacket(pk packet.Packet) packet.Packet {
	if inner := v1_26_45.UnwrapWirePacket(pk); inner != pk {
		return inner
	}
	v := reflect.ValueOf(pk)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return pk
	}
	f := v.Elem().FieldByName("inner")
	if !f.IsValid() || f.Kind() != reflect.Interface || !f.CanAddr() {
		return pk
	}
	inner, ok := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(packet.Packet)
	if !ok {
		return pk
	}
	return inner
}

// startGameFromHostFor1001 uses the host's FULL world definition for a 1001 client, but keeps
// the fields the relay deliberately changes (spawn position, gamemode, version, registry).
func startGameFromHostFor1001(hostProtocol int32, generated *packet.StartGame, raw []byte) (*packet.StartGame, error) {
	codec, ok := resolveProto(hostProtocol)
	if !ok {
		return nil, fmt.Errorf("unknown host StartGame protocol %d", hostProtocol)
	}
	host, err := decodeRawStartGameWith(codec, raw)
	if err != nil {
		return nil, err
	}
	host.EntityUniqueID = generated.EntityUniqueID
	host.EntityRuntimeID = generated.EntityRuntimeID
	host.PlayerPosition = generated.PlayerPosition
	host.PlayerGameMode = generated.PlayerGameMode
	host.GameVersion = generated.GameVersion
	host.BaseGameVersion = generated.BaseGameVersion
	host.Blocks = generated.Blocks
	host.UseBlockNetworkIDHashes = generated.UseBlockNetworkIDHashes
	host.ServerBlockStateChecksum = generated.ServerBlockStateChecksum
	return host, nil
}

// restoreStartGameFromHost builds the client's StartGame from the host's REAL payload, keeping
// only the fields that belong to the client SESSION.
//
// ★ Why this is needed for EVERY different-wire case (not just 1001): `minecraft.GameData`
// does NOT model 9 StartGame fields — LevelID, ServerID, ScenarioID, WorldID, OwnerID,
// WorldTemplateID, ServerChunkTickRadius, EnchantmentSeed, SpawnBiomeType/UserDefinedBiomeName —
// and `Conn.startGame()` additionally hardcodes Generator=1, EducationFeaturesEnabled=true,
// MultiPlayerGame=true… ⇒ rebuilding from GameData is LOSSY. A 1.26.51 client receiving a
// StartGame with missing fields crashes right after the chunks arrive (exactly what log
// 17:02:48 shows: 441 H->C packets, then the client closes).
//
// Decoded with the HOST's codec (not native 2193) because the payload carries the host layout.
func restoreStartGameFromHost(hostCodec minecraft.Protocol, generated *packet.StartGame, raw []byte) (*packet.StartGame, error) {
	host, err := decodeRawStartGameWith(hostCodec, raw)
	if err != nil {
		return nil, err
	}
	host.EntityUniqueID = generated.EntityUniqueID
	host.EntityRuntimeID = generated.EntityRuntimeID
	host.PlayerPosition = generated.PlayerPosition
	host.PlayerGameMode = generated.PlayerGameMode
	host.GameVersion = generated.GameVersion
	host.BaseGameVersion = generated.BaseGameVersion
	// ★ Zero = "opt out" of the checksum check (exactly what the relay did for 1001 clients).
	// The host's checksum belongs to the 1.26.33 registry; a 1.26.51 client uses ITS OWN
	// registry (blocks=0 + hashIDs=true) ⇒ keeping the host checksum mismatches and the client
	// may refuse.
	host.ServerBlockStateChecksum = 0
	// ★ Our listener does NOT serve resource packs (FetchResourcePacks is never set), so the
	// client receives no pack at all. Keeping the host's true flag makes the client believe
	// "the server requires a pack" while none is available ⇒ it refuses to enter the world
	// (log 21:39:46: dead after 0.5 s, StartGame carried texturePackRequired=true because the
	// host world had three packs).
	host.TexturePackRequired = false
	host.HasLockedTexturePack = false
	// ★ Drop the host's custom block list when the client is a different version.
	// A map with a resource pack declared 91 custom blocks in `StartGame.Blocks` (1.26.30 NBT
	// components). A 1.26.51 client validates those entries ⇒ the old schema is easily refused
	// (log 22:51:40: dead after ~1.1 s, skippedCustom=91). Dropping it makes the client use its
	// own registry; those blocks were already mapped to a placeholder at the chunk layer.
	if len(host.Blocks) > 0 {
		slog.Warn("xvrelay: dropping host custom blocks for a different-version client", "count", len(host.Blocks))
		host.Blocks = nil
	}
	// Preserve the host's feature declarations. A protocol enum mismatch must
	// be fixed in the codec, not hidden by dropping world metadata.
	return host, nil
}

// ── Packet translation ─────────────────────────────────────────────────────────────

// rawForward builds a `packet.Unknown` to emit the host's RAW BYTES, or nil when the conditions
// are not met.
//
// `packet.Unknown.Marshal` = `io.Bytes(&Payload)` and `Writer.Bytes` writes raw with no length
// prefix ⇒ the bytes out are exactly the bytes the host sent.
func rawForward(clientProto int32, hostProto int32, id uint32, src *atomic.Pointer[[]byte], label string, conn *minecraft.Conn) packet.Packet {
	if startGameMode != "raw" {
		return nil
	}
	if clientProto != hostProto {
		// Different wire ⇒ must NOT emit raw (different layout). Fall back to rebuilding.
		return nil
	}
	p := src.Load()
	if p == nil || len(*p) == 0 {
		return nil
	}
	// The same wire ID does not mean the same release. A newer client may refuse a StartGame
	// carrying the host's older version string ⇒ rewrite just the two version strings and keep
	// every other field.
	if id == packet.IDStartGame {
		codec, ok := resolveProto(hostProto)
		if !ok {
			return nil
		}
		if sg, err := decodeRawStartGameWith(codec, *p); err == nil {
			if version := conn.ClientData().GameVersion; version != "" && sg.GameVersion != "" && sg.GameVersion != version {
				cloned := *sg
				cloned.GameVersion, cloned.BaseGameVersion = version, version
				// This result bypasses the outer converter. Apply the target's
				// wire wrapper here so legacy clients never receive native bytes.
				converted := codec.ConvertFromLatest(&cloned, conn)
				if len(converted) != 1 {
					return nil
				}
				return converted[0]
			}
		} else {
			return nil
		}
	}
	if (id == packet.IDDimensionData || id == packet.IDItemRegistry) &&
		conn.ClientData().GameVersion != "" && conn.ClientData().GameVersion != protocol.CurrentVersion {
		return nil
	}
	slog.Info("xvrelay: RAW-FORWARD host "+label, "len", len(*p),
		"client", conn.RemoteAddr(), "clientProto", clientProto, "hostProto", hostProto)
	return &packet.Unknown{PacketID: id, Payload: *p}
}

// preludeFromHost replaces the empty packet gophertunnel generates with the host's REAL one.
// The later copy sitting in the host's deferredPackets is skipped (sent=true).
func preludeFromHost(id uint32, raw *atomic.Pointer[[]byte], sent *atomic.Bool) ([]packet.Packet, bool) {
	p := raw.Load()
	if p == nil {
		return nil, false
	}
	if sent.CompareAndSwap(false, true) {
		return []packet.Packet{&packet.Unknown{PacketID: id, Payload: *p}}, true
	}
	return nil, true
}

// legacyPreludeFromHost: the listener generates an EMPTY prelude. For a 1001 client the host's
// bytes cannot be emitted directly when the host is on a DIFFERENT wire ⇒ decode with the HOST's
// codec, then let the target codec encode them again.
//
// ★ The HOST's pool must be used, not `packet.NewServerPool()` (native 2193): the payload is
// bytes the host sent in the HOST's layout. Decoding 2169/1001 with the 2193 layout panics or
// produces garbage.
func legacyPreludeFromHost(id uint32, raw *atomic.Pointer[[]byte], sent *atomic.Bool, fallback packet.Packet, target minecraft.Protocol, hostProtoID int32) ([]packet.Packet, bool) {
	out, ok := preludeFromHost(id, raw, sent)
	if !ok || len(out) == 0 {
		return out, ok
	}
	unknown := out[0].(*packet.Unknown)
	hostProto, okProto := resolveProto(hostProtoID)
	if !okProto {
		sent.Store(false)
		return nil, false
	}
	decode := hostProto.Packets(false)[id]
	if decode == nil {
		sent.Store(false)
		return nil, false
	}
	var decoded packet.Packet
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("decode host prelude %d: %v", id, r)
			}
		}()
		decoded = decode()
		decoded.Marshal(hostProto.NewReader(bytes.NewBuffer(unknown.Payload), 0, true))
	}()
	if err != nil {
		slog.Warn("xvrelay: cannot translate the host prelude, using the listener's own", "id", id, "error", err)
		sent.Store(false)
		return target.ConvertFromLatest(fallback, nil), true
	}
	return target.ConvertFromLatest(decoded, nil), true
}

// ConvertFromLatest is the heart of the translator.
func (p clientVersionProtocol) ConvertFromLatest(pk packet.Packet, conn *minecraft.Conn) []packet.Packet {
	if conn == nil {
		return p.Protocol.ConvertFromLatest(pk, nil)
	}
	protoID := conn.Proto().ID()

	// The session's OWN state. Missing (e.g. the conn has not registered yet) ⇒ every raw path
	// is skipped.
	var cap *spawnCapture
	hostProto := hostProtoID.Load()
	if v, ok := sessions.Load(conn); ok {
		s := v.(*session)
		cap = s.capture
		if s.hostProto != 0 {
			hostProto = s.hostProto
		}
	}

	// go-multiversion cannot represent 1001's request-mode LevelChunk: 2193 carries
	// Optional[SubChunkLimit] while 1001 uses SubChunkCount=0xffffffff. Left to the shared codec,
	// a 1.26.33 client reads a shifted offset and quits before it ever sends SubChunkRequest.
	if protoID == 1001 {
		if chunk, ok := pk.(*packet.LevelChunk); ok {
			return []packet.Packet{legacy1001LevelChunkPacket(chunk)}
		}
	}

	// ★ THE REVERSE DIRECTION: host 1001 → client 2193/2169.
	//
	// The 1001 adapter's `marshalLevelChunk` preserves the sentinel in `SubChunkCount` when
	// READING (0xFFFFFFFE = has a limit, 0xFFFFFFFF = no limit) and sets `SubChunkLimit`
	// accordingly. The 2193 codec validates `SubChunkCount <= 64` when WRITING ⇒ panic
	// "invalid value '4294967294' for level chunk sub-chunk count".
	// Normalise to the 2193 representation: count = 0 + SubChunkLimit (request mode).
	if chunk, ok := pk.(*packet.LevelChunk); ok && protoID != 1001 {
		if chunk.SubChunkCount >= 0xFFFFFFFE {
			cloned := *chunk
			cloned.SubChunkCount = 0
			// The 1001 codec already decoded the sentinel into SubChunkLimit.
			// Only normalise the count for the native envelope. Request-mode still
			// carries biome/border/block-entity data; clearing it creates a truncated
			// world payload. Cache hashes and the host's limit must stay paired with it.
			lim, has := cloned.SubChunkLimit.Value()
			// ★ TRIED AND REVERTED (27/10 18:54): lowering SubChunkLimit to 1 did NOT save the
			// client and made things worse — log 18:54-02: hostLimit=9 → sent 1, the client died
			// 4.48 s BEFORE spawn; whereas keeping limit=10 (log 18:35-59) let the client SPAWN
			// and only then die 0.6 s later. So KEEP the host's limit unchanged.
			// Keep biome/border data by default. Clearing the shell is an explicit
			// diagnostic option, not a requirement of the 2193 envelope.
			if emptyShellPayload {
				cloned.RawPayload = nil
			}
			pk = &cloned
			if cap != nil && cap.chunkLogged.CompareAndSwap(false, true) {
				slog.Info("xvrelay: LevelChunk request-mode (normalised)",
					"sentinel", chunk.SubChunkCount, "dim", cloned.Dimension,
					"limit", lim, "hadLimit", has,
					"hostCache", cloned.CacheEnabled, "hostHashes", len(cloned.BlobHashes),
					"payloadBytes", len(cloned.RawPayload))
			}
		}
	}

	// ★★ RenderHeightMapType=4 (AllCopied, no data) from a 1001 host is the suspect that was
	// nailed down by a chain of evidence: a 2193 client died 24–113 ms after the FIRST SubChunk
	// packet in five consecutive live sessions (16-48/17-03×2/17-19/17-28), and EVERY SubChunk
	// carried hm=1+data / rhm=4+no-data. A real 2193 server (dragonfly v0.11.5 chunk.go:64-130)
	// NEVER emits type 4 and ALWAYS sets both HeightMap and RenderHeightMap with the same
	// type/data. Mirror dragonfly: rhm := hm for every entry of a 2193 client.
	if sc, ok := pk.(*packet.SubChunk); ok && protoID == 2193 {
		cloned := *sc
		cloned.SubChunkEntries = append([]protocol.SubChunkEntry(nil), sc.SubChunkEntries...)
		changed := false
		for i := range cloned.SubChunkEntries {
			e := &cloned.SubChunkEntries[i]
			if e.HeightMapType != e.RenderHeightMapType {
				e.RenderHeightMapType = e.HeightMapType
				e.RenderHeightMapData = e.HeightMapData
				changed = true
			}
		}
		if changed {
			pk = &cloned
			if rhmFixedLogged.CompareAndSwap(false, true) {
				slog.Info("xvrelay: SubChunk mirror RenderHeightMap := HeightMap for a 2193 client",
					"reason", "host1001 rhm=4 (AllCopied) — a real dragonfly 2193 never emits 4")
			}
		}
	}

	if cap != nil {
		// Prelude: JigsawStructureData / VoxelShapes generated by `startGame()` are EMPTY.
		if protoID == hostProto {
			switch pk.ID() {
			case packet.IDJigsawStructureData:
				if out, ok := preludeFromHost(pk.ID(), &cap.jigsaw, &cap.jigsawSent); ok {
					return out
				}
			case packet.IDVoxelShapes:
				if out, ok := preludeFromHost(pk.ID(), &cap.voxelShapes, &cap.voxelSent); ok {
					return out
				}
			}
		}
		if protoID == 1001 {
			switch pk.ID() {
			case packet.IDJigsawStructureData:
				if out, ok := legacyPreludeFromHost(pk.ID(), &cap.jigsaw, &cap.jigsawSent, pk, p.Protocol, hostProto); ok {
					return out
				}
			case packet.IDVoxelShapes:
				if out, ok := legacyPreludeFromHost(pk.ID(), &cap.voxelShapes, &cap.voxelSent, pk, p.Protocol, hostProto); ok {
					return out
				}
			}
		}

		// Absolute priority: raw-forward when the client shares the host's wire format.
		switch pk.ID() {
		case packet.IDStartGame:
			if raw := rawForward(protoID, hostProto, packet.IDStartGame, &cap.startGame, "StartGame", conn); raw != nil {
				return []packet.Packet{raw}
			}
		case packet.IDDimensionData:
			if raw := rawForward(protoID, hostProto, packet.IDDimensionData, &cap.dimensionData, "DimensionData", conn); raw != nil {
				return []packet.Packet{raw}
			}
		case packet.IDItemRegistry:
			if raw := rawForward(protoID, hostProto, packet.IDItemRegistry, &cap.itemRegistry, "ItemRegistry", conn); raw != nil {
				return []packet.Packet{raw}
			}
		}

		// Restore the host's REAL StartGame for EVERY different-wire case (the same-wire case was
		// already handled by rawForward above). This is the only faithful path because GameData
		// is lossy.
		if sg, ok := pk.(*packet.StartGame); ok && protoID != hostProto {
			if startGameMode != "raw" {
				slog.Info("xvrelay: -startgame=rebuild ⇒ building StartGame from GameData (verified relay behaviour)",
					"clientProto", protoID, "hostProto", hostProto)
			} else if raw := cap.startGame.Load(); raw != nil {
				if protoID == 1001 {
					if restored, err := startGameFromHostFor1001(hostProto, sg, *raw); err == nil {
						pk = restored
						slog.Info("xvrelay: restored the host's StartGame for a 1001 client", "rawBytes", len(*raw))
					} else {
						slog.Warn("xvrelay: decoding the host StartGame failed, using GameData", "error", err)
					}
				} else if hostCodec, okc := resolveProto(hostProto); okc && startGameMode == "raw" {
					// ★ Evidence from the RELAY capture (which did work with a real client):
					// the relay does NOT restore the raw StartGame, it rebuilds from GameData, and
					// the 1.26.51 client spawns normally (the capture contains
					// `SetLocalPlayerAsInitialised`). The raw restore here is therefore OPTIONAL:
					// `-startgame rebuild` disables it to fall back to the exact relay behaviour.
					if restored, err := restoreStartGameFromHost(hostCodec, sg, *raw); err == nil {
						pk = restored
						exps := make([]string, 0, len(restored.Experiments))
						for _, e := range restored.Experiments {
							exps = append(exps, e.Name)
						}
						slog.Info("xvrelay: ★ restored the host's FULL StartGame",
							"rawBytes", len(*raw), "clientProto", protoID, "hostProto", hostProto,
							"levelID", restored.LevelID, "serverID", restored.ServerID,
							"worldID", restored.WorldID, "ownerID", restored.OwnerID,
							"scenarioID", restored.ScenarioID, "enchantmentSeed", restored.EnchantmentSeed,
							"generator", restored.Generator, "educationFeatures", restored.EducationFeaturesEnabled,
							"blocks", len(restored.Blocks), "hashIDs", restored.UseBlockNetworkIDHashes,
							"checksum", restored.ServerBlockStateChecksum,
							"texturePackRequired", restored.TexturePackRequired,
							"hasLockedTexturePack", restored.HasLockedTexturePack,
							"experiments", strings.Join(exps, ","),
							"gameRules", len(restored.GameRules),
							"dimension", restored.Dimension,
							"worldGameMode", restored.WorldGameMode,
							"playerGameMode", restored.PlayerGameMode,
							"clientSideGeneration", restored.ClientSideGeneration,
							"editorWorldType", restored.EditorWorldType,
							"serverChunkTickRadius", restored.ServerChunkTickRadius)
					} else {
						slog.Warn("xvrelay: decoding the host StartGame failed, using GameData", "error", err)
					}
				}
			}
		}
	}

	// Override the version with the client's REAL version.
	if p.enabled {
		if sg, ok := pk.(*packet.StartGame); ok {
			if v := conn.ClientData().GameVersion; v != "" && (sg.GameVersion != v || sg.BaseGameVersion != v) {
				cloned := *sg
				cloned.GameVersion, cloned.BaseGameVersion = v, v
				pk = &cloned
				slog.Info("xvrelay: overriding StartGame.GameVersion with the client's", "version", v)
			}
		}
	}
	return p.Protocol.ConvertFromLatest(pk, conn)
}

// legacy1001LevelChunkPacket emits LevelChunk in protocol 1001's wire layout.
// It deliberately returns `Unknown`: the destination conn must NOT run the latest->1001
// converter a second time over the request-mode sentinel.
func legacy1001LevelChunkPacket(src *packet.LevelChunk) *packet.Unknown {
	var raw bytes.Buffer
	w := protocol.NewWriter(&raw, 0)

	pos := src.Position
	dim := src.Dimension
	w.ChunkPos(&pos)
	w.Varint32(&dim)

	count := src.SubChunkCount
	limit, hasLimit := src.SubChunkLimit.Value()
	if hasLimit {
		if limit < 0 {
			count = ^uint32(0)
		} else {
			if limit > 65535 {
				panic(fmt.Errorf("protocol 1001 SubChunkLimit %d exceeds uint16", limit))
			}
			count = ^uint32(0) - 1
		}
	}
	w.Varuint32(&count)
	if count == ^uint32(0)-1 {
		highest := uint16(limit)
		w.Uint16(&highest)
	}

	if src.CacheEnabled {
		cache := true
		w.Bool(&cache)
		hashes := append([]uint64(nil), src.BlobHashes...)
		protocol.FuncSlice(w, &hashes, w.Uint64)
	} else {
		cache := false
		w.Bool(&cache)
	}

	payload := src.RawPayload
	w.ByteSlice(&payload)
	return &packet.Unknown{PacketID: packet.IDLevelChunk, Payload: raw.Bytes()}
}

// resolveProto maps a protocol id to a minecraft.Protocol.
//
//	1001 -> 1.26.30 (1.26.30–1.26.36) — WIP adapter, wire level only
//	2168 -> v1_26_44
//	2169 -> v1_26_45  ⚠️ a REAL 1.26.44 client sends 2169 (verified from logs)
//	2193 -> the native codec of the tree being built
func resolveProto(id int32) (minecraft.Protocol, bool) {
	switch id {
	case 1001:
		return proto1001(), true
	case 2168:
		return proto2168(), true
	case 2169:
		return proto2169(), true
	case 2193:
		return minecraft.DefaultProtocol, true
	}
	return nil, false
}

var (
	proto1001Once  sync.Once
	proto1001Value minecraft.Protocol
	// adapter1001HasRegistry = the 1001 adapter was built with NewWithRegistries ⇒ it maps
	// blocks/items ITSELF (ConvertFromLatest/ToLatest). Our C→H mapper must then be disabled to
	// avoid double mapping.
	adapter1001HasRegistry bool

	proto2168Once          sync.Once
	proto2168Value         minecraft.Protocol
	adapter2168HasRegistry bool

	proto2169Once          sync.Once
	proto2169Value         minecraft.Protocol
	adapter2169HasRegistry bool
)

// registryAwareCodec builds a 2168/2169 adapter that knows BOTH the native 2193
// block palette and the native 2193 item table. Without them the codec is a
// pass-through and item runtime IDs inside gameplay packets stay at the host's
// values. Measured offline (cmd/xvrelay/item_gameplay_test.go): with the native
// table the mapper builds cleanly and none of the six item-carrying packet
// families are dropped.
func registryAwareCodec(id int32) (minecraft.Protocol, error) {
	reg, err := blocktranslate.NativeRegistry(2193)
	if err != nil {
		return nil, fmt.Errorf("native 2193 block registry: %w", err)
	}
	items, err := blocktranslate.NativeItems()
	if err != nil {
		return nil, fmt.Errorf("native 2193 item registry: %w", err)
	}
	switch id {
	case 2168:
		return v1_26_44.NewWithRegistries(reg, items)
	case 2169:
		return v1_26_45.NewWithRegistries(reg, items)
	}
	return nil, fmt.Errorf("no registry-aware adapter for protocol %d", id)
}

// proto2168/proto2169 mirror proto1001: build the expensive mapper once, fall
// back to the wire-only codec if anything fails. mapGameplayItems=false keeps
// the historic behaviour for A/B bisects.
func proto2168() minecraft.Protocol {
	proto2168Once.Do(func() {
		if !mapGameplayItems {
			proto2168Value = v1_26_44.New()
			return
		}
		p, err := registryAwareCodec(2168)
		if err != nil {
			slog.Warn("xvrelay: NewWithRegistries(2168) failed — using wire-only", "error", err)
			proto2168Value = v1_26_44.New()
			return
		}
		adapter2168HasRegistry = true
		proto2168Value = p
		slog.Info("xvrelay: 2168 adapter uses registries (item + block mapping)")
	})
	return proto2168Value
}

func proto2169() minecraft.Protocol {
	proto2169Once.Do(func() {
		if !mapGameplayItems {
			proto2169Value = v1_26_45.New()
			return
		}
		p, err := registryAwareCodec(2169)
		if err != nil {
			slog.Warn("xvrelay: NewWithRegistries(2169) failed — using wire-only", "error", err)
			proto2169Value = v1_26_45.New()
			return
		}
		adapter2169HasRegistry = true
		proto2169Value = p
		slog.Info("xvrelay: 2169 adapter uses registries (item + block mapping)")
	})
	return proto2169Value
}

func adapterHasRegistry(id int32) bool {
	switch id {
	case 1001:
		return adapter1001HasRegistry
	case 2168:
		return adapter2168HasRegistry
	case 2169:
		return adapter2169HasRegistry
	}
	return false
}

// proto1001 returns the 1001 adapter WITH registries (item mapping + block mapping).
//
// ★ Why this is needed: the wire-only `New()` has runtime=nil ⇒ `marshalStackRequestItem`
// reports "protocol 1001 item mapping is not configured" and drops every ItemStackRequest,
// and InventoryContent/ItemRegistry never get their item IDs remapped. Building the mapper is
// expensive, so it is cached.
func proto1001() minecraft.Protocol {
	proto1001Once.Do(func() {
		reg, err := blocktranslate.NativeRegistry(2193)
		if err != nil {
			slog.Warn("xvrelay: cannot get the 2193 palette for the 1001 adapter — using wire-only", "error", err)
			proto1001Value = v1_26_30.New()
			return
		}
		start := time.Now()
		// nativeItems = the 2193 item table (modern client) ⇒ the mapper knows both directions:
		// 2193→1001 (ConvertFromLatest) and 1001→2193 (ConvertToLatest, the host read path).
		nativeItems, ierr := blocktranslate.NativeItems()
		if ierr != nil {
			slog.Warn("xvrelay: no 2193 item registry — item mapping will rely on GameData", "error", ierr)
			nativeItems = nil
		}
		p, err := v1_26_30.NewWithRegistries(reg, nativeItems)
		if err != nil {
			slog.Warn("xvrelay: NewWithRegistries(1001) failed — using wire-only", "error", err)
			proto1001Value = v1_26_30.New()
			return
		}
		adapter1001HasRegistry = true
		proto1001Value = p
		slog.Info("xvrelay: 1001 adapter uses registries (item + block mapping)", "elapsed", time.Since(start))
	})
	return proto1001Value
}

// clientItemRegistry picks the ItemRegistry to put in GameData.Items for an incoming client.
//
// The listener sends GameData.Items as an ItemRegistry packet through WritePacket, which runs the
// client codec's ConvertFromLatest. There are two behaviours, and mixing them up is destructive:
//   - registry-aware (NewWithRegistries: 1001 always, 2168/2169 when
//     -map-gameplay-items is on): the codec TRANSLATES the packet itself and
//     expects the NATIVE (2193) table as input. Handing it the client's own
//     table makes ValidateNativeEntries fail and the WHOLE ItemRegistry packet
//     is dropped (return nil).
//   - registry-less (plain New()): ConvertFromLatest is a pass-through, so
//     GameData.Items must already BE the client protocol's own table.
//
// Measured in internal/blocktranslate/items_test.go:TestItemRegistryMatchesCodec — the tables
// returned by blocktranslate.Items are exactly what the registry-aware codecs emit, and
// v1_26_45.New() passes all 2076 native entries through unchanged.
// clientShieldID returns the runtime ID of minecraft:shield in the client's
// own protocol table. This is the ID the client uses when decoding item
// stacks; a listener-side Conn must encode with exactly the same value.
// Measured: 1001 => 387, 2168/2169/2193 => 358 (native 2193 is also 358).
func clientShieldID(clientProto int32) int32 {
	items, err := blocktranslate.Items(clientProto)
	if err != nil {
		return 0
	}
	for _, e := range items {
		if e.Name == "minecraft:shield" {
			return int32(e.RuntimeID)
		}
	}
	return 0
}

func clientItemRegistry(clientProto int32) ([]protocol.ItemEntry, string, error) {
	// Ensure the cached adapter has been built; sync.Once makes later calls free
	// and also establishes the happens-before for reading the registry flags.
	switch clientProto {
	case 1001:
		proto1001()
	case 2168:
		proto2168()
	case 2169:
		proto2169()
	}
	if adapterHasRegistry(clientProto) {
		items, err := blocktranslate.NativeItems()
		return items, fmt.Sprintf("native 2193 (%d codec translates it)", clientProto), err
	}
	items, err := blocktranslate.Items(clientProto)
	return items, fmt.Sprintf("protocol %d (codec is pass-through)", clientProto), err
}

// gameDataItemsFor picks the ItemRegistry the listener will send for ONE session.
//
// ★ It MUST run for every session, INCLUDING same-wire ones. A registry-aware codec
// (1001 always, 2168/2169 by default) drops any ItemRegistry that is not the native
// 2193 table. Before 2026-11-05 the caller only replaced gd.Items when
// clientProto != hostProto; a 1001 client into a 1001 host therefore inherited the
// host's 1933-entry table and its ItemRegistry was silently dropped, so the client
// never sent RequestChunkRadius / SetLocalPlayerAsInitialised. See
// TestSameWireGameDataItemsNeverDropped in item_registry_test.go.
//
// keepHost=true is the -keepitems bisect escape hatch. The third return value says
// whether a replacement happened (used only for the log line).
func gameDataItemsFor(clientProto int32, hostItems []protocol.ItemEntry, keepHost bool) ([]protocol.ItemEntry, string, bool) {
	if keepHost {
		return hostItems, "host table (-keepitems)", false
	}
	items, source, err := clientItemRegistry(clientProto)
	if err != nil {
		return hostItems, fmt.Sprintf("host table (client registry unavailable: %v)", err), false
	}
	return items, source, true
}

// protocolForVersion maps a Minecraft version string → protocol id.
// Used to VERIFY auto-probing: a lenient host (like the host app) may accept a protocol that is
// not its own, but StartGame still declares the real version.
func protocolForVersion(v string) int32 {
	switch v {
	case "1.26.30", "1.26.31", "1.26.32", "1.26.33", "1.26.34", "1.26.35", "1.26.36":
		return 1001
	case "1.26.40", "1.26.41", "1.26.42", "1.26.43", "1.26.44":
		return 2168
	case "1.26.45":
		return 2169
	case "1.26.50", "1.26.51", "1.26.52":
		return 2193
	}
	return 0
}

// verifyHostProtocol verifies the protocol just dialled against the host's
// `StartGame.GameVersion`.
//
// ★ Why this is needed: the host app (and a few other hosts) does NOT reject a foreign protocol
// — dialling 2193 into a 1.26.33 host still "succeeds", but the packets the host sends are in
// the 1001 layout. Raw-forwarding those to a 2193 client crashes it (log 17:33:21: RAW-FORWARD
// ItemRegistry 107194 B, then the client dies). StartGame declares the real version ⇒ comparing
// it against the protocol we dialled is the reliable detection.
func verifyHostProtocol(cap *spawnCapture, pid int32) (string, bool) {
	raw := cap.startGame.Load()
	if raw == nil {
		return "", true // nothing to check against; accept
	}
	codec, ok := resolveProto(pid)
	if !ok {
		return "", false
	}
	sg, err := decodeRawStartGameWith(codec, *raw)
	if err != nil {
		slog.Warn("xvrelay: cannot decode StartGame to verify the protocol", "protocol", pid, "error", err)
		return "", false
	}
	v := sg.GameVersion
	if v == "" || v == "*" {
		return v, true // the host does not declare a specific version
	}
	want := protocolForVersion(v)
	if want == 0 {
		return v, true // unknown version, no conclusion
	}
	return v, want == pid
}

// resolveHostProto decides the host's protocol.
//
//	"auto"  → use the protocol the host ANNOUNCES in ServerData (w.Host.Protocol)
//	"<num>" → force it, per the user
//
// Returns (protocol, known for sure). "Not known" = ServerData v5/v6 has no Protocol field
// ⇒ dialHost probes in hostProtoProbeOrder.
func resolveHostProto(flagVal string, announced int32) (int32, bool) {
	if strings.EqualFold(strings.TrimSpace(flagVal), "auto") {
		if announced != 0 {
			if _, ok := resolveProto(announced); ok {
				slog.Info("xvrelay: -host-proto auto ⇒ using the protocol the host announces", "protocol", announced)
				return announced, true
			}
			slog.Warn("xvrelay: host announces an unsupported protocol — probing instead", "announced", announced)
			return 2193, false
		}
		return 2193, false
	}
	id, err := strconv.Atoi(strings.TrimSpace(flagVal))
	if err != nil {
		slog.Warn("xvrelay: -host-proto is invalid — probing instead", "value", flagVal)
		return 2193, false
	}
	if _, ok := resolveProto(int32(id)); !ok {
		slog.Warn("xvrelay: no codec for this protocol — probing instead", "id", id)
		return 2193, false
	}
	return int32(id), true
}

// ── Relay ─────────────────────────────────────────────────────────────────────────

// dialRetryMax / dialRetryDelay: retry count and back-off when dialling the host hits an
// IMMEDIATE error.
const (
	dialRetryMax   = 3
	dialRetryDelay = 3 * time.Second
)

// isTransientDialErr recognises dial errors that happen IMMEDIATELY (no timeout spent) because
// the host is not ready — as opposed to "deadline exceeded" (dialTimeout fully spent, the host
// really is not answering).
func isTransientDialErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{
		"no address known", // the host just changed worlds: NetherNet has no address yet ⇒ fails at once
		"signal offer",     // same cause (error wrapped inside auto-probe)
		"empty packet",     // decoder.go:162-164 — a batch with packet length 0
		"connection refused",
		"connection reset",
		"unexpected compression",
		"unknown compression",
		"missing compression algorithm",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

var (
	wireDumpMu sync.Mutex
	wireDumped = map[uint32]bool{}
)

// dumpWireOnce writes the ABOUT-TO-BE-SENT-TO-CLIENT payload of one packet ID (once per ID).
//
// Purpose: decode those bytes with **the client's own codec** (bedrock-codec jar + JDK) and let
// the client's codec report the error — this is the technique that found the wrong
// `SubChunkEntry.Result` enum. Use it for any wire suspicion the relay log does not explain.
func dumpWireOnce(to *minecraft.Conn, pk packet.Packet) {
	wireDumpMu.Lock()
	defer wireDumpMu.Unlock()
	id := pk.ID()
	if wireDumped[id] {
		return
	}
	wireDumped[id] = true
	var buf bytes.Buffer
	pk.Marshal(to.Proto().NewWriter(&buf, 0))
	dir := "log"
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Join(filepath.Dir(exe), "log")
	}
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, fmt.Sprintf("wire-%d-%s.bin", id, time.Now().Format("150405")))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		slog.Warn("xvrelay: writing the wire dump failed", "id", id, "error", err)
		return
	}
	slog.Info("xvrelay: wrote the payload sent to the client", "id", id, "path", path, "bytes", buf.Len())
}

// resizeBitset returns a bitset with the same content but of size `size` (drops the extra bits).
//
// ★ Why: `ClientMovementPredictionSync.ActorFlags` is read/written by the fork using
// `EntityDataFlagCount` for **2193 = 131**, while the 1001 adapter requires **130** ⇒
// `Writer.Bitset` panics "bitset size mismatch: expected 130, got 131" ⇒ the relay DROPS the
// packet (log 20-10-21: repeating about every 30 s) ⇒ the host never receives the client's
// movement prediction ⇒ the host rolls the position back ⇒ the player is yanked backwards while
// running.
func resizeBitset(src protocol.Bitset, size int) protocol.Bitset {
	if src.Len() == size {
		return src
	}
	dst := protocol.NewBitset(size)
	for i := 0; i < size && i < src.Len(); i++ {
		if src.Load(i) {
			dst.Set(i)
		}
	}
	return dst
}

// serverConnDone returns a channel that closes after d.
func serverConnDone(ctx context.Context, d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}()
	return ch
}

// subChunkRequests prevents duplicate SubChunkRequests for the same chunk — PER SESSION.
type subChunkRequests struct {
	mu    sync.Mutex
	seen  map[[3]int32]bool
	count int
}

func newSubChunkRequests() *subChunkRequests {
	return &subChunkRequests{seen: make(map[[3]int32]bool)}
}

// request fetches terrain on the client's behalf when the client does NOT send its own
// SubChunkRequest.
//
// ★ Evidence: log 17:52 — a 1.26.51 client only sent RequestChunkRadius + ServerBoundLoadingScreen
// and never sent SubChunkRequest (id 175) ⇒ it sat on the loading screen and then closed. The
// relay hit the same case with a 1001 client and handled it by sending SubChunkRequest to the
// host itself, then relaying SubChunk back.
func (r *subChunkRequests) request(host *minecraft.Conn, chunk *packet.LevelChunk) {
	if r == nil || host == nil || chunk == nil {
		return
	}
	if _, requestMode := chunk.SubChunkLimit.Value(); !requestMode {
		return
	}
	key := [3]int32{chunk.Dimension, chunk.Position[0], chunk.Position[1]}
	r.mu.Lock()
	if r.seen[key] || r.count >= 256 {
		r.mu.Unlock()
		return
	}
	r.seen[key] = true
	r.count++
	n := r.count
	r.mu.Unlock()

	// Overworld: Y=-64..319 => sub-chunk index -4..19.
	first, last := int32(-4), int32(19)
	if chunk.Dimension == 1 {
		first, last = 0, 7
	} else if chunk.Dimension == 2 {
		first, last = 0, 15
	}
	offsets := make([]protocol.SubChunkOffset, 0, int(last-first)+1)
	for y := first; y <= last; y++ {
		offsets = append(offsets, protocol.SubChunkOffset{0, int8(y), 0})
	}
	req := &packet.SubChunkRequest{
		Dimension: chunk.Dimension,
		Offsets:   offsets,
		Position:  protocol.SubChunkPos{chunk.Position[0], 0, chunk.Position[1]},
	}
	if err := writeSafe(host, req); err != nil {
		slog.Warn("xvrelay: on-behalf SubChunkRequest for the 1001 client failed", "chunk", key, "error", err)
		return
	}
	if n == 1 {
		slog.Info("xvrelay: the relay FETCHED SubChunk itself (client sent no SubChunkRequest)",
			"chunk", key, "yRange", fmt.Sprintf("%d..%d", first, last), "limit", 256)
	}
}

// relay forwards packets from `from` to `to`.
//
// Every packet is DECODED with `from`'s codec and re-ENCODED with `to`'s codec
// (ConvertToLatest on read, ConvertFromLatest on write) ⇒ this is REAL TRANSLATION.
func relay(from, to *minecraft.Conn, dir string, trace int) {
	defer to.Close()
	defer from.Close()
	var n, skipped int64
	tally := make(map[string]int64)
	// Ring buffer of the LAST 15 packets sent to the client — shows which packet the client died
	// right after.
	var recent []string
	defer func() {
		if dir == "H->C" && len(recent) > 0 {
			slog.Info("xvrelay: last 15 packets sent to the client", "list", strings.Join(recent, " "))
		}
	}()
	for {
		pk, err := from.ReadPacket()
		if err != nil {
			var disc minecraft.DisconnectError
			if errors.As(err, &disc) {
				slog.Info("xvrelay: relay end (disconnect)", "dir", dir, "packets", n, "reason", disc.Error())
			} else {
				slog.Info("xvrelay: relay end", "dir", dir, "packets", n, "error", err)
			}
			logTally(dir, tally, skipped)
			return
		}
		name := fmt.Sprintf("%T", pk)
		tally[name]++
		// Diagnostics: the publisher update decides whether the client asks for chunks at all.
		if dir == "H->C" {
			if pu, ok := pk.(*packet.NetworkChunkPublisherUpdate); ok {
				if v, ok2 := sessions.Load(to); ok2 {
					s := v.(*session)
					if s.capture != nil && s.capture.publisherLogged.CompareAndSwap(false, true) {
						slog.Info("xvrelay: first NetworkChunkPublisherUpdate",
							"pos", pu.Position, "radius", pu.Radius,
							"savedChunks", len(pu.SavedChunks))
					}
				}
			}
		}
		// BISECT: drop chunks to learn whether the client dies because of chunks or something else.
		if dir == "H->C" {
			if onlyPrefix {
				continue
			}
			if dropChunks {
				if _, ok := pk.(*packet.LevelChunk); ok {
					continue
				}
				if _, ok := pk.(*packet.SubChunk); ok {
					continue
				}
			}
			if dropSubChunks {
				if _, ok := pk.(*packet.SubChunk); ok {
					continue
				}
			}
			if dropIDs != nil && dropIDs[pk.ID()] {
				continue
			}
		}
		if dir == "H->C" {
			if chunk, ok := pk.(*packet.LevelChunk); ok {
				if v, ok := sessions.Load(to); ok {
					s := v.(*session)
					// ★ A cross-wire client does NOT fetch terrain by itself in practice.
					// Evidence: log 23:03 (2193 client, 1001 host) H->C had LevelChunk=921 but
					// SubChunk=0 and C->H carried no SubChunkRequest at all ⇒ the client stood
					// in an empty world ("chunks never load").
					//
					// ★★ Confirmed by three live sessions 16-48 / 17-03 / 17-19 (2193 client,
					// 1001 host): a 1.26.44+ client ALWAYS sends its own SubChunkRequest
					// (10–16 requests × 3 neighbouring offsets within <0.5 s). Pushing on its
					// behalf is only needed for a 1001 client (which never sends id 175 —
					// logs 23:03, 17:52).
					// A push running inside the ~0.4 s window between "gate opens" and "client's
					// first request" is still enough to kill the client: 139 24-entry replies
					// crammed into 0.4 s (some 1–2.4 MB because of large chest block-entity NBT)
					// ⇒ the client drops RakNet itself.
					// ⇒ Push ONLY for a 1001 client. clientRequested is the extra safety catch.
					if !noPushSubChunks && to.Proto().ID() == 1001 && !s.clientRequested.Load() &&
						s.hostProto != 0 && s.hostProto != to.Proto().ID() {
						s.reqs.request(from, chunk)
					}
				}
			}
		}
		if trace > 0 && n < int64(trace) {
			slog.Info("xvrelay: trace", "dir", dir, "n", n, "type", name, "id", pk.ID())
		}
		if dir == "H->C" {
			if value, ok := sessions.Load(to); ok {
				// The 1001 adapter maps blocks itself for non-chunk packets when registries are
				// enabled — skip to avoid double mapping.
				if !noBlockTranslate && !adapterMapsBlocks(to.Proto().ID(), pk) {
					translated, err := translateSessionPacket(value.(*session), pk)
					if err != nil {
						slog.Error("xvrelay: block translation failed; closing session", "packet", name, "error", err)
						return
					}
					if translated == nil {
						// Incomplete translation ⇒ DROP this packet (that area becomes air)
						// instead of sending the host payload with block IDs that do not exist on
						// the client.
						continue
					}
					pk = translated
				}
			}
		}
		if dir == "C->H" {
			// ★ Measuring the 6-asked/3-answered gap: log the EXACT offsets the client asked for
			// so they can be matched against the AUDIT entry of the returned SubChunk (capped at
			// 12 requests per session to keep the noise down).
			if req, ok := pk.(*packet.SubChunkRequest); ok {
				if v, ok2 := sessions.Load(from); ok2 {
					s := v.(*session)
					// Mark that the client fetches for itself ⇒ disable on-behalf pushing (see
					// session.clientRequested).
					s.clientRequested.Store(true)
					if s.reqLogged.Add(1) <= 12 {
						offs := make([]string, 0, len(req.Offsets))
						for _, o := range req.Offsets {
							offs = append(offs, fmt.Sprintf("[%d %d %d]", o[0], o[1], o[2]))
						}
						slog.Info("xvrelay: client xin SubChunk", "n", s.reqLogged.Load(),
							"dim", req.Dimension,
							"pos", fmt.Sprintf("[%d %d %d]", req.Position[0], req.Position[1], req.Position[2]),
							"soOffset", len(req.Offsets), "offsets", strings.Join(offs, " "))
					}
				}
			}
			if value, ok := sessions.Load(from); ok {
				s := value.(*session)
				if s.reverseBlocks != nil {
					translated, err := s.reverseBlocks.TranslateInteractions(pk)
					if err != nil {
						slog.Error("xvrelay: clicked block translation failed; closing session", "error", err)
						return
					}
					pk = translated
				}
			}
		}
		// AUDIT + VERIFY sit IMMEDIATELY BEFORE the write ⇒ they measure the exact bytes the
		// client receives, after both the shell normalisation and the palette translation.
		// (Placed before the normalisation they used to false-alarm on the host's 0xFFFFFFFE
		// sentinel.)
		if dir == "H->C" {
			if auditOn {
				switch a := pk.(type) {
				case *packet.SubChunk:
					auditSubChunk(a, to.Proto().ID(), to.GameData().UseBlockNetworkIDHashes)
				case *packet.LevelChunk:
					auditLevelChunk(a, to.Proto().ID())
				}
			}
			if !verifyPacket(pk, to.Proto()) {
				// Do not send a packet the client would fail to decode ⇒ keep the session alive.
				skipped++
				continue
			}
		}
		// Diagnostics: write the ABOUT-TO-BE-SENT payload so it can be decoded with the client's
		// own codec.
		if dir == "H->C" && dumpWireIDs != nil && dumpWireIDs[pk.ID()] {
			dumpWireOnce(to, pk)
		}
		if err := writeSafe(to, pk); err != nil {
			// RULE: one packet that fails to encode must NOT kill the session.
			if connDead(err) {
				slog.Info("xvrelay: relay write end (destination closed)", "dir", dir, "packets", n, "error", err)
				logTally(dir, tally, skipped)
				return
			}
			skipped++
			slog.Warn("xvrelay: dropped 1 packet, session kept", "dir", dir, "type", name, "id", pk.ID(), "error", err)
			continue
		}
		n++
		if dir == "H->C" {
			recent = append(recent, fmt.Sprintf("%s#%d", strings.TrimPrefix(name, "*packet."), pk.ID()))
			if len(recent) > 15 {
				recent = recent[1:]
			}
		}
	}
}

// writeSafe turns an encode-time PANIC into an error.
//
// gophertunnel's Writer dispatches on the Go type; a packet that decodes fine with the source
// codec can panic when encoded with the destination codec (layout mismatch). A panic inside a
// relay goroutine kills the WHOLE PROCESS.
func writeSafe(to *minecraft.Conn, pk packet.Packet) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while encoding %T: %v", pk, r)
		}
	}()
	return to.WritePacket(pk)
}

// connDead reports whether a write error came from the DESTINATION already being closed.
func connDead(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// logTally prints the per-type packet counter, descending.
func logTally(dir string, tally map[string]int64, skipped int64) {
	type kv struct {
		name string
		n    int64
	}
	list := make([]kv, 0, len(tally))
	for name, c := range tally {
		list = append(list, kv{name, c})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].name < list[j].name
	})
	// ★ No longer truncated to 25: the H->C tally can reach 39 types, and cutting it hid 14 —
	// which once concealed the evidence while narrowing down why the client dropped mid-session.
	parts := make([]string, 0, len(list))
	for _, e := range list {
		parts = append(parts, fmt.Sprintf("%s=%d", strings.TrimPrefix(e.name, "*packet."), e.n))
	}
	slog.Info("xvrelay: relay tally", "dir", dir, "kinds", len(tally), "skipped", skipped,
		"detail", strings.Join(parts, " "))
}

// ── PacketFunc ────────────────────────────────────────────────────────────────────

// newHostPacketFunc captures the host's RAW payloads for ONE session (its own capture).
//
// `Conn.decode` calls packetFunc with the ORIGINAL payload (packet.go:26-28) ⇒ these are exactly
// the bytes the host sent. The relay NEVER sends these 5 types to the host ⇒ every occurrence is
// host→relay.
func newHostPacketFunc(cap *spawnCapture) func(packet.Header, []byte, net.Addr, net.Addr) {
	return func(h packet.Header, payload []byte, src, dst net.Addr) {
		switch h.PacketID {
		case packet.IDJigsawStructureData:
			storeRaw(&cap.jigsaw, payload)
		case packet.IDVoxelShapes:
			storeRaw(&cap.voxelShapes, payload)
		case packet.IDStartGame:
			storeRaw(&cap.startGame, payload)
			slog.Info("xvrelay: ★ captured the host's RAW StartGame", "len", len(payload), "clientAddr", dst)
		case packet.IDDimensionData:
			storeRaw(&cap.dimensionData, payload)
		case packet.IDItemRegistry:
			storeRaw(&cap.itemRegistry, payload)
		case packet.IDSubChunk:
			// ★ Capture the HOST's (1001) original SubChunk wire bytes — a decode mismatch
			// between the 1001 codec and the real wire is the suspect behind the 1.9 MB entry
			// payload; only the original bytes allow a comparison.
			captureHostSubChunkWire(payload)
		}
	}
}

var hostSubChunkOnce sync.Once

// captureHostSubChunkWire writes the host's first SubChunk payload to log/.
func captureHostSubChunkWire(payload []byte) {
	hostSubChunkOnce.Do(func() {
		dir := "log"
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Join(filepath.Dir(exe), "log")
		}
		path := filepath.Join(dir, "host-subchunk-"+time.Now().Format("150405")+".bin")
		if err := os.WriteFile(path, payload, 0o644); err == nil {
			slog.Info("xvrelay: captured the host's SubChunk wire bytes", "path", path, "len", len(payload))
		}
	})
}

// clientPacketFunc opens the H->C gate once the RequestChunkRadius response has been written.
//
// Condition: an empty CreativeContent (2-byte payload {0,0}) — this packet is only ever sent
// server→client, so the direction does not need to be distinguished. It is written INSIDE the
// conn's `sendMu` (conn.go:407 is inside writePacket), so by the time the signal fires,
// StartGame/PlayStatus are already in bufferedSend ⇒ the H->C relay cannot squeeze a chunk into
// the middle of the spawn sequence.
//
// ★ Multiple clients: the gate is looked up by CLIENT ADDRESS. On READ the client is `src`, on
// WRITE the client is `dst` (packet.go:28 vs conn.go:408) — both are tried, and the map only
// holds client addresses, so there is no mix-up.
func clientPacketFunc(h packet.Header, payload []byte, src, dst net.Addr) {
	// Only log packets READ FROM the client: on read, src is the client address (already
	// registered in clientGates). On write, src is the listener's address ⇒ not counted.
	if src != nil {
		if v, fromClient := clientGates.Load(src.String()); fromClient {
			g := v.(*clientGate)
			if n := g.count.Add(1); n <= 80 {
				slog.Info("xvrelay: client sent", "n", n, "id", h.PacketID, "len", len(payload))
			}
			// id=113 is what makes `conn.spawn` close ⇒ StartGame returns nil. Logged explicitly
			// and NOT subject to the 80-packet cap, so every session can be concluded.
			if h.PacketID == packet.IDSetLocalPlayerAsInitialised {
				g.saw113.Store(true)
				slog.Info("xvrelay: ★ client sent SetLocalPlayerAsInitialised (113)", "len", len(payload))
			}
		} else if h.PacketID == packet.IDSubChunk {
			blocktranslate.CaptureSubChunkWire(payload)
		} else if dumpStartPath != "" && h.PacketID == packet.IDStartGame {
			// This is the WRITE direction: save the exact bytes we send the client so they can be
			// compared against the relay capture.
			if err := os.WriteFile(dumpStartPath, payload, 0o644); err == nil {
				slog.Info("xvrelay: dumped the StartGame sent to the client", "path", dumpStartPath, "len", len(payload))
			}
		}
	}
	if h.PacketID == packet.IDCreativeContent && len(payload) == 2 && payload[0] == 0 && payload[1] == 0 {
		signalClientGate(src)
		signalClientGate(dst)
	}
	if h.PacketID == packet.IDDisconnect {
		slog.Warn("xvrelay: ★ DISCONNECT", "len", len(payload), "text", printableRuns(payload))
	}
}

func signalClientGate(addr net.Addr) {
	if addr == nil {
		return
	}
	if v, ok := clientGates.Load(addr.String()); ok {
		v.(*clientGate).gate.signal()
	}
}

// printableRuns extracts the readable runs from a raw payload (keeps bytes >= 0x80 for UTF-8).
func printableRuns(b []byte) string {
	var runs []string
	var cur []byte
	flush := func() {
		if len(cur) >= 3 {
			runs = append(runs, string(cur))
		}
		cur = cur[:0]
	}
	for _, c := range b {
		if c >= 0x20 && c != 0x7F {
			cur = append(cur, c)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(runs, " | ")
}

// ── LAN discovery ────────────────────────────────────────────────────────────────

// probeTargetsFor turns Minecraft's local UDP ports into unicast destinations.
//
// ★ Why this is needed: a 1.26.45+ host does NOT bind 7551, it uses an ephemeral socket. A
// broadcast to 255.255.255.255:7551 never reaches it ⇒ a host running on the SAME MACHINE is
// invisible.
func probeTargetsFor(ports []int) []*net.UDPAddr {
	if len(ports) == 0 {
		return nil
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv4bcast}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsUnspecified() {
				continue
			}
			ips = append(ips, ip4)
		}
	}
	out := make([]*net.UDPAddr, 0, len(ports)*len(ips))
	for _, p := range ports {
		if p <= 0 || p > 65535 {
			continue
		}
		for _, ip := range ips {
			out = append(out, &net.UDPAddr{IP: ip, Port: p})
		}
	}
	return out
}

// discoverWorlds opens the discovery socket and waits until a usable LAN world appears.
func discoverWorlds(ctx context.Context, timeout time.Duration) (*lansignal.Signaling, error) {
	localPorts := mcport.Find()
	addr := "0.0.0.0:7551"
	if len(localPorts) > 0 {
		addr = "0.0.0.0:0"
		slog.Info("xvrelay: Minecraft local giữ UDP port ⇒ probe trực tiếp",
			"ports", mcport.Describe(localPorts))
	}
	extra := probeTargetsFor(localPorts)
	ls, err := lansignal.Listen(lansignal.Options{Addr: addr, Log: slog.Default(), ExtraBroadcasts: extra})
	if err != nil {
		ls, err = lansignal.Listen(lansignal.Options{Addr: "0.0.0.0:0", Log: slog.Default(), ExtraBroadcasts: extra})
	}
	if err != nil {
		return nil, fmt.Errorf("listen discovery: %w", err)
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var firstMatch time.Time
	for {
		for _, w := range ls.Worlds() {
			if usableWorld(w) {
				if firstMatch.IsZero() {
					firstMatch = time.Now()
				}
				break
			}
		}
		// Wait one more broadcast beat so every world is collected before choosing.
		if !firstMatch.IsZero() && time.Since(firstMatch) >= 2200*time.Millisecond {
			return ls, nil
		}
		select {
		case <-ctx.Done():
			ls.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			if !firstMatch.IsZero() {
				return ls, nil
			}
			peers := ls.Peers()
			ls.Close()
			return nil, fmt.Errorf("no LAN world found after %s (peers=%d)", timeout, peers)
		case <-tick.C:
		}
	}
}

// usableWorld filters out worlds without a complete HostInfo. ServerData v6+ must carry a Nonce.
func usableWorld(w lansignal.World) bool {
	if w.Host == nil {
		return false
	}
	if w.Host.Version >= 6 && w.Host.Nonce == "" {
		return false
	}
	return true
}

// pickWorld selects a world by ID (or the only world when no ID is given).
func pickWorld(ls *lansignal.Signaling, id string) (lansignal.World, error) {
	return pickWorldSnapshot(ls.Worlds(), id)
}

func pickWorldSnapshot(worlds []lansignal.World, id string) (lansignal.World, error) {
	var found []lansignal.World
	for _, w := range worlds {
		if usableWorld(w) {
			found = append(found, w)
		}
	}
	if id != "" {
		// ★ Prefer the level/host NAME over the ID: measured live on 27/10 — NetworkID
		// changes every time the host reopens the world (5 different IDs within 6 hours:
		// 3496404436851570975 → 4395078694002118293 → 9407821823456942144 →
		// 274860648516507692 → 11710586761163450150 → 9742803096907036147), while the
		// level name stays stable ⇒ `--world "name"` survives reopening the world; a
		// numeric ID is still accepted.
		for _, w := range found {
			if w.Host == nil {
				continue
			}
			if w.Host.LevelName == id || w.Host.ServerName == id {
				return w, nil
			}
		}
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil {
			ids := make([]string, 0, len(found))
			for _, w := range found {
				name := ""
				if w.Host != nil {
					name = w.Host.LevelName
				}
				ids = append(ids, fmt.Sprintf("%d(%q)", w.NetworkID, name))
			}
			return lansignal.World{}, fmt.Errorf("--world %q does not match any world/host name; worlds: %v", id, ids)
		}
		for _, w := range found {
			if w.NetworkID == n {
				return w, nil
			}
		}
		return lansignal.World{}, fmt.Errorf("no world with id %s", id)
	}
	if len(found) != 1 {
		ids := make([]uint64, 0, len(found))
		for _, w := range found {
			ids = append(ids, w.NetworkID)
		}
		return lansignal.World{}, fmt.Errorf("found %d worlds (IDs %v); pick one with --world ID", len(found), ids)
	}
	return found[0], nil
}

// worldByID re-reads the world RIGHT BEFORE dialling — discovery may have refreshed the Nonce.
// lastWorlds keeps the LAST SEEN address of every network ID.
//
// ★ Why this is needed: lansignal's `peerTimeout` is 15 s (mirroring vanilla). the host app STOPS
// broadcasting while it is serving a session, so a single slow dial is enough for the peer to
// expire and every subsequent dial to be blocked by "world … disappeared from discovery" — even
// though the host is alive and still on the same address. Measured: log 12:20:30 (announce
// 12:20:31 → probe hung 25 s → world gone at 12:21:03) and log 13:19:18 (announce 13:19:19 →
// 4 probe rounds → world gone at 13:19:55).
// This cache is only an OBSERVED ADDRESS for a given network ID; if the host really died the dial
// still fails at the transport layer, so it never hides a real failure.
var (
	lastWorldMu sync.Mutex
	lastWorlds  = map[uint64]lansignal.World{}
)

func rememberWorld(w lansignal.World) {
	if w.NetworkID == 0 || w.Addr == nil {
		return
	}
	lastWorldMu.Lock()
	lastWorlds[w.NetworkID] = w
	lastWorldMu.Unlock()
}

func worldByID(ls *lansignal.Signaling, id uint64) (lansignal.World, bool) {
	for _, w := range ls.Worlds() {
		if w.NetworkID == id && usableWorld(w) {
			rememberWorld(w)
			return w, true
		}
	}
	lastWorldMu.Lock()
	w, ok := lastWorlds[id]
	lastWorldMu.Unlock()
	if ok && usableWorld(w) {
		slog.Warn("xvrelay: world temporarily absent from discovery — using the remembered address",
			"network_id", id, "addr", w.Addr, "level", w.Host.LevelName)
		return w, true
	}
	return lansignal.World{}, false
}

// dialHost dials the LAN world with the HOST's codec, borrowing the real client's ClientData.
// `cap` is this session's OWN raw-payload capture buffer.
//
// ★★ IDENTITY: the client's IdentityData must be passed through to the Dialer UNCHANGED.
//
// Evidence (gophertunnel dial.go:543-551):
//
//	func defaultIdentityData(data *login.IdentityData) {
//	    if data.Identity == "" { data.Identity = uuid.New().String() }
//	    if data.DisplayName == "" { data.DisplayName = "Steve" }   // ← right here
//	}
//	func defaultClientData(address, username string, d *login.ClientData) {
//	    d.ThirdPartyName = username                                // ← overwrites the client name
//	}
//
// A Dialer that does not set IdentityData ⇒ empty DisplayName ⇒ gophertunnel fills in "Steve" and
// overwrites ThirdPartyName ⇒ the host sees Steve, exactly as if nobody had signed into Xbox.
//
// The fix: build a self-signed Login JWT carrying the client's `xname`/`xid`/`leguuid`, then patch
// `ThirdPartyName`/`SelfSignedId`/`Nonce` in the ClientData JWT. Gophertunnel does the equivalent
// job in `login.EncodeOffline` — it just needs real IdentityData handed to it.
func dialHost(ctx context.Context, ls *lansignal.Signaling, id uint64, cd login.ClientData, identity login.IdentityData, cap *spawnCapture) (*minecraft.Conn, error) {
	hostProtoMu.Lock()
	defer hostProtoMu.Unlock()
	if hostProtoAuto && hostProfileWorldID != id {
		current, ok := worldByID(ls, id)
		if !ok {
			return nil, fmt.Errorf("world %d disappeared from discovery", id)
		}
		resetHostProfile(current)
		hostProfileWorldID = id
	}
	// Protocol already known (flag or ServerData v7) ⇒ dial straight away.
	if !hostProtoAuto || hostProtoKnown.Load() {
		return dialHostWith(ctx, ls, id, cd, identity, cap, hostProtoID.Load(), dialTimeout)
	}
	// ★ Unknown (ServerData v5/v6 carries no Protocol): PROBE for it.
	// A host given the wrong protocol answers PlayStatusLoginFailedServer very quickly (~0.5 s), so
	// trying them in turn is far cheaper than making the user guess numbers one at a time.
	if hostProtoKnown.Load() {
		return dialHostWith(ctx, ls, id, cd, identity, cap, hostProtoID.Load(), dialTimeout)
	}
	// Candidate queue: the CORRECT protocol (inferred from the version the host declares) can be
	// pushed to the front.
	queue := append([]int32(nil), hostProtoProbeOrder...)
	// Hint by host address: a protocol that already dialled successfully for another world on the
	// same machine.
	if h := hostProtoHint.Load(); h != 0 {
		queue = append([]int32{h}, queue...)
	}
	tried := make(map[int32]bool, len(queue))
	var lastErr error
	// fallback: the connection handshaked SUCCESSFULLY but the version the host declares does not
	// match the codec. Kept because a successful handshake means the host ACCEPTED that protocol;
	// with no better candidate, using it beats dropping the session.
	var fallback *minecraft.Conn
	var fallbackProto int32
	var fallbackStartGame []byte
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if tried[pid] {
			continue
		}
		tried[pid] = true
		cap.reset()
		conn, err := dialHostWith(ctx, ls, id, cd, identity, cap, pid, 10*time.Second)
		if err != nil {
			lastErr = err
			slog.Warn("xvrelay: auto-probe failed", "protocol", pid, "error", err)
			if ctx.Err() != nil {
				return nil, err
			}
			// The host needs a few hundred ms to clean up the session it just closed before it
			// accepts a new one.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(1200 * time.Millisecond):
			}
			continue
		}
		// ★ Cross-check against the version the host declares — a lenient host may accept the
		// wrong protocol.
		if hv, ok := verifyHostProtocol(cap, pid); !ok {
			// ★ BUG FIXED: this branch used to `continue` WITHOUT assigning lastErr ⇒ when every
			// candidate landed here, `fmt.Errorf(… %w, lastErr)` produced "%!w(<nil>)" and the
			// session was dropped even though a dial had succeeded (log 19:34-17: "cannot dial
			// the host with any protocol [2193 2169 1001 2168]: %!w(<nil>)").
			lastErr = fmt.Errorf("host declares version %q which does not match protocol %d", hv, pid)
			slog.Warn("xvrelay: host version DIFFERS from the dialled protocol — dropping candidate",
				"protocol", pid, "hostGameVersion", hv)
			if fallback == nil {
				fallback, fallbackProto = conn, pid
				if p := cap.startGame.Load(); p != nil {
					fallbackStartGame = append([]byte(nil), (*p)...)
				}
			} else {
				_ = conn.Close()
			}
			// Jump STRAIGHT to the correct protocol instead of guessing through the rest.
			if want := protocolForVersion(hv); want != 0 && !tried[want] {
				queue = append([]int32{want}, queue...)
				slog.Info("xvrelay: prioritising the protocol inferred from the host version", "protocol", want, "version", hv)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(1200 * time.Millisecond):
			}
			continue
		} else if hv != "" {
			slog.Info("xvrelay: protocol verification OK", "protocol", pid, "hostGameVersion", hv)
		}
		hostProtoID.Store(pid)
		hostProtoKnown.Store(true)
		if w, ok := worldByID(ls, id); ok && w.Addr != nil {
			rememberHostProto(w.Addr.String(), pid)
		}
		slog.Info("xvrelay: ★ auto-probe found the host protocol", "protocol", pid,
			"probeOrder", hostProtoProbeOrder)
		return conn, nil
	}
	if fallback != nil {
		// Restore the raw StartGame payload from that successful handshake (cap.reset() in the
		// later attempts wiped it).
		if fallbackStartGame != nil {
			storeRaw(&cap.startGame, fallbackStartGame)
		}
		slog.Warn("xvrelay: no protocol matched the host version — using the connection that handshaked successfully",
			"protocol", fallbackProto)
		hostProtoID.Store(fallbackProto)
		hostProtoKnown.Store(true)
		if w, ok := worldByID(ls, id); ok && w.Addr != nil {
			rememberHostProto(w.Addr.String(), fallbackProto)
		}
		return fallback, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no protocol candidate left")
	}
	return nil, fmt.Errorf("cannot dial the host with any protocol %v: %w", hostProtoProbeOrder, lastErr)
}

// hostProtoProbeOrder is the order tried when the host does NOT declare a protocol (ServerData
// v5/v6).
var hostProtoProbeOrder = []int32{2193, 2169, 1001, 2168}

func dialHostWith(ctx context.Context, ls *lansignal.Signaling, id uint64, cd login.ClientData, identity login.IdentityData, cap *spawnCapture, protoID int32, timeout time.Duration) (*minecraft.Conn, error) {
	w, ok := worldByID(ls, id)
	if !ok {
		return nil, fmt.Errorf("world %d disappeared from discovery", id)
	}
	proto, ok := resolveProto(protoID)
	if !ok {
		return nil, fmt.Errorf("no codec for host protocol %d", protoID)
	}
	if protoID == 1001 {
		proto = host1001Protocol{Protocol: proto}
	}

	// CONN-005: ONE guard per dial (sessions can spawn concurrently). packCtx is the
	// parent of the dial context, so the guard can abort the DialContext wait on spawn
	// the moment a REAL pack size breaks a limit — instead of burning dialTimeout on a
	// host that misreports sizes. See cmd/xvrelay/packs.go for the measured seams.
	packCtx, packCancel := context.WithCancel(ctx)
	defer packCancel()
	g := newPackGuard(packCfg)
	g.setAbort(func(reason string) {
		slog.Warn("xvrelay: pack guard cancelled the host dial", "reason", reason)
		packCancel()
	})
	cap.packs.Store(g)
	hostCodec := packSniffProtocol{Protocol: proto, guard: g}

	d := minecraft.Dialer{
		EnableClientCache:   false, // rewritten chunk bytes must not retain host blob hashes
		KeepXBLIdentityData: true,  // keep XUID/TitleID in the token sent to the host
		IdentityData:        identity,
		ClientData:          cd,
		Protocol:            hostCodec,
		ErrorLog:            slog.Default().With("link", "host"),
		PacketFunc:          newHostPacketFunc(cap),
		// Cache packs across dials: a host with its own pack would otherwise re-download from
		// scratch every session and blow the timeout (log 21:35: ResourcePackDataInfo ×3 then
		// deadline).
		ResourcePackCache: minecraft.DirResourcePackCache{Dir: packCacheDir()},
		// ★ Default policy (pack-mode=skip) still refuses every pack — the behaviour that
		// was hardcoded here before CONN-005. The guard only downloads when the operator
		// explicitly asks for -pack-mode download|limit, and it bounds the cost by
		// -pack-budget-mb / -pack-max-mb. A lying host is caught at ResourcePackDataInfo
		// (the REAL size) before the first chunk is requested (-pack-overrun abort).
		DownloadResourcePack: g.decide,
	}
	// Send the HOST's version to the host, not the client's version.
	// (the caller may have forced one, e.g. -probe-version)
	if d.ClientData.GameVersion == "" {
		d.ClientData.GameVersion = hostGameVersion
	}
	if d.ClientData.GameVersion == "" {
		d.ClientData.GameVersion = proto.Ver()
	}
	// ThirdPartyName is the name the host shows to other players. A real client usually has one,
	// but if it is empty fill it from the identity so it does not fall back to "Steve".
	if d.ClientData.ThirdPartyName == "" {
		d.ClientData.ThirdPartyName = identity.DisplayName
	}
	// SelfSignedId: normalise to the UUID derived from the XUID when protocol >= 2168
	// (UUID.nameUUIDFromBytes("pocket-auth-1-xuid:"+xuid)).
	// The host uses this value to confirm that a self-signed login really declares a real XUID.
	if protoID >= 2168 && identity.XUID != "" {
		if canonical := uuidForXUID(identity.XUID); canonical != "" {
			d.ClientData.SelfSignedID = canonical
		}
	}
	if w.Host != nil && w.Host.Nonce != "" {
		d.ClientData.Nonce = w.Host.Nonce
	}
	slog.Info("xvrelay: identity sent to the host",
		"displayName", identity.DisplayName, "xuid", identity.XUID,
		"identity", identity.Identity, "selfSignedId", d.ClientData.SelfSignedID,
		"thirdPartyName", d.ClientData.ThirdPartyName,
		"hostProto", protoID, "gameVersion", d.ClientData.GameVersion)
	dctx, cancel := context.WithTimeout(packCtx, timeout)
	defer cancel()
	var conn *minecraft.Conn
	var dialErr error
	if directHostAddr != "" {
		// Plain RakNet host (BDS with transport=raknet). No WebRTC offer/answer,
		// no LAN discovery signalling — the self-check harness uses this path.
		slog.Info("xvrelay: dialling host over direct RakNet",
			"addr", directHostAddr, "hostProto", protoID)
		conn, dialErr = d.DialContext(dctx, "raknet", directHostAddr)
	} else {
		conn, dialErr = d.DialContextNetwork(dctx, minecraft.NetherNet{
			Signaling: ls,
			Dialer: nethernet.Dialer{
				AllowIdentitylessServer: true,
				DisableTrickleICE:       true,
			},
		}, strconv.FormatUint(id, 10))
	}
	if dialErr != nil {
		if aborted, reason := g.abortStatus(); aborted {
			return nil, fmt.Errorf("%w (pack guard aborted the host dial: %s)", dialErr, reason)
		}
		return nil, dialErr
	}
	slog.Info("xvrelay: pack guard summary", "hostProto", protoID, "summary", g.summary())
	return conn, nil
}

// uuidForXUID reproduces Java's UUID.nameUUIDFromBytes exactly:
// MD5(bytes) then force version 3 + the RFC-4122 variant. Used for SelfSignedId.
func uuidForXUID(xuid string) string {
	sum := md5.Sum([]byte("pocket-auth-1-xuid:" + xuid))
	sum[6] = (sum[6] & 0x0f) | 0x30 // version 3
	sum[8] = (sum[8] & 0x3f) | 0x80 // variant RFC-4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// ── DimensionData fallback ────────────────────────────────────────────────────────

// defaultDimensionDefinitions is the historical synthetic table retained only
// for the explicit -dimdata force diagnostic. Auto must not emit this table.
func defaultDimensionDefinitions() []protocol.DimensionDefinition {
	return []protocol.DimensionDefinition{
		{
			Name:          "minecraft:overworld",
			MinimumY:      -64,
			HeightRange:   383,
			Generator:     protocol.GeneratorOverworld,
			DimensionType: packet.DimensionOverworld,
			PackID:        uuid.Nil,
			DefaultBiome:  "minecraft:plains",
		},
		{
			Name:          "minecraft:nether",
			MinimumY:      0,
			HeightRange:   127,
			Generator:     protocol.GeneratorNether,
			DimensionType: packet.DimensionNether,
			PackID:        uuid.Nil,
			DefaultBiome:  "minecraft:nether_wastes",
		},
		{
			Name:          "minecraft:the_end",
			MinimumY:      0,
			HeightRange:   255,
			Generator:     protocol.GeneratorEnd,
			DimensionType: packet.DimensionEnd,
			PackID:        uuid.Nil,
			DefaultBiome:  "minecraft:the_end",
		},
	}
}

// applyDimensionFallback preserves the host's dimension definitions by default.
// Only the explicit diagnostic "force" mode synthesises the historical table.
func applyDimensionFallback(gd *minecraft.GameData) {
	switch dimDataMode {
	case "off":
		return
	case "force":
		gd.Dimensions = defaultDimensionDefinitions()
		slog.Info("xvrelay: synthetic DimensionData (-dimdata=force)", "count", len(gd.Dimensions))
	default:
		if len(gd.Dimensions) > 0 {
			slog.Info("xvrelay: DimensionData from the host", "count", len(gd.Dimensions))
			return
		}
		slog.Info("xvrelay: host sent no DimensionData; keeping the client's vanilla dimensions")
	}
}

// ── Client session ───────────────────────────────────────────────────────────────

func handleConn(ctx context.Context, client *minecraft.Conn, ls *lansignal.Signaling, worldID uint64, trace int) error {
	defer client.Close()
	started := time.Now()
	defer func() {
		slog.Info("xvrelay: session ended", "remote", client.RemoteAddr(),
			"elapsed", time.Since(started).Round(time.Millisecond))
	}()

	// ★ State PRIVATE to this session — concurrent clients must not trample each other.
	sess := &session{
		capture: &spawnCapture{},
		gate:    newSpawnPrefixGate(),
		reqs:    newSubChunkRequests(),
	}
	sessions.Store(client, sess)
	defer sessions.Delete(client)
	cg := &clientGate{gate: sess.gate}
	clientGates.Store(client.RemoteAddr().String(), cg)
	defer func() {
		// Only delete it if it is still THIS SESSION's gate — a new session reusing the same
		// address may already have Stored over the same key before this one ended.
		if cur, ok := clientGates.Load(client.RemoteAddr().String()); ok && cur == cg {
			clientGates.Delete(client.RemoteAddr().String())
		}
	}()

	cd := client.ClientData()
	identity := client.IdentityData()
	if identityMode == "bot" {
		identity = login.IdentityData{}
	}
	slog.Info("xvrelay: client connected", "remote", client.RemoteAddr(),
		"proto", client.Proto().ID(), "hostProto", hostProtoID.Load(),
		"gameVersion", cd.GameVersion, "displayName", identity.DisplayName,
		"xuid", identity.XUID, "identity", identity.Identity, "identityMode", identityMode)

	// Borrow the client's ClientData + IdentityData, but force GameVersion to the HOST's.
	hostData := cd
	hostData.GameVersion = ""
	// ★ The host dial can fail TEMPORARILY right after a previous session: a player-host needs a
	// few seconds to clean up the old session. Measured (log 19-10-47): client connected at
	// 19:18:04 → 206 ms later "decode batch: empty packet" — the host sent a batch containing a
	// packet with `length == 0`, rejected at `minecraft/protocol/packet/decoder.go:162-164`.
	// That is an IMMEDIATE error (no timeout burned), not a dead host ⇒ retry instead of kicking
	// the client straight away.
	var serverConn *minecraft.Conn
	var err error
	for attempt := 1; attempt <= dialRetryMax; attempt++ {
		serverConn, err = dialHost(ctx, ls, worldID, hostData, identity, sess.capture)
		if err == nil {
			break
		}
		if !isTransientDialErr(err) || attempt == dialRetryMax {
			break
		}
		slog.Warn("xvrelay: host dial failed temporarily — retrying",
			"attempt", attempt, "of", dialRetryMax, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-serverConnDone(ctx, dialRetryDelay):
		}
	}
	if err != nil {
		slog.Error("xvrelay: dialling the world failed", "error", err)
		return err
	}
	defer serverConn.Close()
	// ★ Pin the REAL protocol of this session — every translation path below reads
	// sess.hostProto, never the global (two sessions may probe different results).
	sess.hostProto = serverConn.Proto().ID()
	if packs := serverConn.ResourcePacks(); len(packs) > 0 {
		hostPacks.Store(&packs)
		slog.Info("xvrelay: fetched packs from the host", "count", len(packs))
	}
	slog.Info("xvrelay: dial world OK", "hostProto", sess.hostProto,
		"clientProto", client.Proto().ID())

	// Spawn the bot into the host FIRST so that GameData() has real data.
	if err := serverConn.DoSpawn(); err != nil {
		slog.Error("xvrelay: bot spawn into the host failed", "error", err)
		return err
	}
	gd := serverConn.GameData()
	// ★ Do not let the client inherit the HOST's ItemRegistry. A client on another protocol
	// would see the host's item network IDs, so holding/placing/dropping items goes wrong
	// ("lost items"). Amber/Ouranos solve this with the client protocol's ItemTypeDictionary.
	// Which table to feed depends on whether the client codec translates ItemRegistry itself —
	// see clientItemRegistry. This used to call Items(clientProto) unconditionally, which would
	// make the registry-aware 1001 adapter drop the packet entirely.
	//
	// ★ SAME-WIRE BUG (measured 2026-11-05): this replacement used to be gated on
	// `clientProto != hostProto`. For a 1001 client into a 1001 host, gd.Items stayed
	// at the host's 1933-entry table, but the 1001 codec IS registry-aware and
	// ConvertFromLatest DROPS any ItemRegistry that is not the native 2193 table
	// (asserted in item_registry_test.go: "1001 codec correctly drops a non-native
	// ItemRegistry (0 packets)"). The client therefore received NO item table at all
	// and never sent RequestChunkRadius/SetLocalPlayerAsInitialised. The gate is
	// unnecessary: clientItemRegistry() already returns the client's OWN table for
	// pass-through codecs (a no-op when host and client share a protocol) and the
	// native table for registry-aware codecs. So run it unconditionally.
	items, source, replaced := gameDataItemsFor(client.Proto().ID(), gd.Items, keepHostItems)
	if replaced {
		slog.Info("xvrelay: ItemRegistry replaced",
			"clientProto", client.Proto().ID(), "source", source,
			"items", len(items), "hostItems", len(serverConn.GameData().Items))
	} else {
		slog.Warn("xvrelay: keeping the host's ItemRegistry", "clientProto", client.Proto().ID(), "source", source)
	}
	gd.Items = items
	if noItems {
		gd.Items = nil
		slog.Warn("xvrelay: -noitems ⇒ empty ItemRegistry (the listener sends its own, bypassing the relay)")
	}
	if err := configureBlockTranslation(sess, client.Proto().ID(), gd); err != nil {
		return fmt.Errorf("configure block translation: %w", err)
	}
	slog.Info("xvrelay: world data", "world", gd.WorldName,
		"baseGameVersion", gd.BaseGameVersion, "items", len(gd.Items), "blocks", len(gd.CustomBlocks))
	applyDimensionFallback(&gd)

	// ★ SHIELD ID (measured 2026-11-05 with cmd/clientclone + BDS 1.26.33.1):
	// protocol adapters append an 8-byte BlockingTick to every item stack whose
	// NetworkID equals ShieldID(). A listener-side Conn never receives an
	// ItemRegistry from upstream (the relay raw-forwards it as packet.Unknown),
	// so its shieldID stays 0 unless we set it. The client decodes with the
	// shield ID from the table IT received, i.e. the client protocol's own
	// table. Native 2193 shield=358, target 1001 shield=387: using 0 (or the
	// native 358) made CreativeContent/CraftingData decode short (EOF).
	if !noItems {
		if sid := clientShieldID(client.Proto().ID()); sid != 0 {
			client.SetShieldID(sid)
		}
	}

	// ★ THE RELAY MUST RUN CONCURRENTLY WITH StartGame — see spawnPrefixGate.
	var rg sync.WaitGroup
	rg.Add(2)
	// C->H opens IMMEDIATELY. A packet the client sends that the fork does not "expect" goes into
	// deferredPackets (an UNBOUNDED slice) and surfaces again through ReadPacket ⇒ nothing is lost.
	go func() { defer rg.Done(); relay(client, serverConn, "C->H", trace) }()
	go func() { defer rg.Done(); relayAfterSpawnPrefix(sess.gate, serverConn, client, trace) }()

	if err := client.StartGame(gd); err != nil {
		// ★ context.Cause decides WHO died first: go-raknet closing itself (closeImmediately)
		// gives cause=context.Canceled; minecraft.Conn.Close() gives net.ErrClosed; a Disconnect
		// packet gives DisconnectError. Log BOTH directions so nothing is guessed.
		slog.Error("xvrelay: StartGame to the client failed", "error", err,
			"clientCause", contextCause(client), "hostCause", contextCause(serverConn),
			"clientID113", cg.saw113.Load(), "elapsed", time.Since(started).Round(time.Millisecond))
		_ = client.Close()
		_ = serverConn.Close()
		rg.Wait()
		return err
	}
	slog.Info("xvrelay: ★ client has spawned into the world")
	// ★ OPEN THE H->C GATE UNCONDITIONALLY.
	//
	// The old gate only opened on an empty `CreativeContent` — but the fork only sends that packet
	// inside `handleRequestChunkRadius` (conn.go:1476), i.e. it depends on the client sending
	// RequestChunkRadius in time AND on PacketFunc matching the address. If it does not match, the
	// gate stays shut forever ⇒ `relayAfterSpawnPrefix` never runs ⇒ the client spawns but receives
	// no further packet from the host and then disconnects itself.
	//
	// Evidence: log 13:41:45–13:42:15 (world "My World", host1001). The client spawned at
	// 13:41:46.318, sent 582 PlayerAuthInput over ~28 s, there was NO "open H->C relay" line, NO
	// H->C trace/tally, then "relay end C->H … context canceled".
	// StartGame returning means the spawn prefix chain has been written ⇒ opening the gate is safe.
	sess.gate.signal()
	rg.Wait()
	return nil
}

// relayAfterSpawnPrefix waits for the gate before running the H->C relay.
func relayAfterSpawnPrefix(gate *spawnPrefixGate, from, to *minecraft.Conn, trace int) {
	select {
	case <-gate.done():
		slog.Info("xvrelay: opening the H->C relay", "client", to.RemoteAddr())
	case <-from.Context().Done():
		return
	case <-to.Context().Done():
		return
	}
	relay(from, to, "H->C", trace)
}

// prefetchHostPacks dials the host once to grab its packs and then closes, feeding
// FetchResourcePacks.
func prefetchHostPacks(ctx context.Context, ls *lansignal.Signaling, id uint64) {
	identity := login.IdentityData{DisplayName: "xvrelay-packs", Identity: uuid.New().String()}
	cd := login.ClientData{
		DeviceOS:         1,
		DeviceID:         login.DeviceID(strings.ReplaceAll(uuid.New().String(), "-", "")),
		LanguageCode:     "en_US",
		CurrentInputMode: 1,
	}
	start := time.Now()
	cap := &spawnCapture{}
	conn, err := dialHost(ctx, ls, id, cd, identity, cap)
	if g := cap.packs.Load(); g != nil {
		slog.Info("xvrelay: pack prefetch guard", "summary", g.summary())
		if aborted, reason := g.abortStatus(); aborted {
			slog.Warn("xvrelay: pack prefetch aborted by the guard", "reason", reason)
		}
	}
	if err != nil {
		slog.Warn("xvrelay: pack prefetch failed", "error", err, "elapsed", time.Since(start))
		return
	}
	defer conn.Close()
	packs := conn.ResourcePacks()
	if len(packs) == 0 {
		slog.Info("xvrelay: the host has no packs", "elapsed", time.Since(start))
		return
	}
	hostPacks.Store(&packs)
	slog.Info("xvrelay: pack prefetch done", "count", len(packs), "elapsed", time.Since(start))
}

// packCacheDir is where resource packs are cached between host dials.
func packCacheDir() string {
	dir := "log"
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Join(filepath.Dir(exe), "log")
	}
	return filepath.Join(dir, "pack-cache")
}

// setupLogging writes logs to `<exe dir>/log/xvrelay-<timestamp>.log` AND to stdout.
// Needed because a long relay session scrolls the console away, and copying from a terminal
// usually truncates.
func setupLogging() (io.Closer, string) {
	dir := "log"
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Join(filepath.Dir(exe), "log")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("xvrelay: cannot create the log directory", "dir", dir, "error", err)
		return nil, ""
	}
	name := filepath.Join(dir, "xvrelay-"+time.Now().Format("2006-01-02_15-04-05")+".log")
	f, err := os.Create(name)
	if err != nil {
		slog.Warn("xvrelay: cannot create the log file", "path", name, "error", err)
		return nil, ""
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, f),
		&slog.HandlerOptions{Level: slog.LevelInfo})))
	return f, name
}

// decodeStartGameFile prints the fields of a StartGame payload (2193) read from a file.
// Used to compare what xvrelay sends the client against what the old bridge sent (capture file).
func decodeStartGameFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		slog.Error("xvrelay: reading the file failed", "path", path, "error", err)
		return
	}
	sg, err := decodeRawStartGameWith(minecraft.DefaultProtocol, b)
	if err != nil {
		slog.Error("xvrelay: StartGame decode failed", "path", path, "len", len(b), "error", err)
		return
	}
	exps := make([]string, 0, len(sg.Experiments))
	for _, e := range sg.Experiments {
		exps = append(exps, e.Name)
	}
	slog.Info("StartGame trong file", "path", path, "len", len(b),
		"levelID", sg.LevelID, "worldName", sg.WorldName,
		"serverID", sg.ServerID, "worldID", sg.WorldID, "ownerID", sg.OwnerID,
		"scenarioID", sg.ScenarioID,
		"gameVersion", sg.GameVersion, "baseGameVersion", sg.BaseGameVersion,
		"generator", sg.Generator, "worldGameMode", sg.WorldGameMode,
		"playerGameMode", sg.PlayerGameMode, "dimension", sg.Dimension,
		"worldSeed", sg.WorldSeed, "time", sg.Time,
		"blocks", len(sg.Blocks), "hashIDs", sg.UseBlockNetworkIDHashes,
		"checksum", sg.ServerBlockStateChecksum,
		"experiments", strings.Join(exps, ","), "gameRules", len(sg.GameRules),
		"clientSideGeneration", sg.ClientSideGeneration, "editorWorldType", sg.EditorWorldType,
		"texturePackRequired", sg.TexturePackRequired,
		"educationFeatures", sg.EducationFeaturesEnabled, "multiPlayerGame", sg.MultiPlayerGame,
		"commandsEnabled", sg.CommandsEnabled, "serverChunkTickRadius", sg.ServerChunkTickRadius,
		"playerPermissions", sg.PlayerPermissions,
		"serverAuthoritativeInventory", sg.ServerAuthoritativeInventory,
		"difficulty", sg.Difficulty, "hardcore", sg.Hardcore,
		"spawnBiomeType", sg.SpawnBiomeType, "enchantmentSeed", sg.EnchantmentSeed)
}

// ── Host protocol cache on disk ──────────────────────────────────────────────────
//
// ★ Why this is needed: a Bedrock client has a ~10 s join timeout measured from connecting
// (measured 8.5–9.6 s across the 17:46–18:42 sessions). In-session auto-probing costs ~3 s plus
// dial plus spawn ⇒ the deadline is blown and the client disconnects itself. So the protocol must
// be known BEFORE a client connects.

type protoCacheEntry struct {
	NetworkID   uint64 `json:"network_id"`
	Address     string `json:"address"`
	Protocol    int32  `json:"protocol"`
	GameVersion string `json:"game_version"`
}

func protoCachePath() string {
	dir := "."
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Dir(exe)
	}
	return filepath.Join(dir, "hostproto.json")
}

// loadProtoCache only matches by NETWORK ID (world instance).
//
// ★ It used to match by address too, to avoid probing after a host restart — WRONG: the host app
// reuses the same address 192.168.1.219:8554 for worlds of different versions. A new world
// (different network_id) would inherit the old world's protocol ⇒ dial with the wrong protocol ⇒
// hang until dialTimeout (log 21:57:36: elapsed exactly 1m30s, no StartGame).
func loadProtoCache(networkID uint64, address string) (int32, string, bool) {
	b, err := os.ReadFile(protoCachePath())
	if err != nil {
		return 0, "", false
	}
	var e protoCacheEntry
	if json.Unmarshal(b, &e) != nil || e.NetworkID != networkID {
		return 0, "", false
	}
	return e.Protocol, e.GameVersion, true
}

func saveProtoCache(networkID uint64, address string, proto int32, gameVersion string) {
	b, err := json.MarshalIndent(protoCacheEntry{networkID, address, proto, gameVersion}, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(protoCachePath(), b, 0o644); err != nil {
		slog.Warn("xvrelay: cannot write the protocol cache", "error", err)
	}
}

// probeHostProtoAtStartup probes the protocol BEFORE the listener opens so client sessions never
// have to wait. Costs ~3–5 s at startup in exchange for clients connecting inside the timeout.
func probeHostProtoAtStartup(ctx context.Context, ls *lansignal.Signaling, id uint64) (int32, string, error) {
	cap := &spawnCapture{}
	identity := login.IdentityData{DisplayName: "xvrelay-probe", Identity: uuid.New().String()}
	cd := login.ClientData{
		DeviceOS:         1,
		DeviceID:         login.DeviceID(strings.ReplaceAll(uuid.New().String(), "-", "")),
		LanguageCode:     "en_US",
		CurrentInputMode: 1,
	}
	// ★ The queue can PUSH the correct protocol to the front, same as the client path's `dialHost`.
	// This loop used to walk 2193→2169→1001→2168 with a 25 s timeout each: measured at 13:19:18 it
	// took 37 s (2193 rejected, 2169 rejected, 1001 ICE timeout 25 s, 2168 world gone) — beyond the
	// 15 s `peerTimeout`, so by the last dial the world had vanished.
	// The host declares its version on the very first dial (StartGame.GameVersion), so 2 rounds are
	// enough.
	const perDial = 12 * time.Second
	const budget = 22 * time.Second
	started := time.Now()
	queue := append([]int32(nil), hostProtoProbeOrder...)
	tried := make(map[int32]bool, len(queue))
	var lastErr error
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if tried[pid] {
			continue
		}
		tried[pid] = true
		if len(tried) > 1 {
			// The host (especially the host app) needs a few hundred ms to clean up the previous
			// session.
			select {
			case <-ctx.Done():
				return 0, "", ctx.Err()
			case <-time.After(700 * time.Millisecond):
			}
		}
		if time.Since(started) > budget {
			lastErr = fmt.Errorf("startup probe exceeded its %s budget", budget)
			slog.Warn("xvrelay: stopping the startup probe to open the listener in time", "elapsed", time.Since(started))
			break
		}
		cap.reset()
		conn, err := dialHostWith(ctx, ls, id, cd, identity, cap, pid, perDial)
		if err != nil {
			lastErr = err
			slog.Warn("xvrelay: startup probe failed", "protocol", pid, "error", err)
			continue
		}
		hv, ok := verifyHostProtocol(cap, pid)
		_ = conn.Close()
		if ok {
			if w, okw := worldByID(ls, id); okw && w.Addr != nil {
				rememberHostProto(w.Addr.String(), pid)
			}
			return pid, hv, nil
		}
		lastErr = fmt.Errorf("host declares %q but the dialled protocol is %d", hv, pid)
		slog.Warn("xvrelay: startup probe dropped the candidate", "protocol", pid, "hostGameVersion", hv)
		// ★ Jump STRAIGHT to the correct protocol instead of guessing through the rest.
		if want := protocolForVersion(hv); want != 0 && !tried[want] {
			queue = append([]int32{want}, queue...)
			slog.Info("xvrelay: startup probe prioritising the protocol inferred from the host version",
				"protocol", want, "version", hv)
		}
	}
	return 0, "", lastErr
}

// ── main ─────────────────────────────────────────────────────────────────────────

// buildVersion is stamped by the linker at release time:
//
//	go build -ldflags "-X main.buildVersion=1.0.0" ./cmd/xvrelay
//
// It stays "dev" for a plain `go build`, which is the honest answer for a working tree.
var buildVersion = "dev"

// directHostAddr is set by -host-raknet. When non-empty the relay skips LAN
// discovery entirely and dials the host over plain RakNet. That is what makes a
// dedicated server (BDS with transport=raknet) usable as a complete host for the
// offline self-check harness: no player-hosted NetherNet world and no Xbox Live
// session are needed.
var directHostAddr string

// mapGameplayItems controls whether the 2168/2169 codecs are built with
// NewWithRegistries. Registry-aware codecs translate item runtime IDs inside
// gameplay packets (InventoryContent, InventorySlot, MobEquipment, CraftingData,
// CreativeContent, ...) in BOTH directions. Plain New() passes those IDs
// straight through, so a 2168/2169 host leaves host IDs in the canonical 2193
// packets. It is opt-in because enabling it also changes ~20 packet families
// (block runtime IDs in UpdateBlock, transactions, PlayerAuthInput, ...).
var mapGameplayItems = true

// directWorld builds the synthetic World used in -host-raknet mode. The
// NetworkID is a stable hash of the address so the on-disk protocol cache does
// not collide with real LAN worlds. Version 5 mirrors the BDS ServerData v5 we
// measured (47 bytes, no nonce), so usableWorld() accepts it without a nonce.
func directWorld(addr string) lansignal.World {
	sum := md5.Sum([]byte(addr))
	id := binary.BigEndian.Uint64(sum[:8])
	if id == 0 {
		id = 1
	}
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		slog.Warn("xvrelay: cannot resolve -host-raknet address", "addr", addr, "error", err)
	}
	return lansignal.World{
		NetworkID: id,
		Addr:      uaddr,
		Host: &lansignal.HostInfo{
			Version:    5,
			ServerName: "Direct RakNet host",
			LevelName:  "Direct RakNet host",
		},
	}
}

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:19164", "address to listen on for clients")
	world := flag.String("world", "", "LAN world: level/host NAME (stable across re-opens) or NetworkID (empty = the only world found)")
	timeout := flag.Duration("timeout", 20*time.Second, "how long to scan for LAN worlds")
	trace := flag.Int("trace", 40, "print the first N packet names per direction (0 = off)")
	clientProtos := flag.String("client-proto", "auto",
		"client protocols to accept: auto = all (1001,2168,2169,2193) | list, e.g. 1001,2169")
	hostProto := flag.String("host-proto", "auto",
		"LAN world protocol: auto (read from LAN discovery) | 2193=1.26.51 | 2169=1.26.45 | 2168=1.26.44 | 1001=1.26.30-36")
	directHost := flag.String("host-raknet", "",
		"dial the host directly over RakNet (host:port) instead of NetherNet/LAN discovery; for a BDS with transport=raknet and the self-check harness")
	flag.BoolVar(&mapGameplayItems, "map-gameplay-items", true,
		"build the 2168/2169 codecs with NewWithRegistries so item IDs inside gameplay packets are translated (disable with -map-gameplay-items=false for bisect)")
	flag.StringVar(&startGameMode, "startgame", "raw", "raw (replay host bytes verbatim when the wire matches) | rebuild")
	flag.StringVar(&dimDataMode, "dimdata", "auto", "DimensionData: auto/off (keep the host's data) | force (diagnostic: inject a synthetic table)")
	flag.StringVar(&identityMode, "identity", "auto", "identity sent to the host: auto (the client's real name + XUID) | bot (the host sees Steve)")
	probe := flag.Bool("probe", false, "dial the world once and exit (diagnostic, no client needed)")
	probeXUID := flag.String("probe-xuid", "", "fake XUID used with -probe (empty = send no XUID)")
	probeVersion := flag.String("probe-version", "", "fake GameVersion used with -probe (empty = proto.Ver())")
	stripRegs := flag.Bool("stripregs", false, "bisect: drop registries (ItemRegistry/BiomeDefinitionList/AvailableActorIdentifiers) - ViaVersion's 'strip' strategy")
	stripItems := flag.Bool("stripitems", false, "bisect: drop inventory packets (1.26.33 item IDs are invalid for a 1.26.51 client)")
	stripActors := flag.Bool("stripactors", false, "bisect: drop entity/actor packets (1.26.33 metadata may differ from 1.26.51)")
	noChunks := flag.Bool("nochunks", false, "bisect: never send LevelChunk/SubChunk to the client")
	noSubChunks := flag.Bool("nosubchunk", false, "bisect: never send SubChunk (still send the LevelChunk shell)")
	onlyPrefixFlag := flag.Bool("onlyprefix", false, "bisect: drop EVERY relayed H->C packet, keep only the spawn prefix")
	noBlockTranslateFlag := flag.Bool("noblocktranslate", false, "bisect: disable the block palette translator (keep chunk framing)")
	noItemsFlag := flag.Bool("noitems", false, "bisect: send an EMPTY ItemRegistry to the client (emitted by the listener, -dropids cannot reach it)")
	keepItemsFlag := flag.Bool("keepitems", false, "bisect: keep the host's ItemRegistry instead of swapping in the client's table")
	noPushFlag := flag.Bool("nopush", false, "bisect: do not request terrain on the client's behalf (let the client send SubChunkRequest itself)")
	shellPayload := flag.Bool("shellpayload", false, "shell the request-mode LevelChunk sent to a cross-version client: true = empty payload")
	dumpSub := flag.String("dumpsubchunk", "", "write one host/client SubChunk payload pair plus the wire bytes for comparison")
	auditFlag := flag.Bool("audit", false, "diagnostic: re-parse LevelChunk/SubChunk SENT TO THE CLIENT and check state IDs against the target registry")
	verifyFlag := flag.Bool("verify", false, "diagnostic: encode then decode every H->C packet; packets that fail are DROPPED (not sent)")
	dialTimeoutFlag := flag.Duration("dial-timeout", 60*time.Second, "maximum time to dial the host (includes downloading resource packs)")
	dropIDsFlag := flag.String("dropids", "", "bisect: comma-separated packet IDs to drop when sending to the client, e.g. 162,122,119")
	dumpWireIDsFlag := flag.String("dumpwire", "", "diagnostic: write the payload SENT TO THE CLIENT for the given packet IDs (e.g. 61,174,161) to log/wire-<id>-<ts>.bin for decoding with a real client codec")
	dumpStart := flag.String("dumpstartgame", "", "write the StartGame payload SENT TO THE CLIENT to a file (diagnostic)")
	decodeFile := flag.String("decodefile", "", "decode a StartGame payload from a file, print its fields, exit")
	versionFlag := flag.Bool("version", false, "print the build version and exit")
	packModeFlag := flag.String("pack-mode", "skip",
		"CONN-005 resource-pack policy: skip (default, never download) | download | limit (declared size <= -pack-max-mb)")
	packMaxMBFlag := flag.Float64("pack-max-mb", 8,
		"with -pack-mode limit: per-pack DECLARED size ceiling in MB (0 = no per-pack ceiling)")
	packBudgetMBFlag := flag.Float64("pack-budget-mb", 32,
		"hard ceiling on total resource-pack bytes accounted per dial in MB (0 = unlimited)")
	packOverrunFlag := flag.String("pack-overrun", "abort",
		"when the REAL size from ResourcePackDataInfo breaks a limit: abort (cancel dial) | report (log only)")
	packPrefetchFlag := flag.Bool("pack-prefetch", false,
		"dial the host once BEFORE opening the listener so the first client also receives the map's packs (needs -pack-mode download|limit)")
	flag.Parse()
	directHostAddr = *directHost

	if *versionFlag {
		fmt.Printf("xvrelay %s\n", buildVersion)
		return
	}

	onlyPrefix = *onlyPrefixFlag
	noBlockTranslate = *noBlockTranslateFlag
	noItems = *noItemsFlag
	keepHostItems = *keepItemsFlag
	noPushSubChunks = *noPushFlag
	emptyShellPayload = *shellPayload
	auditOn = *auditFlag
	verifyOn = *verifyFlag
	if *dumpSub != "" {
		blocktranslate.DumpSubchunkPath = *dumpSub
	}
	dialTimeout = *dialTimeoutFlag
	// CONN-005: invalid pack flags always fall back to the SAFE policy (skip), never
	// to a download. A typo must not turn a relay into an unbounded pack downloader.
	{
		mode, err := parsePackMode(*packModeFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "xvrelay:", err, "— falling back to skip")
			mode = packModeSkip
		}
		overrun, err := parsePackOverrun(*packOverrunFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "xvrelay:", err, "— falling back to abort")
			overrun = packOverrunAbort
		}
		maxMB, budgetMB := *packMaxMBFlag, *packBudgetMBFlag
		if maxMB < 0 || budgetMB < 0 {
			fmt.Fprintln(os.Stderr, "xvrelay: -pack-max-mb / -pack-budget-mb must be >= 0 — clamping to 0")
			if maxMB < 0 {
				maxMB = 0
			}
			if budgetMB < 0 {
				budgetMB = 0
			}
		}
		packCfg = packPolicy{Mode: mode, MaxMB: maxMB, BudgetMB: budgetMB,
			Overrun: overrun, Prefetch: *packPrefetchFlag}
		if *packPrefetchFlag && !prefetchEnabled(packCfg) {
			fmt.Fprintln(os.Stderr, "xvrelay: -pack-prefetch needs -pack-mode download|limit — prefetch stays off")
		}
		slog.Info("xvrelay: resource-pack policy", "mode", mode, "maxMB", maxMB,
			"budgetMB", budgetMB, "overrun", overrun, "prefetch", prefetchEnabled(packCfg))
	}
	if *dumpWireIDsFlag != "" {
		dumpWireIDs = make(map[uint32]bool)
		for _, part := range strings.Split(*dumpWireIDsFlag, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, err := strconv.ParseUint(part, 10, 32)
			if err != nil {
				slog.Warn("xvrelay: -dumpwire skipping an invalid ID", "id", part, "error", err)
				continue
			}
			dumpWireIDs[uint32(n)] = true
		}
		slog.Info("xvrelay: -dumpwire will write the payload of these packet IDs", "ids", *dumpWireIDsFlag)
	}
	if *stripActors {
		if dropIDs == nil {
			dropIDs = make(map[uint32]bool)
		}
		// 13 AddActor · 111 MoveActorDelta · 39 SetActorData · 40 SetActorMotion · 165 SyncActorProperty
		for _, id := range []uint32{13, 111, 39, 40, 165} {
			dropIDs[id] = true
		}
		slog.Info("xvrelay: -stripactors ⇒ dropping entity/actor packets")
	}
	if *stripItems {
		if dropIDs == nil {
			dropIDs = make(map[uint32]bool)
		}
		// 49 InventoryContent · 50 InventorySlot · 31 MobEquipment · 52 CraftingData · 148 ItemStackResponse
		for _, id := range []uint32{49, 50, 31, 52, 148} {
			dropIDs[id] = true
		}
		slog.Info("xvrelay: -stripitems ⇒ dropping inventory packets (item IDs differ across versions)")
	}
	if *stripRegs {
		if dropIDs == nil {
			dropIDs = make(map[uint32]bool)
		}
		// 162 ItemRegistry · 122 BiomeDefinitionList · 119 AvailableActorIdentifiers
		for _, id := range []uint32{162, 122, 119} {
			dropIDs[id] = true
		}
		slog.Info("xvrelay: -stripregs ⇒ dropping ItemRegistry/Biome/ActorIdentifiers")
	}
	dumpStartPath = *dumpStart
	if *decodeFile != "" {
		decodeStartGameFile(*decodeFile)
		return
	}

	dropChunks = *noChunks
	dropSubChunks = *noSubChunks
	if *dropIDsFlag != "" {
		dropIDs = make(map[uint32]bool)
		for _, s := range strings.Split(*dropIDsFlag, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				dropIDs[uint32(n)] = true
			}
		}
		slog.Info("xvrelay: dropping packets before sending to the client", "ids", *dropIDsFlag)
	}

	if *trace < 0 {
		*trace = 0
	}
	// Also write logs to a FILE (alongside stdout) — a long session scrolls the console away, but
	// the file can be re-read.
	if closer, path := setupLogging(); closer != nil {
		defer closer.Close()
		slog.Info("xvrelay: log file", "path", path)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 1. Discovery — WAIT for a world instead of exiting. A LAN host is usually not open yet at
	// startup, and the process used to die immediately ⇒ the relay had to be restarted by hand
	// every time.
	var ls *lansignal.Signaling
	var w lansignal.World
	if directHostAddr != "" {
		// Still create a Signaling object: dialHostWith -> worldByID asks it for
		// Worlds(), and the remembered synthetic world in lastWorlds is returned
		// from there. NoBroadcast keeps us off UDP 7551 so a real LAN host is not
		// disturbed.
		var err error
		ls, err = lansignal.Listen(lansignal.Options{
			Addr: "0.0.0.0:0", NoBroadcast: true, Log: slog.Default(),
		})
		if err != nil {
			slog.Error("xvrelay: direct discovery socket failed", "error", err)
			os.Exit(1)
		}
		w = directWorld(directHostAddr)
		rememberWorld(w)
		slog.Info("xvrelay: direct RakNet host mode — LAN discovery skipped",
			"addr", directHostAddr, "network_id", w.NetworkID)
	} else {
		for attempt := 1; ; attempt++ {
			var err error
			ls, err = discoverWorlds(ctx, *timeout)
			if err == nil {
				w, err = pickWorld(ls, *world)
				if err == nil {
					break
				}
				slog.Warn("xvrelay: no world selected yet — retrying", "attempt", attempt,
					"error", err, "peers", ls.Peers())
				ls.Close()
			} else {
				slog.Warn("xvrelay: no LAN world yet — retrying", "attempt", attempt, "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}
	defer ls.Close()
	slog.Info("xvrelay: world selected", "network_id", w.NetworkID, "level", w.Host.LevelName,
		"host", w.Host.ServerName, "addr", w.Addr)
	// Remember the address up front: if the peer expires from the TTL while probing/dialling,
	// there is still a route to dial.
	rememberWorld(w)

	// ★ AUTO: the host protocol comes from ServerData (v7+). v5/v6 has none ⇒ it is probed at dial
	// time.
	hostProtoAuto = strings.EqualFold(strings.TrimSpace(*hostProto), "auto")
	hostProfileWorldID = w.NetworkID
	pid, known := resolveHostProto(*hostProto, w.Host.Protocol)
	hostProtoID.Store(pid)
	hostProtoKnown.Store(known)
	hostGameVersion = w.Host.GameVersion
	// 1) Disk cache — known immediately from the previous run, costing the client no time.
	if !known {
		if cp, cgv, ok := loadProtoCache(w.NetworkID, w.Addr.String()); ok {
			if _, okp := resolveProto(cp); okp {
				pid, known = cp, true
				hostProtoID.Store(cp)
				hostProtoKnown.Store(true)
				slog.Info("xvrelay: using the cached protocol", "protocol", cp, "gameVersion", cgv)
			}
		}
	}
	// 2) Probe BEFORE the listener opens. A client has a ~10 s join timeout; probing inside the
	// session would blow it.
	if !known && hostProtoAuto && !*probe {
		if pp, pgv, err := probeHostProtoAtStartup(ctx, ls, w.NetworkID); err == nil {
			pid, known = pp, true
			hostProtoID.Store(pp)
			hostProtoKnown.Store(true)
			saveProtoCache(w.NetworkID, w.Addr.String(), pp, pgv)
			slog.Info("xvrelay: ★ startup probe picked the protocol before listening", "protocol", pp, "gameVersion", pgv)
		} else {
			slog.Warn("xvrelay: startup probe failed — will probe when a client connects", "error", err)
		}
	}
	if !known {
		slog.Info("xvrelay: host does NOT declare a protocol — will probe when a client connects",
			"probeOrder", hostProtoProbeOrder)
	}
	slog.Info("xvrelay: target world", "network_id", w.NetworkID, "level", w.Host.LevelName,
		"addr", w.Addr, "protocol", w.Host.Protocol, "hostProtoUsed", pid,
		"protoKnown", known, "gameVersion", hostGameVersion, "nonce", w.Host.Nonce != "")

	// ── Diagnostic: dial once and exit, no client needed ─────────────────────────────
	if *probe {
		pid := login.IdentityData{DisplayName: "Probe", Identity: uuid.New().String(), XUID: *probeXUID}
		pcd := login.ClientData{
			DeviceOS:         1,
			DeviceID:         login.DeviceID(strings.ReplaceAll(uuid.New().String(), "-", "")),
			LanguageCode:     "en_US",
			CurrentInputMode: 1,
			GameVersion:      *probeVersion,
		}
		cap := &spawnCapture{}
		conn, err := dialHost(ctx, ls, w.NetworkID, pcd, pid, cap)
		if err != nil {
			slog.Error("xvrelay: PROBE FAILED", "error", err,
				"xuid", *probeXUID, "version", *probeVersion)
			os.Exit(1)
		}
		defer conn.Close()
		gd := conn.GameData()
		slog.Info("xvrelay: PROBE SUCCEEDED", "world", gd.WorldName,
			"baseGameVersion", gd.BaseGameVersion, "items", len(gd.Items),
			"blocks", len(gd.CustomBlocks), "proto", conn.Proto().ID(),
			"xuid", *probeXUID, "version", *probeVersion)
		// ★ Also exercise the REAL StartGame decode path (unwrapping the wrapper) — this decides
		// whether the client receives a complete StartGame.
		if raw := cap.startGame.Load(); raw != nil {
			hostCodec, _ := resolveProto(conn.Proto().ID())
			if sg, err := decodeRawStartGameWith(hostCodec, *raw); err == nil {
				slog.Info("xvrelay: PROBE StartGame",
					"rawBytes", len(*raw), "levelID", sg.LevelID, "serverID", sg.ServerID,
					"worldID", sg.WorldID, "ownerID", sg.OwnerID, "scenarioID", sg.ScenarioID,
					"enchantmentSeed", sg.EnchantmentSeed, "generator", sg.Generator,
					"blocks", len(sg.Blocks), "hashIDs", sg.UseBlockNetworkIDHashes,
					"checksum", sg.ServerBlockStateChecksum,
					"texturePackRequired", sg.TexturePackRequired)
			} else {
				slog.Warn("xvrelay: PROBE StartGame decode failed", "error", err)
			}
		}
		os.Exit(0)
	}

	// 2. The list of codecs the client accepts.
	protoList := *clientProtos
	if strings.EqualFold(protoList, "auto") {
		protoList = "1001,2168,2169"
	}
	var accepted []minecraft.Protocol
	seen := map[int32]bool{}
	add := func(p minecraft.Protocol, source string) {
		if p == nil || seen[p.ID()] {
			return
		}
		seen[p.ID()] = true
		accepted = append(accepted, p)
		slog.Info("xvrelay: accepting client codec", "id", p.ID(), "ver", p.Ver(), "source", source)
	}
	for _, s := range strings.Split(protoList, ",") {
		id, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			slog.Warn("xvrelay: skipping an invalid -client-proto", "value", s)
			continue
		}
		p, ok := resolveProto(int32(id))
		if !ok {
			slog.Warn("xvrelay: no codec for this protocol", "id", id)
			continue
		}
		add(p, "flag")
	}
	// ★ MANDATORY: 2193 must be present BEFORE wrapping. The listener always appends `proto{}`
	// (ID 2193) and selectProtocol takes the FIRST ID match ⇒ without 2193 the wrapper is shadowed.
	native, _ := resolveProto(2193)
	add(native, "native (mandatory)")
	for i, p := range accepted {
		accepted[i] = wrapClientVersion(p, true)
	}

	// Directional quality warnings — so logs are not misread during an incident.
	// (only printed when already known; otherwise dialHost prints after the first probe)
	if known {
		switch pid {
		case 1001:
			slog.Info("xvrelay: host protocol1001; cross-version terrain uses pinned block palette mapping")
		case 2169:
			slog.Info("xvrelay: host is protocol 2169 (1.26.45) — full wire adapter")
		}
	}

	// 3. Listener.
	listenCfg := minecraft.ListenConfig{
		AuthenticationDisabled: true,
		AcceptedProtocols:      accepted,
		ErrorLog:               slog.Default().With("link", "client"),
		AllowUnknownPackets:    true,
		AllowInvalidPackets:    true,
		PacketFunc:             clientPacketFunc,
		// ★ Serve the HOST's packs to the client. A map with a resource pack whose pack the client
		// never receives ⇒ wrong textures/models/sounds, and the client is easily disconnected
		// ("Bat").
		// Packs come from the most recent host dial (hostPacks) — empty if there has not been one.
		FetchResourcePacks: func(id login.IdentityData, cd login.ClientData, current []*resource.Pack) []*resource.Pack {
			p := hostPacks.Load()
			if p == nil || len(*p) == 0 {
				slog.Warn("xvrelay: no host packs available to serve the client", "clientVersion", cd.GameVersion)
				return current
			}
			slog.Info("xvrelay: serving the host's packs to the client",
				"count", len(*p), "clientVersion", cd.GameVersion)
			return append(append([]*resource.Pack(nil), current...), *p...)
		},
	}
	// ★ Prefetch the host's packs BEFORE opening the listener: FetchResourcePacks runs while the
	// client is logging in, and at that moment the dial has not happened yet ⇒ the first client
	// would get no packs. One extra dial here (packs are cached in log/pack-cache) buys every
	// client the map's correct packs.
	// CONN-005: the old hardcoded `if false` is gone. Prefetch is now opt-in behind
	// -pack-prefetch AND requires a download policy; the per-dial pack guard bounds the
	// cost (declared size, REAL size, budget, overrun abort). Default = off = old behaviour.
	if prefetchEnabled(packCfg) {
		prefetchHostPacks(ctx, ls, w.NetworkID)
	}
	listener, err := listenCfg.Listen("raknet", *listenAddr)
	if err != nil {
		slog.Error("xvrelay: listen failed", "error", err)
		os.Exit(1)
	}
	defer listener.Close()
	slog.Info("xvrelay: listening", "addr", *listenAddr,
		"protocols", func() []int32 {
			out := make([]int32, 0, len(accepted))
			for _, p := range accepted {
				out = append(out, p.ID())
			}
			return out
		}())

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		c, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("xvrelay: accept failed", "error", err)
			continue
		}
		conn, ok := c.(*minecraft.Conn)
		if !ok {
			_ = c.Close()
			continue
		}
		go func(conn *minecraft.Conn) {
			current := w
			if directHostAddr == "" {
				var err error
				current, err = pickWorld(ls, *world)
				if err != nil {
					slog.Error("xvrelay: selecting the current world failed", "error", err)
					_ = conn.Close()
					return
				}
			}
			slog.Info("xvrelay: world selected for incoming client", "network_id", current.NetworkID, "level", current.Host.LevelName, "host", current.Host.ServerName)
			if err := handleConn(ctx, conn, ls, current.NetworkID, *trace); err != nil {
				slog.Error("xvrelay: session error", "remote", conn.RemoteAddr(), "error", err)
			}
		}(conn)
	}
}
