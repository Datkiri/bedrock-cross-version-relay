package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

const testMB = 1024 * 1024

func testPackInfo(id uuid.UUID, version string, size uint64) protocol.TexturePackInfo {
	return protocol.TexturePackInfo{UUID: id, Version: version, Size: size}
}

// observeViaWrapper drives the exact seam used in production: the host Protocol's
// ConvertToLatest runs BEFORE handlePacket, so the guard must see decoded packets
// there and have its advert table ready when decide() is called.
func observeViaWrapper(g *packGuard, pk packet.Packet) {
	p := packSniffProtocol{Protocol: minecraft.DefaultProtocol, guard: g}
	_ = p.ConvertToLatest(pk, nil)
}

func TestParsePackFlags(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want packMode
	}{
		{"skip", packModeSkip}, {"download", packModeDownload}, {"limit", packModeLimit},
		{" LIMIT ", packModeLimit},
	} {
		got, err := parsePackMode(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parsePackMode(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if _, err := parsePackMode("everything"); err == nil {
		t.Error("parsePackMode accepted an invalid mode")
	}
	for _, tc := range []struct {
		in   string
		want packOverrun
	}{
		{"abort", packOverrunAbort}, {"report", packOverrunReport}, {" REPORT ", packOverrunReport},
	} {
		got, err := parsePackOverrun(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parsePackOverrun(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if _, err := parsePackOverrun("ignore"); err == nil {
		t.Error("parsePackOverrun accepted an invalid policy")
	}
}

func TestPrefetchEnabled(t *testing.T) {
	cases := []struct {
		mode     packMode
		prefetch bool
		want     bool
	}{
		{packModeSkip, false, false},
		{packModeSkip, true, false}, // -pack-prefetch with skip must be a no-op
		{packModeDownload, false, false},
		{packModeDownload, true, true},
		{packModeLimit, true, true},
	}
	for _, c := range cases {
		got := prefetchEnabled(packPolicy{Mode: c.mode, Prefetch: c.prefetch})
		if got != c.want {
			t.Errorf("prefetchEnabled(mode=%v prefetch=%v) = %v, want %v", c.mode, c.prefetch, got, c.want)
		}
	}
}

func TestPackGuardSkipIsDefaultAndNeverDownloads(t *testing.T) {
	g := newPackGuard(defaultPackPolicy())
	id := uuid.New()
	observeViaWrapper(g, &packet.ResourcePacksInfo{
		TexturePacks: []protocol.TexturePackInfo{testPackInfo(id, "1.0.0", 2*testMB)},
	})
	if g.decide(id, "1.0.0", 0, 1) {
		t.Fatal("default skip policy downloaded a pack")
	}
	s := g.snapshot()
	if s.Advertised != 1 || s.Downloaded != 0 || s.Skipped != 1 || s.AdvertBytes != 2*testMB {
		t.Errorf("unexpected stats: %+v", s)
	}
	if aborted, _ := g.abortStatus(); aborted {
		t.Error("skip policy must never abort the dial")
	}
}

func TestPackGuardLimitUsesDeclaredSize(t *testing.T) {
	small, big := uuid.New(), uuid.New()
	g := newPackGuard(packPolicy{Mode: packModeLimit, MaxMB: 4, BudgetMB: 0, Overrun: packOverrunAbort})
	observeViaWrapper(g, &packet.ResourcePacksInfo{TexturePacks: []protocol.TexturePackInfo{
		testPackInfo(small, "1.0.0", 1*testMB),
		testPackInfo(big, "1.0.0", 9*testMB),
	}})
	if !g.decide(small, "1.0.0", 0, 2) {
		t.Error("pack under -pack-max-mb was refused")
	}
	if g.decide(big, "1.0.0", 1, 2) {
		t.Error("pack over -pack-max-mb was downloaded on the DECLARED size")
	}
	s := g.snapshot()
	if s.Downloaded != 1 || s.Skipped != 1 || s.UsedBytes != 1*testMB {
		t.Errorf("unexpected stats: %+v", s)
	}
}

func TestPackGuardBudgetBlocksSecondPack(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	g := newPackGuard(packPolicy{Mode: packModeDownload, BudgetMB: 5, Overrun: packOverrunAbort})
	observeViaWrapper(g, &packet.ResourcePacksInfo{TexturePacks: []protocol.TexturePackInfo{
		testPackInfo(a, "1.0.0", 3*testMB),
		testPackInfo(b, "1.0.0", 3*testMB),
	}})
	if !g.decide(a, "1.0.0", 0, 2) {
		t.Error("first pack should fit the budget")
	}
	if g.decide(b, "1.0.0", 1, 2) {
		t.Error("second pack exceeded the 5 MB budget but was still downloaded")
	}
	s := g.snapshot()
	if s.BudgetHits != 1 || s.Downloaded != 1 || s.Skipped != 1 || s.UsedBytes != 3*testMB {
		t.Errorf("unexpected stats: %+v", s)
	}
}

func TestPackGuardUnknownDeclaredSizeIsRefused(t *testing.T) {
	id := uuid.New()
	g := newPackGuard(packPolicy{Mode: packModeLimit, MaxMB: 4, BudgetMB: 32})
	// No ResourcePacksInfo observed (e.g. the host sent a decode-resistant packet).
	if g.decide(id, "1.0.0", 0, 1) {
		t.Error("a pack with no known size must be refused, not guessed")
	}
	if s := g.snapshot(); s.UnknownSize != 1 {
		t.Errorf("UnknownSize = %d, want 1", s.UnknownSize)
	}
}

func TestPackGuardDetectsRealSizeLieAndAborts(t *testing.T) {
	id := uuid.New()
	aborts := 0
	g := newPackGuard(packPolicy{Mode: packModeDownload, BudgetMB: 5, Overrun: packOverrunAbort})
	g.setAbort(func(string) { aborts++ })

	declared, realSize := uint64(1*testMB), uint64(40*testMB)
	observeViaWrapper(g, &packet.ResourcePacksInfo{
		TexturePacks: []protocol.TexturePackInfo{testPackInfo(id, "1.0.0", declared)},
	})
	if !g.decide(id, "1.0.0", 0, 1) {
		t.Fatal("pack was refused before the real size was known")
	}
	// ResourcePackDataInfo arrives before the first chunk request. UUID is "<uuid>_<version>".
	observeViaWrapper(g, &packet.ResourcePackDataInfo{
		UUID: id.String() + "_1.0.0", Size: realSize, DataChunkSize: 1024 * 128,
	})

	aborted, reason := g.abortStatus()
	if !aborted {
		t.Fatal("lying host did not abort the dial")
	}
	if !strings.Contains(reason, "-pack-budget-mb") {
		t.Errorf("abort reason does not name the binding constraint: %q", reason)
	}
	if aborts != 1 {
		t.Errorf("abort hook called %d times, want 1", aborts)
	}
	s := g.snapshot()
	if s.Lies != 1 || s.Overruns != 1 || s.RealBytes != realSize {
		t.Errorf("unexpected stats: %+v", s)
	}
	// accounted = declared + (real - declared) = real
	if s.UsedBytes != realSize {
		t.Errorf("accounted bytes = %d, want real %d", s.UsedBytes, realSize)
	}
}

func TestPackGuardOverrunReportDoesNotAbort(t *testing.T) {
	id := uuid.New()
	aborts := 0
	g := newPackGuard(packPolicy{Mode: packModeDownload, BudgetMB: 5, Overrun: packOverrunReport})
	g.setAbort(func(string) { aborts++ })
	observeViaWrapper(g, &packet.ResourcePacksInfo{
		TexturePacks: []protocol.TexturePackInfo{testPackInfo(id, "1.0.0", 1*testMB)},
	})
	g.decide(id, "1.0.0", 0, 1)
	observeViaWrapper(g, &packet.ResourcePackDataInfo{
		UUID: id.String() + "_1.0.0", Size: 40 * testMB, DataChunkSize: 1024 * 128,
	})
	if aborted, _ := g.abortStatus(); aborted {
		t.Error("-pack-overrun report must not abort the dial")
	}
	if aborts != 0 {
		t.Errorf("abort hook called %d times under report, want 0", aborts)
	}
	if s := g.snapshot(); s.Overruns != 1 || s.Lies != 1 {
		t.Errorf("unexpected stats: %+v", s)
	}
}

func TestPackGuardAbortIsIdempotent(t *testing.T) {
	calls := 0
	g := newPackGuard(defaultPackPolicy())
	g.setAbort(func(string) { calls++ })
	g.requestAbort("first")
	g.requestAbort("second")
	aborted, reason := g.abortStatus()
	if !aborted || reason != "first" || calls != 1 {
		t.Errorf("aborted=%v reason=%q calls=%d; want true/first/1", aborted, reason, calls)
	}
}

func TestPackGuardIgnoresDataInfoForSkippedPack(t *testing.T) {
	skipped, known := uuid.New(), uuid.New()
	g := newPackGuard(packPolicy{Mode: packModeLimit, MaxMB: 1, BudgetMB: 100, Overrun: packOverrunAbort})
	observeViaWrapper(g, &packet.ResourcePacksInfo{TexturePacks: []protocol.TexturePackInfo{
		testPackInfo(known, "1.0.0", 1),
		testPackInfo(skipped, "1.0.0", 500*testMB),
	}})
	g.decide(known, "1.0.0", 0, 2)
	g.decide(skipped, "1.0.0", 1, 2)
	// A host could still send a DataInfo for a pack we refused. It must not be counted.
	observeViaWrapper(g, &packet.ResourcePackDataInfo{
		UUID: skipped.String() + "_1.0.0", Size: 900 * testMB, DataChunkSize: 1024 * 128,
	})
	if aborted, _ := g.abortStatus(); aborted {
		t.Error("DataInfo for a skipped pack triggered an abort")
	}
	if s := g.snapshot(); s.Lies != 0 || s.Overruns != 0 || s.RealBytes != 0 {
		t.Errorf("skipped pack polluted the stats: %+v", s)
	}
}

func TestPackGuardSummaryIsMeasurable(t *testing.T) {
	g := newPackGuard(packPolicy{Mode: packModeDownload, BudgetMB: 8})
	id := uuid.New()
	observeViaWrapper(g, &packet.ResourcePacksInfo{
		TexturePacks: []protocol.TexturePackInfo{testPackInfo(id, "1.0.0", 2*testMB)},
	})
	g.decide(id, "1.0.0", 0, 1)
	line := g.summary()
	for _, want := range []string{"advertised=1", "downloaded=1", "skipped=0", "accounted="} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q missing %q", line, want)
		}
	}
}
