// Command selfcheck is a headless terminal Bedrock client used to independently
// exercise a running xvrelay listener. It connects with a selectable client
// protocol, spawns, counts gameplay packets and writes a JSON report.
//
// Two ingress paths are supported: the default RakNet dial (-addr), and
// -lan-join, which discovers the relay's world card on UDP 7551 and joins over
// NetherNet/WebRTC exactly like a game client using the LAN world list
// (CONN-002). It deliberately does not talk to the real game, Xbox Live or any
// GUI: it is the independent client used in place of a game client for live
// measurements.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"bedrock-cross-version-relay/internal/blocktranslate"
	"bedrock-cross-version-relay/internal/lansignal"

	"github.com/df-mc/go-nethernet"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	v1_26_30 "github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
	v1_26_44 "github.com/shawtymarco/go-multiversion/protocols/v1_26_44"
	v1_26_45 "github.com/shawtymarco/go-multiversion/protocols/v1_26_45"
)

// defaultDrainWindow is how long a session keeps reading packets when -soak is
// not set. Spawn-time packets (StartGame, registries, chunks) arrive over the
// first seconds after DoSpawn, so a bounded drain is needed to observe them.
const defaultDrainWindow = 3 * time.Second

// readDeadlineSlice bounds each individual ReadPacket call. A short slice keeps
// the reader goroutine responsive to the session's stop time and guarantees the
// harness never hangs on an idle or half-closed connection.
const readDeadlineSlice = 200 * time.Millisecond

// cpuNote is recorded verbatim in every report: process-level CPU usage cannot
// be sampled portably across platforms, so no CPU number is invented.
const cpuNote = "not measured: process-level CPU is not portable across platforms"

type config struct {
	addr         string
	proto        int32
	spawnTimeout time.Duration
	soak         time.Duration
	reconnect    int
	jsonPath     string
	verbose      bool

	// LAN/NetherNet join (CONN-002): when lanJoin is true selfcheck behaves like
	// a real game client — it discovers the relay's world card on UDP 7551 and
	// dials its network ID over WebRTC instead of dialling RakNet on -addr.
	lanJoin    bool
	lanAddr    string
	networkID  string
	lanTimeout time.Duration

	// interactions sends the C2H packet families a real GUI client uses to
	// place/click blocks and use spawn eggs, plus ClientMovementPredictionSync.
	// It exercises the relay's read/convert path for those packets live.
	interactions bool
}

var verbose bool

func vlog(format string, args ...any) {
	if verbose {
		fmt.Fprintf(os.Stderr, "[selfcheck] "+format+"\n", args...)
	}
}

type itemSample struct {
	Name      string `json:"name"`
	RuntimeID int16  `json:"runtime_id"`
}

