package minecraft

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestDeferredPacketWakesBlockedReader(t *testing.T) {
	for _, mode := range []string{"packet", "bytes", "read"} {
		t.Run(mode, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			c := newConn(a, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultProtocol, 0, true)
			defer c.cancelFunc(context.Canceled)
			done := make(chan error, 1)
			go func() {
				var err error
				switch mode {
				case "packet":
					_, err = c.ReadPacket()
				case "bytes":
					_, err = c.ReadBytes()
				case "read":
					_, err = c.Read(make([]byte, 16))
				}
				done <- err
			}()
			// Ensure the reader has reached its empty-queue wait first.
			select {
			case err := <-done:
				t.Fatalf("read returned before input: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			c.deferPacket(&packetData{h: &packet.Header{PacketID: 9999}, payload: bytes.NewBuffer([]byte{42}), full: []byte{42}})
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(300 * time.Millisecond):
				c.cancelFunc(context.Canceled)
				<-done
				t.Fatal("deferred packet did not wake reader before spawn")
			}
		})
	}
}
