package main

// Headless, terminal-only Bedrock client behavior. The reactions below mirror
// what a real client must do after DoSpawnContext(): gophertunnel stops
// auto-handling packets once loggedIn is true (conn.go:687), so the client must
// echo latency, answer forms, request sub-chunks and keep sending movement
// input itself. The exact shapes were measured on a real host and written up
// in reverse-engineering/TPBOT-CLIENT-PROFILE.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

const (
	inputPeriod = 50 * time.Millisecond  // 20 Hz, one PlayerAuthInput per tick
	chunkPeriod = 100 * time.Millisecond // batched SubChunkRequest flushes
	readSlice   = 200 * time.Millisecond
)

// errorCounter captures gophertunnel's internal decode errors. ReadPacket()
// logs a failed decode and silently retries, so without this hook a corrupted
// relayed packet would be invisible to a self-check.
type errorCounter struct {
	mu    sync.Mutex
	count uint64
	last  string
}

func (h *errorCounter) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelError
}
func (h *errorCounter) Handle(_ context.Context, r slog.Record) error {
	// Only count packet read/decode failures. gophertunnel logs those as
	// "read packet: ..." and then silently retries, so this is the only way a
	// self-check can see a corrupted relayed packet.
	if !strings.Contains(r.Message, "read packet:") && !strings.Contains(strings.ToLower(r.Message), "decode") {
		return nil
	}
	h.mu.Lock()
	h.count++
	h.last = r.Message
	h.mu.Unlock()
	return nil
}
func (h *errorCounter) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *errorCounter) WithGroup(_ string) slog.Handler      { return h }
func (h *errorCounter) snapshot() (uint64, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count, h.last
}

type chunkKey struct {
	x, z, dim int32
}

// packetTraceEntry records one raw packet header seen by gophertunnel's
// PacketFunc. parseData() calls it for incoming packets, WritePacket() for
// outgoing ones; src/dst let us tell them apart. This is how the self-check
// names the exact packet that fails to decode instead of only counting errors.
type packetTraceEntry struct {
	id      uint32
	length  int
	hex     string
	raw     []byte
	src     string
	dst     string
	inbound bool
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}

type session struct {
	cfg  config
	name string

	conn       *minecraft.Conn
	codec      minecraft.Protocol
	gd         minecraft.GameData
	errCounter *errorCounter

	ctx    context.Context
	cancel context.CancelFunc
	dead   chan struct{}
	wg     sync.WaitGroup

	mu sync.Mutex
	// World state, updated from server packets.
	pos      mgl32.Vec3
	pitch    float32
	yaw      float32
	headYaw  float32
	dim      int32
	radius   int32
	subLimit int32
	onGround bool
	tick     uint64
	// Command-driven input state.
	wantJump    bool
	sneaking    bool
	sprinting   bool
	hotbarSlot  byte
	pendingChat []string
	// Counters.
	packets        map[string]uint64
	packetsTotal   uint64
	unknownPackets uint64
	sent           map[string]uint64
	sentTotal      uint64
	sendErrors     uint64
	latencyEcho    uint64
	forms          uint64
	chunkRequests  uint64
	authInput      uint64
	chunkQ         []chunkKey
	chunkSeen      map[chunkKey]bool
	// Decode-failure diagnostics.
	packetTrace  []packetTraceEntry
	badPackets   uint64
	lastBadID    uint32
	lastBadLen   int
	lastBadHex   string
	lastBadError string
	// Session facts.
	joined           bool
	spawned          bool
	spawnMS          int64
	start            time.Time
	proto            int32
	addr             string
	direct           bool
	disconnectReason string
	errorText        string

	// Live wire item census (#19). itemsReady is set before the read loop
	// starts, so observeItems() can cheaply check it without taking s.mu.
	items        itemStats
	itemsIndex   itemIndex
	itemsReady   atomic.Bool
	itemLogCount int
}

func newSession(cfg config) *session {
	ctx, cancel := context.WithCancel(context.Background())
	return &session{
		cfg:        cfg,
		name:       cfg.name,
		ctx:        ctx,
		cancel:     cancel,
		dead:       make(chan struct{}),
		errCounter: &errorCounter{},
		packets:    make(map[string]uint64),
		sent:       make(map[string]uint64),
		chunkSeen:  make(map[chunkKey]bool),
		items:      newItemStats(),
		start:      time.Now(),
		hotbarSlot: 0,
	}
}

func packetName(pk packet.Packet) string {
	t := fmt.Sprintf("%T", pk)
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimPrefix(t, "*")
}