type heapSample struct {
	OffsetMS       int64  `json:"offset_ms"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	NumGoroutine   int    `json:"num_goroutine"`
	// NumGC distinguishes a real retained-heap leak from plain GC pacing: if Alloc keeps
	// rising while NumGC also rises, live objects are actually accumulating.
	NumGC uint32 `json:"num_gc"`
}

type assertion struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type session struct {
	Proto             int32             `json:"proto"`
	Addr              string            `json:"addr"`
	Timestamp         string            `json:"timestamp"`
	SpawnMS           int64             `json:"spawn_ms"`
	LifetimeMS        int64             `json:"lifetime_ms"`
	PacketsTotal      uint64            `json:"packets_total"`
	Packets           map[string]uint64 `json:"packets"`
	GameDataItems     int               `json:"game_data_items"`
	Items             []itemSample      `json:"items"`
	ItemRegistryMatch bool              `json:"item_registry_match"`
	Chunks            uint64            `json:"chunks"`
	SubChunks         uint64            `json:"subchunks"`
	DisconnectReason  string            `json:"disconnect_reason"`
	HeapAllocBytes    uint64            `json:"heap_alloc_bytes"`
	NumGoroutine      int               `json:"num_goroutine"`
	HeapSamples       []heapSample      `json:"heap_samples,omitempty"`
	CPU               string            `json:"cpu"`
	Assertions        []assertion       `json:"assertions"`
	OK                bool              `json:"ok"`
	Error             string            `json:"error,omitempty"`
}

// trackedPacket ties a human-readable counter name to the uint32 packet ID
// reported by Conn.ReadPacket(). The go-multiversion codecs expose their pools
// under the same latest packet.ID* constants, so one table covers all protocols.
type trackedPacket struct {
	name string
	id   uint32
}

var trackedPackets = []trackedPacket{
	{"LevelChunk", packet.IDLevelChunk},
	{"SubChunk", packet.IDSubChunk},
	{"SubChunkRequest", packet.IDSubChunkRequest},
	{"ItemRegistry", packet.IDItemRegistry},
	{"BiomeDefinitionList", packet.IDBiomeDefinitionList},
	{"AvailableActorIdentifiers", packet.IDAvailableActorIdentifiers},
	{"CraftingData", packet.IDCraftingData},
	{"CreativeContent", packet.IDCreativeContent},
	{"InventoryContent", packet.IDInventoryContent},
	{"InventorySlot", packet.IDInventorySlot},
	{"MobEquipment", packet.IDMobEquipment},
	{"AddActor", packet.IDAddActor},
	{"SetActorData", packet.IDSetActorData},
	{"UpdateBlock", packet.IDUpdateBlock},
	{"ResourcePackStack", packet.IDResourcePackStack},
	{"ResourcePackDataInfo", packet.IDResourcePackDataInfo},
	{"ResourcePackChunkData", packet.IDResourcePackChunkData},
	{"PlayerAuthInput", packet.IDPlayerAuthInput},
}

type packetCounter struct {
	mu       sync.Mutex
	counts   map[string]uint64
	idToName map[uint32]string
	total    uint64
	unknown  uint64
}

func newPacketCounter() *packetCounter {
	c := &packetCounter{counts: make(map[string]uint64), idToName: make(map[uint32]string)}
	for _, tp := range trackedPackets {
		c.counts[tp.name] = 0
		c.idToName[tp.id] = tp.name
	}
	return c
}

func (c *packetCounter) add(id uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
	if name, ok := c.idToName[id]; ok {
		c.counts[name]++
	} else {
		c.unknown++
	}
}

func (c *packetCounter) snapshot() (map[string]uint64, uint64, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]uint64, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out, c.total, c.unknown
}

// sameItemIDs compares the (name, runtimeID) pairs of two item registries.
// Re-implemented locally from cmd/xvrelay/item_registry_test.go so selfcheck
// stays an independent binary (no test-only package imports).
func sameItemIDs(a, b []protocol.ItemEntry) bool {
	if len(a) != len(b) {
		return false
	}
	ids := make(map[string]int16, len(b))
	for _, e := range b {
		ids[e.Name] = e.RuntimeID
	}
	for _, e := range a {
		if id, ok := ids[e.Name]; !ok || id != e.RuntimeID {
			return false
		}
	}
	return true
}

func codecFor(id int32) (minecraft.Protocol, error) {
	switch id {
	case 1001:
		return v1_26_30.New(), nil
	case 2168:
		return v1_26_44.New(), nil
	case 2169:
		return v1_26_45.New(), nil
	case 2193:
		return minecraft.DefaultProtocol, nil
	default:
		return nil, fmt.Errorf("invalid -proto %d (valid: 1001|2168|2169|2193)", id)
	}
}

func isDeadline(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(strings.ToLower(err.Error()), "deadline exceeded")
}

// sendInteractions writes every C2H packet family measured in the live GUI
// session of 21:02-21:08, plus the other families a player touches while
// placing/clicking blocks and using spawn eggs (hotbar equipment, entity
// interaction, breaking animation, inventory requests, legacy item particles).
// Two of these used to be silently dropped ("place block / click block / spawn
// egg does nothing") or rejected by the host writer ("bitset size mismatch");
// the rest are sent so the relay's ConvertToLatest read path is exercised on a
// real host, where codecDropLogs would warn if any adapter returned nothing.
func sendInteractions(conn *minecraft.Conn, protoID int32) error {
	flagCount := protocol.EntityDataFlagCount
	if protoID == 1001 {
		flagCount = 130 // the 1001 wire layout has 130 entity data flags
	}
	for i := 0; i < 3; i++ {
		if err := conn.WritePacket(&packet.ClientMovementPredictionSync{
			ActorFlags: protocol.NewBitset(flagCount),
		}); err != nil {
			return fmt.Errorf("ClientMovementPredictionSync #%d: %w", i, err)
		}
	}

	reg, err := blocktranslate.New(protoID, protoID, true, nil)
	if err != nil {
		return err
	}
	blockID, _ := reg.SourceID("minecraft:stone", nil)
	tx := protocol.UseItemTransactionData{
		ActionType:     protocol.UseItemActionClickBlock,
		BlockRuntimeID: blockID,
		BlockPosition:  protocol.BlockPos{0, -60, 0},
		BlockFace:      1,
	}
	for i := 0; i < 2; i++ {
		if err := conn.WritePacket(&packet.InventoryTransaction{TransactionData: &tx}); err != nil {
			return fmt.Errorf("InventoryTransaction #%d: %w", i, err)
		}
		if err := conn.WritePacket(&packet.PlayerAuthInput{ItemInteractionData: protocol.Option(tx)}); err != nil {
			return fmt.Errorf("PlayerAuthInput #%d: %w", i, err)
		}
	}

	// The remaining families are deliberately benign probes: runtime IDs of 0
	// and empty request slices do not ask the host to mutate the world, but they
	// still pass through the relay's ConvertToLatest / codec path. A real GUI
	// client sends these alongside the UseItem packets above.
	extra := []struct {
		name string
		pk   packet.Packet
	}{
		{"MobEquipment", &packet.MobEquipment{
			EntityRuntimeID: 1,
			NewItem: protocol.ItemInstance{
				Stack: protocol.ItemStack{BlockRuntimeID: int32(blockID)},
			},
		}},
		{"Interact", &packet.Interact{ActionType: packet.InteractActionOpenInventory}},
		{"PlayerAction", &packet.PlayerAction{EntityRuntimeID: 1, BlockFace: 1}},
		{"Animate", &packet.Animate{ActionType: packet.AnimateActionSwingArm, EntityRuntimeID: 1}},
		{"ItemStackRequest", &packet.ItemStackRequest{}},
		{"SubChunkRequest", &packet.SubChunkRequest{}},
		{"SetPlayerInventoryOptions", &packet.SetPlayerInventoryOptions{}},
		{"LevelEvent", &packet.LevelEvent{
			EventType: packet.ParticleTypeIconCrack,
			EventData: int32(blockID),
		}},
	}
	for _, e := range extra {
		if err := conn.WritePacket(e.pk); err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
	}
	vlog("session: sent 3 prediction sync + 2 InventoryTransaction + 2 PlayerAuthInput + %d other C2H families", len(extra))
	return nil
}

// dialRelay bounds the dial both by ctx and by a hard wall-clock limit, so a
// missing/unreachable relay always produces a clear error instead of a hang.
// The returned cleanup (if any) must stay alive until the session finished
// reading: for a LAN join it owns the signalling socket that carries the
// WebRTC negotiation and the data channel.
func dialRelay(ctx context.Context, cfg config, codec minecraft.Protocol, name string) (*minecraft.Conn, func(), error) {
	if cfg.lanJoin {
		return dialLAN(ctx, cfg, codec, name)
	}
	d := minecraft.Dialer{
		Protocol:                   codec,
		ClientData:                 login.ClientData{GameVersion: codec.Ver()},
		IdentityData:               login.IdentityData{DisplayName: name, Identity: uuid.New().String()},
		DisconnectOnUnknownPackets: false,
	}
	type result struct {
		conn *minecraft.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := d.DialContext(ctx, "raknet", cfg.addr)
		ch <- result{conn: c, err: err}
	}()
	hard := cfg.spawnTimeout + 500*time.Millisecond
	select {
	case r := <-ch:
		return r.conn, nil, r.err
	case <-time.After(hard):
		return nil, nil, fmt.Errorf("hard dial timeout after %s: no listener at %s", hard, cfg.addr)
	}
}

// lanNetwork decouples the NetherNet network ID from ClientData.ServerAddress.
// gophertunnel's DialContextNetwork reuses ONE string for both
// Network.DialContext and ClientData.ServerAddress, and the listener validates
// ServerAddress as a resolvable UDP address. A decimal network ID (what LAN
// discovery gives us) fails that validation, so DialContext is overridden to
// use the real network ID while the dial address stays a valid IP:port.
// Real game clients send their own IP:port in ServerAddress and dial the
// network ID out of band; only this headless client needs the shim.
type lanNetwork struct {
	minecraft.NetherNet
	networkID string
}

func (n lanNetwork) DialContext(ctx context.Context, _ string) (net.Conn, error) {
	return n.NetherNet.DialContext(ctx, n.networkID)
}

// isLocalDiscoveryAddr reports whether a LAN peer answered from this machine's
// own IP (or loopback) on the discovery port. The relay's ResponsePacket is
// sourced from the outbound interface IP even when we probed 127.0.0.1, so
// matching the -lan-addr string exactly is too strict; other LAN worlds (the
// host machine) have a different IP and must NOT be dialled by mistake.
// When the relay runs on another machine, pass -network-id explicitly.
func isLocalDiscoveryAddr(a net.Addr, port int) bool {
	u, ok := a.(*net.UDPAddr)
	if !ok || u.Port != port {
		return false
	}
	if u.IP.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, ia := range addrs {
		if ipnet, ok := ia.(*net.IPNet); ok && ipnet.IP.Equal(u.IP) {
			return true
		}
	}
	return false
}

// dialLAN is the CONN-002 path: discover the relay's advertised world on UDP
// 7551, then join it over NetherNet/WebRTC exactly as a LAN game client would.
func dialLAN(ctx context.Context, cfg config, codec minecraft.Protocol, name string) (*minecraft.Conn, func(), error) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if verbose {
		log = slog.Default()
	}
	relayAddr, err := net.ResolveUDPAddr("udp", cfg.lanAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("bad -lan-addr %q: %w", cfg.lanAddr, err)
	}

	// Bind an ephemeral socket: the relay already owns UDP 7551 on this machine.
	// ExtraBroadcasts sends our RequestPacket directly to the relay's socket.
	ls, err := lansignal.Listen(lansignal.Options{
		Addr:            "0.0.0.0:0",
		Log:             log,
		ExtraBroadcasts: []*net.UDPAddr{relayAddr},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("LAN signalling socket: %w", err)
	}
	cleanup := func() { _ = ls.Close() }

	id := cfg.networkID
	if id == "" {
		deadline := time.Now().Add(cfg.lanTimeout)
		for id == "" {
			// Worlds() includes BOTH the relay and any other LAN world (e.g. the
			// host machine's own world). Pick a peer that answered from this
			// machine's own IP on the discovery port, otherwise we would dial
			// the wrong network ID. Exact loopback match is tried first.
			for _, w := range ls.Worlds() {
				if w.Addr != nil && w.Addr.String() == relayAddr.String() {
					id = strconv.FormatUint(w.NetworkID, 10)
					break
				}
			}
			if id == "" {
				for _, w := range ls.Worlds() {
					if w.Addr != nil && isLocalDiscoveryAddr(w.Addr, relayAddr.Port) {
						id = strconv.FormatUint(w.NetworkID, 10)
						break
					}
				}
			}
			if id != "" || time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, cleanup, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if id == "" {
		seen := make([]string, 0)
		for _, w := range ls.Worlds() {
			seen = append(seen, fmt.Sprintf("%d@%v", w.NetworkID, w.Addr))
		}
		return nil, cleanup, fmt.Errorf("no LAN world advertised by %s within %s (is the relay running with -listen-nethernet?); peers seen: %v",
			cfg.lanAddr, cfg.lanTimeout, seen)
	}
	vlog("session: LAN discovered relay network_id=%s addr=%s", id, cfg.lanAddr)

	d := minecraft.Dialer{
		Protocol:                   codec,
		ClientData:                 login.ClientData{GameVersion: codec.Ver()},
		IdentityData:               login.IdentityData{DisplayName: name, Identity: uuid.New().String()},
		DisconnectOnUnknownPackets: false,
		ErrorLog:                   log,
	}
	type result struct {
		conn *minecraft.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := d.DialContextNetwork(ctx, lanNetwork{
			NetherNet: minecraft.NetherNet{
				Signaling: ls,
				Log:       log,
				Dialer: nethernet.Dialer{
					// The relay is self-signed / anonymous (AllowAnonymous=true).
					AllowIdentitylessServer: true,
					DisableTrickleICE:       true,
				},
			},
			networkID: id,
		}, "127.0.0.1:19132")
		ch <- result{conn: c, err: err}
	}()
	hard := cfg.spawnTimeout + 500*time.Millisecond
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, cleanup, r.err
		}
		return r.conn, cleanup, nil
	case <-time.After(hard):
		return nil, cleanup, fmt.Errorf("hard LAN dial timeout after %s (network_id=%s)", hard, id)
	}
}

func runSession(cfg config, codec minecraft.Protocol, index int) session {
	start := time.Now()
	s := session{
		Proto:            cfg.proto,
		Addr:             cfg.addr,
		Timestamp:        start.UTC().Format(time.RFC3339Nano),
		Packets:          make(map[string]uint64),
		Items:            make([]itemSample, 0),
		CPU:              cpuNote,
		DisconnectReason: "not connected",
	}
	for _, tp := range trackedPackets {
		s.Packets[tp.name] = 0
	}

	names := []string{"connect", "spawn", "item_registry", "level_chunk", "packets_total"}
	if cfg.interactions {
		names = append(names, "interactions_sent")
	}
	if cfg.soak > 0 {
		names = append(names, "soak_no_early_disconnect")
	}
	checks := make(map[string]assertion, len(names))
	for _, n := range names {
		checks[n] = assertion{Name: n, OK: false, Detail: "not evaluated"}
	}
	setCheck := func(name string, ok bool, detail string) {
		checks[name] = assertion{Name: name, OK: ok, Detail: detail}
	}
	finish := func() session {
		for _, n := range names {
			s.Assertions = append(s.Assertions, checks[n])
		}
		s.OK = s.Error == ""
		for _, a := range s.Assertions {
			if !a.OK {
				s.OK = false
			}
		}
		return s
	}

	name := fmt.Sprintf("SelfCheck%d", index+1)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), cfg.spawnTimeout)
	defer dialCancel()

	if cfg.lanJoin {
		vlog("session %d: joining over NetherNet LAN discovery (%s) as protocol %d (%s)", index+1, cfg.lanAddr, cfg.proto, codec.Ver())
	} else {
		vlog("session %d: dialing %s as protocol %d (%s)", index+1, cfg.addr, cfg.proto, codec.Ver())
	}
	conn, cleanup, err := dialRelay(dialCtx, cfg, codec, name)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		target := cfg.addr
		if cfg.lanJoin {
			target = cfg.lanAddr + " (nethernet LAN join)"
		}
		s.Error = fmt.Sprintf("dial %s: %v", target, err)
		s.DisconnectReason = s.Error
		setCheck("connect", false, s.Error)
		reason := "skipped: connect failed"
		for _, n := range names[1:] {
			setCheck(n, false, reason)
		}
		vlog("session %d: %s", index+1, s.Error)
		return finish()
	}
	defer conn.Close()
	setCheck("connect", true, "RakNet dial succeeded")

	// 1. DoSpawn within -spawn-timeout.
	spawnCtx, spawnCancel := context.WithTimeout(context.Background(), cfg.spawnTimeout)
	spawnStart := time.Now()
	spawnErr := conn.DoSpawnContext(spawnCtx)
	spawnCancel()
	s.SpawnMS = time.Since(spawnStart).Milliseconds()
	if spawnErr != nil {
		s.Error = fmt.Sprintf("DoSpawn: %v", spawnErr)
		s.DisconnectReason = s.Error
		setCheck("spawn", false, s.Error)
		reason := "skipped: DoSpawn failed"
		for _, n := range names[2:] {
			setCheck(n, false, reason)
		}
		vlog("session %d: %s", index+1, s.Error)
		return finish()
	}
	setCheck("spawn", true, fmt.Sprintf("spawned in %d ms (limit %s)", s.SpawnMS, cfg.spawnTimeout))
	vlog("session %d: spawned in %d ms", index+1, s.SpawnMS)

	// GameData().Items: count plus the full name -> runtimeID list.
	gd := conn.GameData()
	s.GameDataItems = len(gd.Items)
	s.Items = make([]itemSample, 0, len(gd.Items))
	for _, e := range gd.Items {
		s.Items = append(s.Items, itemSample{Name: e.Name, RuntimeID: e.RuntimeID})
	}
	want, wantErr := blocktranslate.Items(cfg.proto)
	switch {
	case wantErr != nil:
		s.ItemRegistryMatch = false
		setCheck("item_registry", false, fmt.Sprintf("blocktranslate.Items(%d): %v", cfg.proto, wantErr))
	case len(gd.Items) == 0:
		s.ItemRegistryMatch = false
		setCheck("item_registry", false, "GameData().Items is empty")
	default:
		s.ItemRegistryMatch = sameItemIDs(gd.Items, want)
		if s.ItemRegistryMatch {
			setCheck("item_registry", true, fmt.Sprintf("%d entries match blocktranslate.Items(%d)", len(gd.Items), cfg.proto))
		} else {
			setCheck("item_registry", false, fmt.Sprintf("%d GameData entries differ from the %d pinned %d entries", len(gd.Items), len(want), cfg.proto))
		}
	}

	if cfg.interactions {
		if err := sendInteractions(conn, cfg.proto); err != nil {
			vlog("session %d: interaction write failed: %v", index+1, err)
			setCheck("interactions_sent", false, err.Error())
		} else {
			setCheck("interactions_sent", true, "3 prediction sync + 2 use-item transactions + 2 auth-input interactions + 8 other C2H families")
		}
	}

	// Read packets in a goroutine with short, refreshed deadlines until the
	// session window ends. Sample heap/goroutines every second meanwhile.
	window := cfg.soak
	if window <= 0 {
		window = defaultDrainWindow
	}
	stopAt := time.Now().Add(window)
	vlog("session %d: reading packets for %s", index+1, window)

	stopSamples := make(chan struct{})
	sampleDone := make(chan struct{})
	var sampleMu sync.Mutex
	var samples []heapSample
	takeSample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		sampleMu.Lock()
		samples = append(samples, heapSample{
			OffsetMS:       time.Since(start).Milliseconds(),
			HeapAllocBytes: m.Alloc,
			NumGoroutine:   runtime.NumGoroutine(),
			NumGC:          m.NumGC,
		})
		sampleMu.Unlock()
	}
	takeSample()
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				takeSample()
			case <-stopSamples:
				takeSample()
				return
			}
		}
	}()

	counter := newPacketCounter()
	readDone := make(chan error, 1)
	go func() {
		for {
			remaining := time.Until(stopAt)
			if remaining <= 0 {
				readDone <- nil
				return
			}
			dl := readDeadlineSlice
			if remaining < dl {
				dl = remaining
			}
			if dl < time.Millisecond {
				dl = time.Millisecond
			}
			if err := conn.SetReadDeadline(time.Now().Add(dl)); err != nil {
				readDone <- err
				return
			}
			pk, err := conn.ReadPacket()
			if err != nil {
				if isDeadline(err) {
					if time.Now().Before(stopAt) {
						continue
					}
					readDone <- nil
					return
				}
				readDone <- err
				return
			}
			counter.add(pk.ID())
		}
	}()

	readErr := <-readDone
	close(stopSamples)
	<-sampleDone

	counts, total, unknown := counter.snapshot()
	s.Packets = counts
	s.PacketsTotal = total
	s.Chunks = counts["LevelChunk"]
	s.SubChunks = counts["SubChunk"]
	s.LifetimeMS = time.Since(start).Milliseconds()

	sampleMu.Lock()
	if n := len(samples); n > 0 {
		s.HeapSamples = samples
		s.HeapAllocBytes = samples[n-1].HeapAllocBytes
		s.NumGoroutine = samples[n-1].NumGoroutine
	}
	sampleMu.Unlock()

	switch {
	case readErr != nil:
		s.DisconnectReason = readErr.Error()
	case cfg.soak > 0:
		s.DisconnectReason = "soak_complete"
	default:
		s.DisconnectReason = "drain_complete"
	}

	setCheck("level_chunk", s.Chunks > 0, fmt.Sprintf("LevelChunk=%d (must be > 0)", s.Chunks))
	setCheck("packets_total", total > 0, fmt.Sprintf("packets_total=%d (must be > 0)", total))
	if cfg.soak > 0 {
		if readErr != nil {
			setCheck("soak_no_early_disconnect", false,
				fmt.Sprintf("disconnected after %d ms, before %s soak ended: %v", s.LifetimeMS, cfg.soak, readErr))
		} else {
			setCheck("soak_no_early_disconnect", true,
				fmt.Sprintf("stayed connected for %d ms (soak %s)", s.LifetimeMS, cfg.soak))
		}
	}

	if cfg.verbose {
		for _, tp := range trackedPackets {
			vlog("session %d: %-28s %d", index+1, tp.name, s.Packets[tp.name])
		}
		vlog("session %d: total=%d unknown=%d items=%d heap=%d goroutines=%d reason=%s",
			index+1, total, unknown, s.GameDataItems, s.HeapAllocBytes, s.NumGoroutine, s.DisconnectReason)
	}
	return finish()
}

func printSummary(sessions []session, cfg config) {
	fmt.Printf("selfcheck: %d session(s), addr=%s proto=%d spawn-timeout=%s soak=%s json=%q\n",
		len(sessions), cfg.addr, cfg.proto, cfg.spawnTimeout, cfg.soak, cfg.jsonPath)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tPROTO\tSPAWN_MS\tLIFETIME_MS\tPACKETS\tCHUNKS\tSUBCHUNKS\tITEMS\tREGISTRY\tDISCONNECT\tOK")
	for i, s := range sessions {
		fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%t\t%s\t%t\n",
			i+1, s.Proto, s.SpawnMS, s.LifetimeMS, s.PacketsTotal, s.Chunks, s.SubChunks,
			s.GameDataItems, s.ItemRegistryMatch, s.DisconnectReason, s.OK)
	}
	_ = w.Flush()
	fmt.Printf("note: %s (heap_alloc_bytes / num_goroutine are sampled every 1s)\n", cpuNote)
}

func run() int {
	var (
		addr         = flag.String("addr", "127.0.0.1:19164", "RakNet listener address of the relay")
		proto        = flag.Int("proto", 2193, "client protocol (1001|2168|2169|2193)")
		spawnTimeout = flag.Duration("spawn-timeout", 15*time.Second, "timeout for dial + DoSpawn")
		soak         = flag.Duration("soak", 0, "keep the connection and read packets for this long (>0 enables the soak assertion)")
		reconnect    = flag.Int("reconnect", 0, "number of additional reconnects after the initial session (total = 1 + N)")
		jsonPath     = flag.String("json", "", "write the JSON session report to this path")
		verboseFlag  = flag.Bool("v", false, "verbose logging to stderr")
		lanJoin      = flag.Bool("lan-join", false, "CONN-002: join over NetherNet/WebRTC LAN discovery like a game client (requires the relay to run with -listen-nethernet); -addr is ignored")
		lanAddr      = flag.String("lan-addr", "127.0.0.1:7551", "UDP signalling address of the relay when -lan-join is set")
		networkID    = flag.String("network-id", "", "relay network ID for -lan-join (empty = discover it from the relay's LAN ResponsePacket)")
		lanTimeout   = flag.Duration("lan-timeout", 6*time.Second, "how long to wait for the relay's LAN world card when -lan-join is set")
		interactions = flag.Bool("interactions", false, "after spawn, send the C2H interaction packets (prediction sync, ClickBlock transaction, PlayerAuthInput.ItemInteractionData) to exercise the relay translation live")
	)
	flag.Parse()
	verbose = *verboseFlag

	cfg := config{
		addr:         *addr,
		proto:        int32(*proto),
		spawnTimeout: *spawnTimeout,
		soak:         *soak,
		reconnect:    *reconnect,
		jsonPath:     *jsonPath,
		verbose:      *verboseFlag,
		lanJoin:      *lanJoin,
		lanAddr:      *lanAddr,
		networkID:    *networkID,
		lanTimeout:   *lanTimeout,
		interactions: *interactions,
	}
	if cfg.spawnTimeout <= 0 || cfg.soak < 0 || cfg.reconnect < 0 {
		fmt.Fprintln(os.Stderr, "selfcheck: -spawn-timeout must be > 0, -soak and -reconnect must be >= 0")
		return 1
	}
	codec, err := codecFor(cfg.proto)
	if err != nil {
		fmt.Fprintln(os.Stderr, "selfcheck:", err)
		return 1
	}

	attempts := 1 + cfg.reconnect
	sessions := make([]session, 0, attempts)
	for i := 0; i < attempts; i++ {
		s := runSession(cfg, codec, i)
		sessions = append(sessions, s)
		for _, a := range s.Assertions {
			if !a.OK {
				fmt.Fprintf(os.Stderr, "FAIL assertion: %s: %s\n", a.Name, a.Detail)
			}
		}
		if s.Error != "" {
			fmt.Fprintf(os.Stderr, "selfcheck: session %d: %s\n", i+1, s.Error)
		}
	}

	if cfg.jsonPath != "" {
		data, err := json.MarshalIndent(sessions, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "selfcheck: marshal report: %v\n", err)
			return 1
		}
		if err := os.WriteFile(cfg.jsonPath, append(data, '\n'), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "selfcheck: write %s: %v\n", cfg.jsonPath, err)
			return 1
		}
		fmt.Printf("selfcheck: report written to %s\n", cfg.jsonPath)
	}

	printSummary(sessions, cfg)
	for _, s := range sessions {
		if !s.OK {
			return 1
		}
	}
	return 0
}

func main() { os.Exit(run()) }
