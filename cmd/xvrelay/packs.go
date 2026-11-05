package main

// CONN-005: resource-pack prefetch with a real cost guard.
//
// Why this file exists (measured, not guessed):
//   - gophertunnel's dial-side Conn calls ConvertToLatest(pk, conn) for EVERY decoded
//     packet in packet.go:74, BEFORE handlePacket(). Wrapping the host Protocol is
//     therefore the earliest seam where ResourcePacksInfo / ResourcePackDataInfo are
//     already fully decoded into the latest protocol, with the host's own wire
//     differences already resolved by the cross-version codec.
//   - The DownloadResourcePack callback (dial.go) has no size argument, so a decision
//     based on size needs this decoded view of ResourcePacksInfo first.
//   - ResourcePackDataInfo arrives BEFORE the first ResourcePackChunkRequest
//     (conn.go:1313 starts the chunk-request goroutine only after
//     handleResourcePackDataInfo returns). It carries the REAL compressed size.
//     That is the last point where an over-budget pack can still be stopped without
//     consuming a single byte of it.
//   - The hang that got prefetch disabled: conn.go:1334 resource.Read() failing on a
//     weird manifest version returns early WITHOUT decrementing packAmount, so
//     expect(IDResourcePackStack) never completes and DialContext waits until its
//     deadline. Cancelling the dial context on a detected overrun aborts that wait
//     immediately; DialContext's select has `case <-ctx.Done()` (dial.go:339/352).
//
// Deliberately NOT done: no fork of gophertunnel, no raw wire decoding, no attempt
// to stop a download mid-stream. Those are the boundaries documented in the README.

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// ── Policy ───────────────────────────────────────────────────────────────────────

type packMode int

const (
	// packModeSkip reproduces the behaviour before CONN-005: refuse every pack
	// (returning false puts it in ignoredResourcePacks, so the spawn still completes).
	packModeSkip packMode = iota
	// packModeDownload downloads every advertised pack, subject to the budget.
	packModeDownload
	// packModeLimit downloads only packs whose DECLARED size is <= -pack-max-mb.
	packModeLimit
)

func (m packMode) String() string {
	switch m {
	case packModeSkip:
		return "skip"
	case packModeDownload:
		return "download"
	case packModeLimit:
		return "limit"
	}
	return fmt.Sprintf("packMode(%d)", int(m))
}

func parsePackMode(s string) (packMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "skip":
		return packModeSkip, nil
	case "download":
		return packModeDownload, nil
	case "limit":
		return packModeLimit, nil
	}
	return packModeSkip, fmt.Errorf("invalid -pack-mode %q (want skip|download|limit)", s)
}

type packOverrun int

const (
	// packOverrunAbort cancels the host dial as soon as the REAL size breaks a limit.
	packOverrunAbort packOverrun = iota
	// packOverrunReport only logs the measurement and keeps downloading (old behaviour).
	packOverrunReport
)

func (o packOverrun) String() string {
	switch o {
	case packOverrunAbort:
		return "abort"
	case packOverrunReport:
		return "report"
	}
	return fmt.Sprintf("packOverrun(%d)", int(o))
}

func parsePackOverrun(s string) (packOverrun, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "abort":
		return packOverrunAbort, nil
	case "report":
		return packOverrunReport, nil
	}
	return packOverrunAbort, fmt.Errorf("invalid -pack-overrun %q (want abort|report)", s)
}

type packPolicy struct {
	Mode     packMode
	MaxMB    float64 // per-pack declared-size ceiling in limit mode (0 = no ceiling)
	BudgetMB float64 // process-wide total download ceiling (0 = unlimited)
	Overrun  packOverrun
	Prefetch bool // run the extra host dial before opening the listener
}

func defaultPackPolicy() packPolicy {
	return packPolicy{
		Mode:     packModeSkip,
		MaxMB:    8,
		BudgetMB: 32,
		Overrun:  packOverrunAbort,
	}
}

// prefetchEnabled keeps the old `if false` honest: a prefetch dial can only produce
// packs if some pack is actually downloaded, so -pack-prefetch with skip is a no-op.
func prefetchEnabled(p packPolicy) bool {
	return p.Prefetch && p.Mode != packModeSkip
}

// ── Guard state ──────────────────────────────────────────────────────────────────

type packAdvert struct {
	declared uint64
	version  string
}

type packStats struct {
	Advertised  int
	Downloaded  int
	Skipped     int
	UnknownSize int
	Lies        int
	Overruns    int
	BudgetHits  int
	AdvertBytes uint64
	RealBytes   uint64
	UsedBytes   uint64
}

