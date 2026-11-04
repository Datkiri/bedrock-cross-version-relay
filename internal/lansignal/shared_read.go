package lansignal

import (
	"fmt"
	"net"

	"github.com/df-mc/go-nethernet/discovery"
)

// readSharedLoop listens on the discovery port in addition to the socket the
// Signaling actually sends from.
//
// A world announced by a host application broadcasts a ResponsePacket from its
// own socket to 255.255.255.255:7551, and it never answers a unicast request.
// A joiner that binds an ephemeral port (which it must, because the host already
// owns 7551) therefore hears nothing at all: the broadcast is addressed to a
// port it does not hold.
//
// Binding 7551 with SO_REUSEADDR closes that gap. Broadcast datagrams are
// delivered to every socket bound to the port, so the announcement is recorded
// here while replies to our own requests keep arriving on the ephemeral socket
// handled by readLoop. Only discovery packets are processed: NetherNet signals
// are always unicast to the sender's own port.
func (s *Signaling) readSharedLoop() {
	if udp, ok := s.conn.LocalAddr().(*net.UDPAddr); ok && udp.Port == DefaultPort {
		return // readLoop already owns the discovery port.
	}
	conn, err := listenShared(fmt.Sprintf("0.0.0.0:%d", DefaultPort))
	if err != nil {
		s.log.Debug("lansignal: shared discovery port unavailable", "error", err)
		return
	}
	go func() {
		<-s.closed
		_ = conn.Close()
	}()
	s.log.Debug("lansignal: listening for announcements on the shared discovery port",
		"addr", conn.LocalAddr(), "sending_from", s.conn.LocalAddr())

	buf := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			s.log.Error("lansignal: shared read", "error", err)
			return
		}
		pk, senderID, err := discovery.Unmarshal(buf[:n])
		if err != nil || senderID == s.networkID {
			continue
		}
		// Log at Info only the FIRST time a world is seen; announcements repeat every few seconds,
		// so logging every packet would bury the log before anyone reads the relay session.
		s.peersMu.RLock()
		_, existed := s.peers[senderID]
		s.peersMu.RUnlock()
		switch pk := pk.(type) {
		case *discovery.RequestPacket:
			s.record(senderID, addr, func(w *World) { w.Announced = true })
		case *discovery.ResponsePacket:
			raw := append([]byte(nil), pk.ApplicationData...)
			host, hostErr := DecodeHostInfo(raw)
			if hostErr != nil {
				s.log.Warn("lansignal: undecodable broadcast ServerData",
					"network_id", senderID, "error", hostErr, "from", addr)
			}
			sd := &discovery.ServerData{}
			if err := sd.UnmarshalBinary(raw); err != nil {
				s.record(senderID, addr, func(w *World) {
					w.Answered = true
					w.Host = host
					w.RawServerData = raw
				})
			} else {
				s.record(senderID, addr, func(w *World) {
					w.Answered = true
					w.ServerData = sd
					w.Host = host
					w.RawServerData = raw
				})
			}
			if host != nil {
				if !existed {
					s.log.Info("lansignal: world announced by broadcast",
						"network_id", senderID, "from", addr,
						"level", host.LevelName, "host", host.ServerName,
						"players", host.PlayerCount, "max_players", host.MaxPlayerCount)
				} else {
					s.log.Debug("lansignal: world re-announced",
						"network_id", senderID, "from", addr, "level", host.LevelName)
				}
			}
		}
	}
}
