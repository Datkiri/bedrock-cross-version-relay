// Command clientclone is a terminal-only, command-driven Bedrock client.
//
// It exists to test a relay (or a BDS directly) without a real game client and
// without a GUI. It is the "custom headless client" counterpart to Cinnabar's
// Go core (core/cmd/bedrock-core + core/proxy/local_bds_live_test.go): that
// project is licensed PolyForm Noncommercial, so no code is copied here —
// clientclone uses this repository's own gophertunnel-2193 codecs and only
// reuses the measured client behavior (see behave.go).
//
// Drive it with one JSON command per line, from -script or stdin:
//
//	{"cmd":"join","addr":"127.0.0.1:19164","proto":1001}
//	{"cmd":"wait","ms":3000}
//	{"cmd":"chat","message":"hello"}
//	{"cmd":"assert","metric":"packet_LevelChunk","op":">","value":0}
//	{"cmd":"dump"}
//	{"cmd":"quit"}
//
// Every command answers with one JSON line. The final JSON report (with -json)
// contains assertions that fail when the session is broken.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"bedrock-cross-version-relay/internal/blocktranslate"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	v1_26_30 "github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
	v1_26_44 "github.com/shawtymarco/go-multiversion/protocols/v1_26_44"
	v1_26_45 "github.com/shawtymarco/go-multiversion/protocols/v1_26_45"
)

