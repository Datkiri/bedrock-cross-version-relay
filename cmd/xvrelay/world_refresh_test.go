package main

import (
	"net"
	"testing"

	"bedrock-cross-version-relay/internal/lansignal"
)

func TestWorldSelectionRefreshesPerJoin(t *testing.T) {
	makeWorld := func(id uint64) lansignal.World {
		return lansignal.World{NetworkID: id, Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8554}, Host: &lansignal.HostInfo{LevelName: "test"}}
	}
	old, next := makeWorld(11), makeWorld(22)
	first, err := pickWorldSnapshot([]lansignal.World{old}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pickWorldSnapshot([]lansignal.World{next}, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.NetworkID != 11 || second.NetworkID != 22 {
		t.Fatal("auto reused stale startup world")
	}
	if _, err := pickWorldSnapshot([]lansignal.World{next}, "11"); err == nil {
		t.Fatal("manual selector silently switched world")
	}
	if _, err := pickWorldSnapshot([]lansignal.World{old, next}, ""); err == nil {
		t.Fatal("ambiguous auto selection guessed a world")
	}
}

func TestWorldChangeResetsHostProtocol(t *testing.T) {
	oldID, oldKnown, oldVersion := hostProtoID.Load(), hostProtoKnown.Load(), hostGameVersion
	defer func() { hostProtoID.Store(oldID); hostProtoKnown.Store(oldKnown); hostGameVersion = oldVersion }()
	hostProtoID.Store(1001)
	hostProtoKnown.Store(true)
	hostGameVersion = "1.26.33"
	resetHostProfile(lansignal.World{NetworkID: 22, Host: &lansignal.HostInfo{Protocol: 2193, GameVersion: "1.26.51"}})
	if hostProtoID.Load() != 2193 || !hostProtoKnown.Load() || hostGameVersion != "1.26.51" {
		t.Fatal("new world kept previous protocol")
	}
	resetHostProfile(lansignal.World{NetworkID: 33, Host: &lansignal.HostInfo{}})
	if hostProtoKnown.Load() || hostGameVersion != "" {
		t.Fatal("unknown new world inherited old protocol/version")
	}
}

// TestWorldChangeUsesAddressProtocolHint: a new world on the SAME host machine must try the
// protocol that worked before, but it must NOT be treated as verified.
// Measured reason: at log 13:37 the 2193 dial ran first and was only rejected for a version
// mismatch, wasting 5.3 s so the total dial reached 13.9 s > the client's join timeout.
func TestWorldChangeUsesAddressProtocolHint(t *testing.T) {
	oldID, oldKnown, oldVersion, oldHint := hostProtoID.Load(), hostProtoKnown.Load(), hostGameVersion, hostProtoHint.Load()
	defer func() {
		hostProtoID.Store(oldID)
		hostProtoKnown.Store(oldKnown)
		hostGameVersion = oldVersion
		hostProtoHint.Store(oldHint)
	}()
	addr := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 219), Port: 8554}
	rememberHostProto(addr.String(), 1001)

	resetHostProfile(lansignal.World{NetworkID: 44, Addr: addr, Host: &lansignal.HostInfo{}})
	if hostProtoKnown.Load() {
		t.Fatal("a hint must not count as verified")
	}
	if got := hostProtoHint.Load(); got != 1001 {
		t.Fatalf("hint = %d, want 1001", got)
	}
	if got := hostProtoID.Load(); got != 1001 {
		t.Fatalf("first candidate = %d, want 1001", got)
	}
}

// TestSpawnPrefixGateSignalsOnce: the gate opens exactly once, is idempotent, and nil = open.
// This is the precondition for the H->C relay; log 13:41 shows that with the gate shut the
// client finishes spawning, receives nothing further, and disconnects on its own.
func TestSpawnPrefixGateSignalsOnce(t *testing.T) {
	g := newSpawnPrefixGate()
	select {
	case <-g.done():
		t.Fatal("gate opened before spawn")
	default:
	}
	g.signal()
	g.signal()
	select {
	case <-g.done():
	default:
		t.Fatal("gate did not open after signal")
	}
	var nilGate *spawnPrefixGate
	nilGate.signal()
	select {
	case <-nilGate.done():
	default:
		t.Fatal("a nil gate must count as already open")
	}
}
