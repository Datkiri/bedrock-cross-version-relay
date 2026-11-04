package main

import (
	"bedrock-cross-version-relay/internal/lansignal"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Protected by hostProtoMu. Established sessions retain their own hostProto.
var hostProfileWorldID uint64

// hostProtoHints remembers the protocol that ALREADY dialled successfully, per host ADDRESS.
//
// ★ Why this exists: `resetHostProfile` resets to 2193/unknown whenever it sees a new world, so
// every new world tries 2193 first again. Against a "lenient" host (the host app) the 2193 dial
// still gets StartGame and is only rejected afterwards because the version differs — measured at
// 5.3 s wasted (log 13:37:12.118 → 13:37:17.418), pushing the total dial time to 13.9 s until the
// client's ~15 s join timeout expired and it disconnected.
// This is only a HINT for the order to try; `verifyHostProtocol` still checks the host's
// GameVersion and jumps to the right protocol as before.
var (
	hostProtoHintMu sync.Mutex
	hostProtoHints  = map[string]int32{}
	// hostProtoHint: the hint for the current dial (0 = none).
	hostProtoHint atomic.Int32
)

func rememberHostProto(addr string, proto int32) {
	if addr == "" || proto == 0 {
		return
	}
	hostProtoHintMu.Lock()
	hostProtoHints[addr] = proto
	hostProtoHintMu.Unlock()
}

func hostProtoHintFor(addr string) int32 {
	hostProtoHintMu.Lock()
	defer hostProtoHintMu.Unlock()
	return hostProtoHints[addr]
}

func resetHostProfile(w lansignal.World) {
	hostProtoID.Store(2193)
	hostProtoKnown.Store(false)
	hostProtoHint.Store(0)
	hostGameVersion = ""
	if w.Host == nil {
		return
	}
	pid, known := resolveHostProto("auto", w.Host.Protocol)
	hostGameVersion = w.Host.GameVersion
	if !known && w.Addr != nil {
		if cp, version, ok := loadProtoCache(w.NetworkID, w.Addr.String()); ok {
			if _, supported := resolveProto(cp); supported {
				pid, known = cp, true
				hostGameVersion = version
			}
		}
	}
	// New world on the same host machine: reuse the previous protocol as a hint (not certain).
	if !known && w.Addr != nil {
		if h := hostProtoHintFor(w.Addr.String()); h != 0 {
			if _, supported := resolveProto(h); supported {
				pid = h
				hostProtoHint.Store(h)
			}
		}
	}
	hostProtoID.Store(pid)
	hostProtoKnown.Store(known)
	slog.Info("xvrelay: refreshed host protocol for current world",
		"network_id", w.NetworkID, "protocol", pid, "known", known,
		"hint", hostProtoHint.Load())
}