type config struct {
	addr         string
	direct       bool
	proto        int32
	name         string
	script       string
	jsonPath     string
	duration     time.Duration
	spawnTimeout time.Duration
	auto         bool
	verbose      bool
	noInput      bool
	noSubchunk   bool
	inputFlags   bool
	noFail       bool
	itemLog      int // max item log lines; 0 = none, <0 = unlimited
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

type assertion struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type report struct {
	Tool              string            `json:"tool"`
	Timestamp         string            `json:"timestamp"`
	Proto             int32             `json:"proto"`
	Addr              string            `json:"addr"`
	Direct            bool              `json:"direct"`
	Name              string            `json:"name"`
	Spawned           bool              `json:"spawned"`
	SpawnMS           int64             `json:"spawn_ms"`
	AliveMS           int64             `json:"alive_ms"`
	GameDataItems     int               `json:"game_data_items"`
	ItemRegistryMatch bool              `json:"item_registry_match"`
	Packets           map[string]uint64 `json:"packets"`
	PacketsTotal      uint64            `json:"packets_total"`
	UnknownPackets    uint64            `json:"unknown_packets"`
	Sent              map[string]uint64 `json:"sent"`
	SentTotal         uint64            `json:"sent_total"`
	SendErrors        uint64            `json:"send_errors"`
	DecodeErrors      uint64            `json:"decode_errors"`
	BadPackets        uint64            `json:"bad_packets"`
	LastBadPacketID   uint32            `json:"last_bad_packet_id"`
	LastBadLen        int               `json:"last_bad_len"`
	LastBadError      string            `json:"last_bad_error,omitempty"`
	LatencyEcho       uint64            `json:"latency_echo"`
	Forms             uint64            `json:"forms"`
	ChunkRequests     uint64            `json:"chunk_requests"`
	AuthInput         uint64            `json:"auth_input"`
	Commands          int               `json:"commands"`
	CommandFailures   int               `json:"command_failures"`
	ItemsDecoded      uint64            `json:"items_decoded"`
	ItemsVisible      uint64            `json:"items_visible"`
	ItemsLost         uint64            `json:"items_lost"`
	ItemsUnknown      uint64            `json:"items_unknown"`
	ItemsEmptySlots   uint64            `json:"items_empty_slots"`
	ItemsTableSize    int               `json:"items_table_size"`
	ItemsByPacket     map[string]uint64 `json:"items_by_packet,omitempty"`
	ItemsSamples      []string          `json:"items_visible_samples,omitempty"`
	ItemsLostFamilies []string          `json:"items_lost_families,omitempty"`
	QuitReason        string            `json:"quit_reason"`
	Assertions        []assertion       `json:"assertions"`
	DisconnectReason  string            `json:"disconnect_reason"`
	Error             string            `json:"error,omitempty"`
	OK                bool              `json:"ok"`
}

func buildReport(s *session, cmdCount, cmdFailures int, quitReason string) report {
	snap := s.snapshot()
	num := func(key string) uint64 {
		switch v := snap[key].(type) {
		case uint64:
			return v
		case int64:
			return uint64(v)
		case uint32:
			return uint64(v)
		case int:
			return uint64(v)
		case float64:
			return uint64(v)
		}
		return 0
	}
	r := report{
		Tool:             "clientclone",
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
		Proto:            s.proto,
		Addr:             s.addr,
		Direct:           s.direct,
		Name:             s.name,
		Spawned:          s.spawned,
		SpawnMS:          s.spawnMS,
		AliveMS:          int64(num("alive_ms")),
		GameDataItems:    len(s.gd.Items),
		Packets:          make(map[string]uint64),
		PacketsTotal:     num("packets_total"),
		UnknownPackets:   num("unknown_packets"),
		Sent:             make(map[string]uint64),
		SentTotal:        num("sent_total"),
		SendErrors:       num("send_errors"),
		DecodeErrors:     num("decode_errors"),
		BadPackets:       num("bad_packets"),
		LastBadPacketID:  uint32(num("last_bad_packet_id")),
		LastBadLen:       int(num("last_bad_len")),
		LatencyEcho:      num("latency_echo"),
		Forms:            num("forms"),
		ChunkRequests:    num("chunk_requests"),
		AuthInput:        num("auth_input"),
		Commands:         cmdCount,
		CommandFailures:  cmdFailures,
		QuitReason:       quitReason,
		DisconnectReason: s.disconnectReason,
		Error:            s.errorText,
	}
	if v, ok := snap["last_bad_error"].(string); ok {
		r.LastBadError = v
	}
	s.mu.Lock()
	for k, v := range s.packets {
		r.Packets[k] = v
	}
	for k, v := range s.sent {
		r.Sent[k] = v
	}
	s.mu.Unlock()

	// Live wire item census: these are IDs a real headless client actually
	// decoded, resolved against its own protocol table.
	items := s.itemSummary()
	r.ItemsDecoded, _ = items["items_decoded"].(uint64)
	r.ItemsVisible, _ = items["items_visible"].(uint64)
	r.ItemsLost, _ = items["items_lost"].(uint64)
	r.ItemsUnknown, _ = items["items_unknown"].(uint64)
	r.ItemsEmptySlots, _ = items["items_empty_slots"].(uint64)
	r.ItemsTableSize, _ = items["items_table_size"].(int)
	if v, ok := items["items_by_packet"].(map[string]uint64); ok {
		r.ItemsByPacket = v
	}
	if v, ok := items["items_visible_samples"].([]string); ok {
		r.ItemsSamples = v
	}
	if v, ok := items["items_lost_families"].([]string); ok {
		r.ItemsLostFamilies = v
	}

	add := func(name string, ok bool, detail string) {
		r.Assertions = append(r.Assertions, assertion{Name: name, OK: ok, Detail: detail})
	}

	if !s.joined {
		add("session_started", false, "no join command succeeded; nothing was tested")
		r.OK = false
		return r
	}

	add("spawn", s.spawned, fmt.Sprintf("DoSpawn completed in %d ms", s.spawnMS))

	want, wantErr := blocktranslate.Items(s.proto)
	switch {
	case wantErr != nil:
		add("item_registry", false, fmt.Sprintf("blocktranslate.Items(%d): %v", s.proto, wantErr))
	case len(s.gd.Items) == 0:
		add("item_registry", false, "GameData().Items is empty")
	default:
		match := sameItemIDs(s.gd.Items, want)
		r.ItemRegistryMatch = match
		add("item_registry", match,
			fmt.Sprintf("%d GameData entries vs %d pinned %d entries", len(s.gd.Items), len(want), s.proto))
	}

	chunks := num("packet_LevelChunk")
	add("level_chunk", chunks > 0, fmt.Sprintf("LevelChunk=%d (must be > 0)", chunks))
	add("decode_errors", num("decode_errors") == 0,
		fmt.Sprintf("gophertunnel read/decode errors=%d", num("decode_errors")))
	add("send_errors", num("send_errors") == 0,
		fmt.Sprintf("WritePacket errors=%d", num("send_errors")))
	if !s.cfg.noInput {
		add("auth_input", num("auth_input") > 0,
			fmt.Sprintf("PlayerAuthInput sent=%d (must be > 0)", num("auth_input")))
	}
	if s.cfg.duration > 0 {
		early := quitReason == "connection died" || strings.Contains(s.disconnectReason, "host_disconnect") ||
			strings.Contains(s.disconnectReason, "read_error")
		add("no_early_disconnect", !early,
			fmt.Sprintf("quit=%s reason=%s alive_ms=%d duration_ms=%d", quitReason, s.disconnectReason, r.AliveMS, s.cfg.duration.Milliseconds()))
	}
	if num("chunk_requests") > 0 {
		replies := num("packet_SubChunk")
		add("subchunk_replies", replies > 0,
			fmt.Sprintf("sent %d SubChunkRequest, received %d SubChunk", num("chunk_requests"), replies))
	}
	add("command_failures", cmdFailures == 0, fmt.Sprintf("%d of %d commands failed", cmdFailures, cmdCount))
	add("items_decoded", r.ItemsDecoded > 0,
		fmt.Sprintf("%d item stacks decoded from the live wire", r.ItemsDecoded))
	add("items_visible", r.ItemsVisible > 0,
		fmt.Sprintf("%d decoded stacks resolved to a named item in the %d-entry table", r.ItemsVisible, r.ItemsTableSize))
	add("items_no_air_loss", r.ItemsLost == 0,
		fmt.Sprintf("%d creative/recipe entries arrived as AIR", r.ItemsLost))
	add("items_no_unknown_id", r.ItemsUnknown == 0,
		fmt.Sprintf("%d stacks carried an ID absent from the client's item table", r.ItemsUnknown))

	r.OK = r.Error == ""
	for _, a := range r.Assertions {
		if !a.OK {
			r.OK = false
		}
	}
	return r
}

func printReport(r report) {
	fmt.Printf("clientclone: proto=%d addr=%s direct=%t spawned=%t spawn_ms=%d alive_ms=%d\n",
		r.Proto, r.Addr, r.Direct, r.Spawned, r.SpawnMS, r.AliveMS)
	fmt.Printf("  packets=%d (LevelChunk=%d SubChunk=%d) items=%d registry_match=%t\n",
		r.PacketsTotal, r.Packets["LevelChunk"], r.Packets["SubChunk"], r.GameDataItems, r.ItemRegistryMatch)
	fmt.Printf("  sent=%d (PlayerAuthInput=%d SubChunkRequest=%d) echo=%d forms=%d errors: send=%d decode=%d\n",
		r.SentTotal, r.Sent["PlayerAuthInput"], r.Sent["SubChunkRequest"], r.LatencyEcho, r.Forms, r.SendErrors, r.DecodeErrors)
	fmt.Printf("  items: decoded=%d visible=%d lost=%d unknown=%d empty_slots=%d table=%d by_packet=%v\n",
		r.ItemsDecoded, r.ItemsVisible, r.ItemsLost, r.ItemsUnknown, r.ItemsEmptySlots, r.ItemsTableSize, r.ItemsByPacket)
	if len(r.ItemsSamples) > 0 {
		fmt.Printf("  items_visible_samples=%v\n", r.ItemsSamples)
	}
	if len(r.ItemsLostFamilies) > 0 {
		fmt.Printf("  items_lost_families=%v\n", r.ItemsLostFamilies)
	}
	if r.BadPackets > 0 {
		fmt.Printf("  DECODE: bad=%d last_id=%d payload_len=%d err=%q\n",
			r.BadPackets, r.LastBadPacketID, r.LastBadLen, r.LastBadError)
	}
	for _, a := range r.Assertions {
		status := "OK  "
		if !a.OK {
			status = "FAIL"
		}
		fmt.Printf("  [%s] %-22s %s\n", status, a.Name, a.Detail)
	}
	fmt.Printf("  quit=%s disconnect=%s ok=%t\n", r.QuitReason, r.DisconnectReason, r.OK)
}

func run() int {
	var (
		addr         = flag.String("addr", "", "RakNet endpoint: relay listener, or BDS address when -direct is set")
		direct       = flag.Bool("direct", false, "treat -addr as a direct BDS (transport=raknet), not a relay listener")
		proto        = flag.Int("proto", 2193, "client protocol (1001|2168|2169|2193)")
		name         = flag.String("name", "ClientClone", "display name (<= 16 chars, no leading digit)")
		script       = flag.String("script", "", "file containing one JSON command per line (default: stdin)")
		jsonPath     = flag.String("json", "", "write the final JSON report to this path")
		duration     = flag.Duration("duration", 0, "auto quit after this long (0 = wait for quit/EOF)")
		spawnTimeout = flag.Duration("spawn-timeout", 20*time.Second, "timeout for dial + DoSpawn")
		auto         = flag.Bool("auto", false, "join immediately at startup using -addr/-direct")
		verbose      = flag.Bool("v", false, "verbose command/packet logging to stderr")
		noInput      = flag.Bool("no-input", false, "disable the 20 Hz PlayerAuthInput loop (bisect)")
		noSubchunk   = flag.Bool("no-subchunk", false, "disable SubChunkRequest (bisect)")
		inputFlags   = flag.Bool("input-flags", false, "set jump/sneak/sprint flags in PlayerAuthInput (bisect)")
		noFail       = flag.Bool("nofail", false, "always exit 0 even when assertions fail (bisect)")
		itemLog      = flag.Int("item-log", 24, "max ITEM_VISIBLE/ITEM_LOST/ITEM_UNKNOWN_ID log lines (0 = off, <0 = all)")
	)
	flag.Parse()

	cfg := config{
		addr:         *addr,
		direct:       *direct,
		proto:        int32(*proto),
		name:         *name,
		script:       *script,
		jsonPath:     *jsonPath,
		duration:     *duration,
		spawnTimeout: *spawnTimeout,
		auto:         *auto,
		verbose:      *verbose,
		noInput:      *noInput,
		noSubchunk:   *noSubchunk,
		inputFlags:   *inputFlags,
		noFail:       *noFail,
		itemLog:      *itemLog,
	}
	if cfg.spawnTimeout <= 0 || cfg.duration < 0 {
		fmt.Fprintln(os.Stderr, "clientclone: -spawn-timeout must be > 0 and -duration >= 0")
		return 1
	}
	if _, err := codecFor(cfg.proto); err != nil {
		fmt.Fprintln(os.Stderr, "clientclone:", err)
		return 1
	}
	if cfg.auto && cfg.addr == "" {
		fmt.Fprintln(os.Stderr, "clientclone: -auto needs -addr")
		return 1
	}

	s := newSession(cfg)
	emit := func(res result) {
		b, _ := json.Marshal(res)
		fmt.Println(string(b))
		if cfg.verbose {
			fmt.Fprintf(os.Stderr, "[clientclone] %s ok=%t %s%s\n", res.Cmd, res.OK, res.Detail, res.Error)
		}
	}

	cmdCount, cmdFailures := 0, 0
	tally := func(res result) {
		cmdCount++
		if !res.OK {
			cmdFailures++
		}
	}

	if cfg.auto {
		res := s.exec(command{Cmd: "join"})
		emit(res)
		tally(res)
	}

	var in io.Reader = os.Stdin
	if cfg.script != "" {
		f, err := os.Open(cfg.script)
		if err != nil {
			fmt.Fprintln(os.Stderr, "clientclone: open -script:", err)
			s.stop()
			return 1
		}
		defer f.Close()
		in = f
	}

	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "clientclone: read commands:", err)
		}
	}()

	var deadline <-chan time.Time
	if cfg.duration > 0 {
		t := time.NewTimer(cfg.duration)
		defer t.Stop()
		deadline = t.C
	}

	quitReason := "eof"
loop:
	for {
		select {
		case line, open := <-lines:
			if !open {
				break loop
			}
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
				continue
			}
			var c command
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				res := errResult("?", fmt.Errorf("bad command line %q: %w", line, err))
				emit(res)
				tally(res)
				continue
			}
			res := s.exec(c)
			emit(res)
			tally(res)
			if res.Quit {
				quitReason = "quit command"
				break loop
			}
		case <-deadline:
			quitReason = "duration elapsed"
			break loop
		case <-s.dead:
			quitReason = "connection died"
			break loop
		case <-s.ctx.Done():
			quitReason = "stopped"
			break loop
		}
	}

	s.stop()
	r := buildReport(s, cmdCount, cmdFailures, quitReason)
	if cfg.jsonPath != "" {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "clientclone: marshal report:", err)
			return 1
		}
		if err := os.WriteFile(cfg.jsonPath, append(data, '\n'), 0644); err != nil {
			fmt.Fprintln(os.Stderr, "clientclone: write report:", err)
			return 1
		}
		fmt.Printf("clientclone: report written to %s\n", cfg.jsonPath)
	}
	printReport(r)
	if !r.OK && !cfg.noFail {
		return 1
	}
	return 0
}

func main() { os.Exit(run()) }
