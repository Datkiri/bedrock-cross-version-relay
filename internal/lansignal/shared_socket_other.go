//go:build !windows

package lansignal

import (
	"context"
	"net"
)

// listenShared binds addr like listenShared on Windows. Outside Windows the
// runtime already sets SO_REUSEADDR, so a plain bind behaves the same way.
func listenShared(addr string) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(context.Background(), "udp4", addr)
}