// packGuard is PER DIAL. Multiple clients can be spawning at the same time, so a
// package-level singleton would mix their measurements. Each dial owns one guard,
// reachable through spawnCapture.packs.
type packGuard struct {
	mu       sync.Mutex
	policy   packPolicy
	budget   uint64
	maxBytes uint64
	adverts  map[string]packAdvert
	// approved holds only the packs decide() actually allowed gophertunnel to
	// request. A DataInfo for any other pack (skipped, or unsolicited) must not
	// affect the cost guard — those packs consume no bytes.
	approved map[string]bool
	// used is the accounted download cost, in DECLARED bytes. It is corrected by
	// (real - declared) when ResourcePackDataInfo exposes a lying host.
	used  uint64
	stats packStats

	abortFn     func(reason string)
	aborted     bool
	abortReason string
}

func newPackGuard(p packPolicy) *packGuard {
	g := &packGuard{policy: p, adverts: make(map[string]packAdvert), approved: make(map[string]bool)}
	if p.BudgetMB > 0 {
		g.budget = uint64(p.BudgetMB * 1024 * 1024)
	}
	if p.MaxMB > 0 {
		g.maxBytes = uint64(p.MaxMB * 1024 * 1024)
	}
	return g
}

// setAbort installs the dial's cancel hook. It must be called BEFORE DialContext,
// because ResourcePacksInfo can arrive during the handshake.
func (g *packGuard) setAbort(fn func(reason string)) {
	g.mu.Lock()
	g.abortFn = fn
	g.mu.Unlock()
}

func (g *packGuard) abortStatus() (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.aborted, g.abortReason
}

// requestAbort is idempotent: several packs can overrun at once, but the dial is
// cancelled once and the first (most useful) reason is kept.
func (g *packGuard) requestAbort(reason string) {
	g.mu.Lock()
	if g.aborted {
		g.mu.Unlock()
		return
	}
	g.aborted = true
	g.abortReason = reason
	fn := g.abortFn
	g.mu.Unlock()
	if fn != nil {
		fn(reason)
	}
}

// ── Observations ─────────────────────────────────────────────────────────────────

// observePacksInfo records what the host CLAIMS. It runs before DownloadResourcePack
// is consulted (ConvertToLatest precedes handlePacket), so decide() always sees a
// populated table.
func (g *packGuard) observePacksInfo(pk *packet.ResourcePacksInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.adverts = make(map[string]packAdvert, len(pk.TexturePacks))
	g.approved = make(map[string]bool, len(pk.TexturePacks))
	var total uint64
	for _, p := range pk.TexturePacks {
		g.adverts[p.UUID.String()] = packAdvert{declared: p.Size, version: p.Version}
		total += p.Size
	}
	g.stats.Advertised = len(pk.TexturePacks)
	g.stats.AdvertBytes = total
}

// observeDataInfo is the only place where the REAL size is knowable before the first
// chunk is requested. A pack that is not in adverts was skipped (or is cache-only),
// so no DataInfo is expected for it and it is ignored.
func (g *packGuard) observeDataInfo(pk *packet.ResourcePackDataInfo) {
	// The wire UUID is "<uuid>_<version>" (conn.go:1284 splits on "_").
	key := strings.Split(pk.UUID, "_")[0]
	g.mu.Lock()
	if !g.approved[key] {
		// We never asked for this pack, so no bytes of it can be consumed.
		g.mu.Unlock()
		return
	}
	adv, known := g.adverts[key]
	real := pk.Size
	lied := known && real != adv.declared
	switch {
	case !known:
		// No claim was ever recorded (ResourcePacksInfo did not decode). The real
		// size is the only number available, so account it here.
		g.used += real
	case lied:
		g.stats.Lies++
		// Correct the accounted cost: at decide() time only the claim existed.
		used := int64(g.used) + (int64(real) - int64(adv.declared))
		if used < 0 {
			used = 0
		}
		g.used = uint64(used)
	}
	g.stats.RealBytes += real
	var reason string
	switch {
	case g.budget > 0 && g.used > g.budget:
		reason = fmt.Sprintf("pack %s: real accounted total %s > -pack-budget-mb %s",
			key, humanBytes(g.used), humanBytes(g.budget))
	case g.policy.Mode == packModeLimit && g.maxBytes > 0 && real > g.maxBytes:
		reason = fmt.Sprintf("pack %s: real size %s > -pack-max-mb %s",
			key, humanBytes(real), humanBytes(g.maxBytes))
	}
	if reason != "" {
		g.stats.Overruns++
	}
	g.stats.UsedBytes = g.used
	overrun := g.policy.Overrun
	declared := adv.declared
	g.mu.Unlock()

	if lied {
		slog.Warn("xvrelay: host misreported a resource pack size",
			"pack", key, "declared", humanBytes(declared), "real", humanBytes(real))
	}
	if reason == "" {
		return
	}
	if overrun == packOverrunAbort {
		slog.Warn("xvrelay: resource pack overrun — aborting the host dial", "reason", reason)
		g.requestAbort(reason)
		return
	}
	slog.Warn("xvrelay: resource pack overrun — -pack-overrun report, continuing",
		"reason", reason)
}