func isDeadline(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(strings.ToLower(err.Error()), "deadline exceeded")
}

func classifyDisconnect(err error) string {
	var de minecraft.DisconnectError
	if errors.As(err, &de) {
		return fmt.Sprintf("host_disconnect: %q", string(de))
	}
	if errors.Is(err, net.ErrClosed) {
		return "transport_closed"
	}
	if strings.Contains(strings.ToLower(err.Error()), "context canceled") {
		return "client_canceled"
	}
	return "read_error: " + err.Error()
}

// join dials a RakNet endpoint (a relay listener, or a BDS with
// transport=raknet via -direct) and finishes the spawn sequence.
func (s *session) join(addr string, direct bool, protoOverride int32, nameOverride string) error {
	s.mu.Lock()
	if s.joined {
		s.mu.Unlock()
		return fmt.Errorf("already joined %s", s.addr)
	}
	s.mu.Unlock()

	if nameOverride != "" {
		s.name = nameOverride
	}
	protoID := s.cfg.proto
	if protoOverride != 0 {
		protoID = protoOverride
	}
	codec, err := codecFor(protoID)
	if err != nil {
		return err
	}
	s.codec = codec

	dialCtx, dialCancel := context.WithTimeout(s.ctx, s.cfg.spawnTimeout)
	defer dialCancel()
	d := minecraft.Dialer{
		Protocol:                   codec,
		ClientData:                 clientData(s.name),
		IdentityData:               login.IdentityData{DisplayName: s.name, Identity: uuid.New().String()},
		DisconnectOnUnknownPackets: false,
		ErrorLog:                   slog.New(s.errCounter),
	}
	// PacketFunc is invoked by parseData() for inbound packets and by
	// WritePacket() for outbound ones. Keep a bounded ring so a decode error can
	// be attributed to the exact packet ID.
	d.PacketFunc = func(h packet.Header, payload []byte, src, dst net.Addr) {
		n := len(payload)
		if n > 32 {
			n = 32
		}
		raw := append([]byte(nil), payload...)
		if len(raw) > 65536 {
			raw = raw[:65536]
		}
		s.mu.Lock()
		s.packetTrace = append(s.packetTrace, packetTraceEntry{
			id: h.PacketID, length: len(payload), hex: fmt.Sprintf("%x", payload[:n]), raw: raw,
			src: addrString(src), dst: addrString(dst),
		})
		// Spawn-time packets (CraftingData/CreativeContent) arrive in the first
		// few hundred packets; keep enough history to match them by ID later.
		if len(s.packetTrace) > 8192 {
			s.packetTrace = append([]packetTraceEntry(nil), s.packetTrace[len(s.packetTrace)-4096:]...)
		}
		s.mu.Unlock()
	}

	type dialResult struct {
		conn *minecraft.Conn
		err  error
	}
	ch := make(chan dialResult, 1)
	go func() {
		c, e := d.DialContext(dialCtx, "raknet", addr)
		ch <- dialResult{conn: c, err: e}
	}()
	var conn *minecraft.Conn
	select {
	case r := <-ch:
		conn, err = r.conn, r.err
	case <-time.After(s.cfg.spawnTimeout + 500*time.Millisecond):
		return fmt.Errorf("hard dial timeout: no listener at %s", addr)
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	s.conn = conn

	spawnCtx, spawnCancel := context.WithTimeout(s.ctx, s.cfg.spawnTimeout)
	spawnStart := time.Now()
	spawnErr := conn.DoSpawnContext(spawnCtx)
	spawnCancel()
	s.mu.Lock()
	s.spawnMS = time.Since(spawnStart).Milliseconds()
	s.mu.Unlock()
	if spawnErr != nil {
		_ = conn.Close()
		return fmt.Errorf("DoSpawn after %d ms: %w", s.spawnMS, spawnErr)
	}

	s.gd = conn.GameData()

	// Resolve live item IDs against the CLIENT's own protocol table: the relay
	// encodes target-protocol IDs on the wire. Build the index before starting
	// the read loop so observeItems() never sees a half-built map.
	if idx, err := newItemIndex(protoID); err != nil {
		slog.Warn("clientclone: item index unavailable", "proto", protoID, "error", err)
	} else {
		s.itemsIndex = idx
		s.itemsReady.Store(true)
		fmt.Fprintf(os.Stderr, "clientclone: item index ready proto=%d entries=%d (live wire IDs resolve against this table)\n",
			protoID, idx.count)
	}

	s.mu.Lock()
	s.pos = s.gd.PlayerPosition
	s.pitch, s.yaw = s.gd.Pitch, s.gd.Yaw
	s.headYaw = s.gd.Yaw
	s.dim = s.gd.Dimension
	s.radius = s.gd.ChunkRadius
	s.joined = true
	s.spawned = true
	s.start = time.Now()
	s.proto = protoID
	s.addr = addr
	s.direct = direct
	s.disconnectReason = "connected"
	s.mu.Unlock()

	s.startBackground()
	return nil
}

func (s *session) startBackground() {
	s.wg.Add(3)
	go s.readLoop()
	if !s.cfg.noInput {
		go s.inputLoop()
	}
	if !s.cfg.noSubchunk {
		go s.chunkLoop()
	}
	if s.cfg.noInput {
		s.wg.Done()
	}
	if s.cfg.noSubchunk {
		s.wg.Done()
	}
}

func (s *session) readLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(readSlice))
		before, _ := s.errCounter.snapshot()
		pk, err := s.conn.ReadPacket()
		after, lastErr := s.errCounter.snapshot()
		if after > before {
			s.recordDecodeFailures(after-before, lastErr)
		}
		if err != nil {
			if isDeadline(err) {
				continue
			}
			reason := classifyDisconnect(err)
			s.mu.Lock()
			s.disconnectReason = reason
			s.mu.Unlock()
			fmt.Fprintf(os.Stderr, "clientclone: read stopped: %s\n", reason)
			select {
			case <-s.dead:
			default:
				close(s.dead)
			}
			return
		}
		s.countPacket(pk)
		s.observeItems(pk)
		s.handle(pk)
	}
}

