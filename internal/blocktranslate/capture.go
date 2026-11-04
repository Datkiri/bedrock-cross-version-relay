package blocktranslate

import (
	"bytes"
	"log/slog"
	"os"
)

var capturedSubChunk []byte
var capturedWire bool

// captureFirstSubChunk records one actual input/output pair, including successful
// rewrites. Parser success alone does not prove the client accepts a payload.
func captureFirstSubChunk(raw, translated []byte) {
	if DumpSubchunkPath == "" {
		return
	}
	translateDumpMu.Lock()
	defer translateDumpMu.Unlock()
	if translateDumped {
		return
	}
	translateDumped = true
	if err := os.WriteFile(DumpSubchunkPath, raw, 0600); err != nil {
		slog.Warn("xvrelay: terrain capture failed", "error", err)
		return
	}
	if err := os.WriteFile(DumpSubchunkPath+".translated.bin", translated, 0600); err != nil {
		slog.Warn("xvrelay: terrain capture failed", "error", err)
		return
	}
	capturedSubChunk = append([]byte(nil), translated...)
	slog.Info("xvrelay: terrain input/output captured", "path", DumpSubchunkPath, "inputBytes", len(raw), "outputBytes", len(translated))
}

// CaptureSubChunkWire is called by the outgoing packet callback after the final
// codec conversion. Only capture the packet containing the recorded output pair.
func CaptureSubChunkWire(payload []byte) {
	if DumpSubchunkPath == "" {
		return
	}
	translateDumpMu.Lock()
	defer translateDumpMu.Unlock()
	if capturedWire || len(capturedSubChunk) == 0 || !bytes.Contains(payload, capturedSubChunk) {
		return
	}
	capturedWire = true
	if err := os.WriteFile(DumpSubchunkPath+".wire.bin", payload, 0600); err != nil {
		slog.Warn("xvrelay: terrain wire capture failed", "error", err)
		return
	}
	slog.Info("xvrelay: terrain wire captured", "path", DumpSubchunkPath+".wire.bin", "bytes", len(payload))
}