// decide is the Dialer.DownloadResourcePack callback. false = skip the pack (still
// valid at the protocol level: gophertunnel reports AllPacksDownloaded for it).
func (g *packGuard) decide(id uuid.UUID, version string, cur, total int) bool {
	key := id.String()
	g.mu.Lock()
	adv, known := g.adverts[key]
	p := g.policy

	download := false
	reason := ""
	switch p.Mode {
	case packModeSkip:
		reason = "pack-mode=skip"
	case packModeDownload:
		download, reason = true, "pack-mode=download"
	case packModeLimit:
		switch {
		case !known:
			reason = "no advertised size — refusing instead of guessing"
		case g.maxBytes > 0 && adv.declared > g.maxBytes:
			reason = fmt.Sprintf("declared %s > -pack-max-mb %s", humanBytes(adv.declared), humanBytes(g.maxBytes))
		default:
			download, reason = true, "within -pack-max-mb"
		}
	}
	// The budget is a hard cost ceiling for ALL modes: it is applied last so the
	// final reason always states the binding constraint.
	if download && g.budget > 0 {
		switch {
		case !known:
			download, reason = false, "budget: unknown declared size"
			g.stats.BudgetHits++
		case g.used+adv.declared > g.budget:
			download, reason = false, fmt.Sprintf("budget exhausted: %s / %s", humanBytes(g.used), humanBytes(g.budget))
			g.stats.BudgetHits++
		}
	}
	// Accounting is unconditional: it must also be correct when -pack-budget-mb=0,
	// otherwise a later ResourcePackDataInfo lie could not be reconciled.
	if download {
		g.approved[key] = true
		if known {
			g.used += adv.declared
		}
	}
	if download {
		g.stats.Downloaded++
	} else {
		g.stats.Skipped++
		if !known {
			g.stats.UnknownSize++
		}
	}
	g.stats.UsedBytes = g.used
	g.mu.Unlock()

	slog.Info("xvrelay: resource pack decision", "pack", key, "version", version,
		"index", fmt.Sprintf("%d/%d", cur+1, total), "download", download,
		"declared", func() string {
			if known {
				return humanBytes(adv.declared)
			}
			return "?"
		}(), "reason", reason)
	return download
}

func (g *packGuard) snapshot() packStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.stats
	s.UsedBytes = g.used
	return s
}

func (g *packGuard) summary() string {
	s := g.snapshot()
	line := fmt.Sprintf("advertised=%d (%s) downloaded=%d skipped=%d", s.Advertised,
		humanBytes(s.AdvertBytes), s.Downloaded, s.Skipped)
	if s.UnknownSize > 0 {
		line += fmt.Sprintf(" unknown_size=%d", s.UnknownSize)
	}
	if s.BudgetHits > 0 {
		line += fmt.Sprintf(" budget_hits=%d", s.BudgetHits)
	}
	if s.Lies > 0 {
		line += fmt.Sprintf(" size_lies=%d real=%s", s.Lies, humanBytes(s.RealBytes))
	}
	if s.Overruns > 0 {
		line += fmt.Sprintf(" overruns=%d policy=%s", s.Overruns, g.policy.Overrun)
	}
	line += fmt.Sprintf(" accounted=%s", humanBytes(s.UsedBytes))
	return line
}

// ── Protocol wrapper ─────────────────────────────────────────────────────────────

// packSniffProtocol observes already-decoded, already-converted packets. All other
// Protocol methods are promoted from the embedded host codec, so the dial behaves
// exactly as before.
type packSniffProtocol struct {
	minecraft.Protocol
	guard *packGuard
}

func (p packSniffProtocol) ConvertToLatest(pk packet.Packet, conn *minecraft.Conn) []packet.Packet {
	out := p.Protocol.ConvertToLatest(pk, conn)
	for _, q := range out {
		switch v := q.(type) {
		case *packet.ResourcePacksInfo:
			p.guard.observePacksInfo(v)
		case *packet.ResourcePackDataInfo:
			p.guard.observeDataInfo(v)
		}
	}
	return out
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGT"[exp])
}