// recordDecodeFailures attributes a gophertunnel read/decode error to the last
// inbound raw packet header. Without this, ReadPacket() logs and silently drops
// the packet, so a self-check only sees "2 errors" and cannot name the culprit.
func (s *session) recordDecodeFailures(n uint64, last string) {
	s.mu.Lock()
	s.badPackets += n
	s.lastBadError = last
	remote := addrString(s.conn.RemoteAddr())
	// The fork's decode error now carries the real packet ID. PacketFunc logs
	// whole batches as they arrive, so "last inbound" alone can point at a later
	// packet; match on the ID from the error instead.
	idFromErr := uint32(0)
	if i := strings.Index(last, "(ID="); i >= 0 {
		rest := last[i+4:]
		if j := strings.IndexByte(rest, ')'); j > 0 {
			if v, e := strconv.ParseUint(rest[:j], 10, 32); e == nil {
				idFromErr = uint32(v)
			}
		}
	}
	var raw []byte
	for i := len(s.packetTrace) - 1; i >= 0; i-- {
		if e := s.packetTrace[i]; e.id == idFromErr || (idFromErr == 0 && e.src == remote) {
			s.lastBadID, s.lastBadLen, s.lastBadHex = e.id, e.length, e.hex
			raw = e.raw
			break
		}
	}
	if idFromErr != 0 {
		s.lastBadID = idFromErr
	}
	id, length, hex := s.lastBadID, s.lastBadLen, s.lastBadHex
	s.mu.Unlock()
	if len(raw) > 0 {
		path := fmt.Sprintf("dist/badpacket-%d.bin", id)
		if err := os.WriteFile(path, raw, 0644); err == nil {
			fmt.Fprintf(os.Stderr, "clientclone: dumped %d raw payload bytes to %s\n", len(raw), path)
		}
	}
	fmt.Fprintf(os.Stderr, "clientclone: DECODE_ERROR id=%d payload_len=%d hex=%s count=%d err=%s\n", id, length, hex, n, last)
}

func (s *session) countPacket(pk packet.Packet) {
	name := packetName(pk)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.packetsTotal++
	s.packets[name]++
	if _, ok := pk.(*packet.Unknown); ok {
		s.unknownPackets++
	}
}

func (s *session) send(pk packet.Packet) error {
	if s.conn == nil {
		return errors.New("not joined")
	}
	name := packetName(pk)
	s.mu.Lock()
	s.sentTotal++
	s.sent[name]++
	s.mu.Unlock()
	if err := s.conn.WritePacket(pk); err != nil {
		s.mu.Lock()
		s.sendErrors++
		s.mu.Unlock()
		return err
	}
	return nil
}

