//go:build windows

package lansignal

import (
	"context"
	"net"
	"syscall"
)

// listenShared binds addr with SO_REUSEADDR so this process can receive
// datagrams addressed to a port that another process already owns.
//
// On Windows a second bind of the same UDP port fails unless both sockets set
// SO_REUSEADDR. Broadcast traffic is then delivered to every socket bound to
// that port, which is exactly what is needed to observe a LAN announcement that
// the host app broadcasts to 255.255.255.255:7551 while it keeps 7551 for itself.
func listenShared(addr string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var setErr error
		if err := c.Control(func(fd uintptr) {
			setErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		}); err != nil {
			return err
		}
		return setErr
	}}
	return lc.ListenPacket(context.Background(), "udp4", addr)
}