// handle implements the post-spawn reactions of a real client.
func (s *session) handle(pk packet.Packet) {
	switch p := pk.(type) {
	case *packet.NetworkStackLatency:
		if p.NeedsResponse {
			if s.send(&packet.NetworkStackLatency{Timestamp: p.Timestamp}) == nil {
				s.mu.Lock()
				s.latencyEcho++
				s.mu.Unlock()
			}
		}
	case *packet.ModalFormRequest:
		if s.send(&packet.ModalFormResponse{
			FormID:       p.FormID,
			ResponseData: protocol.Option([]byte("null")),
		}) == nil {
			s.mu.Lock()
			s.forms++
			s.mu.Unlock()
		}
	case *packet.LevelChunk:
		if limit, ok := p.SubChunkLimit.Value(); ok && p.SubChunkCount == 0 {
			s.mu.Lock()
			s.subLimit = limit
			key := chunkKey{p.Position.X(), p.Position.Z(), p.Dimension}
			if !s.chunkSeen[key] {
				s.chunkSeen[key] = true
				s.chunkQ = append(s.chunkQ, key)
			}
			s.mu.Unlock()
		}
	case *packet.ChunkRadiusUpdated:
		s.mu.Lock()
		s.radius = p.ChunkRadius
		s.mu.Unlock()
	case *packet.ChangeDimension:
		s.mu.Lock()
		s.dim, s.pos = p.Dimension, p.Position
		s.mu.Unlock()
		_ = s.send(&packet.PlayerAction{
			EntityRuntimeID: s.gd.EntityRuntimeID,
			ActionType:      protocol.PlayerActionDimensionChangeDone,
		})
	case *packet.Respawn:
		if p.State == packet.RespawnStateReadyToSpawn {
			s.mu.Lock()
			s.pos = p.Position
			s.mu.Unlock()
			_ = s.send(&packet.Respawn{
				Position:        p.Position,
				State:           packet.RespawnStateClientReadyToSpawn,
				EntityRuntimeID: s.gd.EntityRuntimeID,
			})
		}
	case *packet.MovePlayer:
		if p.EntityRuntimeID == s.gd.EntityRuntimeID {
			s.mu.Lock()
			s.pos, s.pitch, s.yaw, s.onGround = p.Position, p.Pitch, p.Yaw, p.OnGround
			s.mu.Unlock()
		}
	case *packet.CorrectPlayerMovePrediction:
		s.mu.Lock()
		s.pos, s.onGround = p.Position, p.OnGround
		s.mu.Unlock()
	}
}

// inputPacket builds one PlayerAuthInput tick. InputData is present but has no
// flags set by default: a zero flag count is valid for every protocol, whereas
// setting newer flag IDs against an older server can trip UnknownEnumOption.
func (s *session) inputPacket() *packet.PlayerAuthInput {
	s.mu.Lock()
	s.tick++
	pitch, yaw, pos, tick := s.pitch, s.yaw, s.pos, s.tick
	jump, sneak, sprint := s.wantJump, s.sneaking, s.sprinting
	s.wantJump = false
	s.mu.Unlock()

	pk := &packet.PlayerAuthInput{
		Pitch:            pitch,
		Yaw:              yaw,
		HeadYaw:          yaw,
		Position:         pos,
		PlayMode:         packet.PlayModeNormal,
		InputMode:        packet.InputModeTouch,
		InteractionModel: packet.InteractionModelTouch,
		InteractPitch:    pitch,
		InteractYaw:      yaw,
		Tick:             tick,
		InputData:        protocol.NewInputFlags(packet.InputFlagCount),
	}
	cy, sy := math.Cos(float64(yaw)), math.Sin(float64(yaw))
	cp, sp := math.Cos(float64(pitch)), math.Sin(float64(pitch))
	pk.CameraOrientation = mgl32.Vec3{float32(-sy * cp), float32(-sp), float32(cy * cp)}

	if s.cfg.inputFlags {
		f := protocol.NewInputFlags(packet.InputFlagCount)
		f.Set(packet.InputFlagVerticalCollision)
		if jump {
			f.Set(packet.InputFlagStartJumping)
		}
		if sneak {
			f.Set(packet.InputFlagSneaking)
			f.Set(packet.InputFlagPersistSneak)
		}
		if sprint {
			f.Set(packet.InputFlagSprinting)
			f.Set(packet.InputFlagStartSprinting)
		}
		pk.InputData = f
	}
	return pk
}

func (s *session) inputLoop() {
	defer s.wg.Done()
	t := time.NewTicker(inputPeriod)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			if s.send(s.inputPacket()) == nil {
				s.mu.Lock()
				s.authInput++
				s.mu.Unlock()
			}
		}
	}
}

// dimRange is the requested sub-chunk column. Overworld 1.26 is -64..320;
// nether/end are smaller. Values are chunk indices, not block coordinates.
func dimRange(dim int32) (minY, height int32) {
	switch dim {
	case packet.DimensionNether:
		return 0, 128
	case packet.DimensionEnd:
		return 0, 256
	default:
		return -64, 384
	}
}

func (s *session) sendChunkRequest(c chunkKey) {
	minY, height := dimRange(c.dim)
	first := minY >> 4
	last := (minY + height - 1) >> 4

	s.mu.Lock()
	cy := int32(math.Floor(float64(s.pos.Y()))) >> 4
	limit := s.subLimit
	s.mu.Unlock()
	if cy < first {
		cy = first
	}
	if cy > last {
		cy = last
	}

	offs := make([]protocol.SubChunkOffset, 0, last-first+1)
	for y := first; y <= last; y++ {
		offs = append(offs, protocol.SubChunkOffset{0, int8(y - cy), 0})
	}
	step := len(offs)
	if limit > 0 && int(limit) < step {
		step = int(limit)
	}
	for i := 0; i < len(offs); i += step {
		end := i + step
		if end > len(offs) {
			end = len(offs)
		}
		if err := s.send(&packet.SubChunkRequest{
			Dimension: c.dim,
			Offsets:   offs[i:end],
			Position:  protocol.SubChunkPos{c.x, cy, c.z},
		}); err != nil {
			return
		}
		s.mu.Lock()
		s.chunkRequests++
		s.mu.Unlock()
	}
}

func (s *session) chunkLoop() {
	defer s.wg.Done()
	t := time.NewTicker(chunkPeriod)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			for i := 0; i < 8; i++ {
				s.mu.Lock()
				if len(s.chunkQ) == 0 {
					s.mu.Unlock()
					break
				}
				c := s.chunkQ[0]
				s.chunkQ = s.chunkQ[1:]
				s.mu.Unlock()
				s.sendChunkRequest(c)
			}
		}
	}
}

// stop cancels background loops and closes the connection exactly once.
func (s *session) stop() {
	s.cancel()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.wg.Wait()
}

// snapshot returns the live metric view used by commands and the final report.
func (s *session) snapshot() map[string]any {
	// itemSummary takes s.mu itself, so collect it before locking here.
	itemSnap := s.itemSummary()
	s.mu.Lock()
	defer s.mu.Unlock()
	decodeErrors, lastDecodeErr := s.errCounter.snapshot()
	m := map[string]any{
		"joined":             s.joined,
		"spawned":            s.spawned,
		"spawn_ms":           s.spawnMS,
		"alive_ms":           time.Since(s.start).Milliseconds(),
		"proto":              s.proto,
		"addr":               s.addr,
		"direct":             s.direct,
		"name":               s.name,
		"game_data_items":    len(s.gd.Items),
		"entity_runtime_id":  s.gd.EntityRuntimeID,
		"world_name":         s.gd.WorldName,
		"base_game_version":  s.gd.BaseGameVersion,
		"chunk_radius":       s.radius,
		"position":           []float32{s.pos.X(), s.pos.Y(), s.pos.Z()},
		"tick":               s.tick,
		"packets_total":      s.packetsTotal,
		"unknown_packets":    s.unknownPackets,
		"sent_total":         s.sentTotal,
		"send_errors":        s.sendErrors,
		"decode_errors":      decodeErrors,
		"bad_packets":        s.badPackets,
		"last_bad_packet_id": s.lastBadID,
		"last_bad_len":       s.lastBadLen,
		"last_bad_hex":       s.lastBadHex,
		"last_bad_error":     s.lastBadError,
		"latency_echo":       s.latencyEcho,
		"forms":              s.forms,
		"chunk_requests":     s.chunkRequests,
		"auth_input":         s.authInput,
		"disconnect_reason":  s.disconnectReason,
		"error":              s.errorText,
	}
	for k, v := range s.packets {
		m["packet_"+k] = v
	}
	for k, v := range s.sent {
		m["sent_"+k] = v
	}
	for k, v := range itemSnap {
		m[k] = v
	}
	if lastDecodeErr != "" {
		m["last_decode_error"] = lastDecodeErr
	}
	return m
}

// snapshotJSON is used by the `dump` command response.
func (s *session) snapshotJSON() json.RawMessage {
	b, _ := json.Marshal(s.snapshot())
	return b
}
