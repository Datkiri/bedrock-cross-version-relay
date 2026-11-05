// Package lansignal implements nethernet.Signaling over Minecraft's LAN
// discovery protocol (UDP 7551).
//
// It exists because the stock discovery.Listener cannot discover a Minecraft
// 1.26.45 LAN host. Two behaviours of the game force a custom implementation:
//
//  1. The host does not listen on 7551. It broadcasts RequestPackets from its
//     own ephemeral port and expects peers to answer on the port that received
//     them. A client therefore has to bind 7551 and *receive* the host's
//     announcement rather than merely broadcasting its own request.
//
//  2. The stock Listener suppresses its own broadcast whenever it is bound to
//     7551 (`BroadcastAddress` is left nil), so a joiner built on it never sends
//     a single discovery packet and can never learn a network ID from a host
//     that only announces itself.
//
// This type records the address and network ID of every peer it hears from —
// whether via Request, Response or Message packets — and exposes them through
// Worlds, so the joiner can offer hosts that never replied to a broadcast.
package lansignal

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/df-mc/go-nethernet/discovery"
)

// DefaultPort is the UDP port used by Minecraft for LAN discovery.
const DefaultPort = 7551

// broadcastInterval matches the vanilla client's discovery cadence.
const broadcastInterval = 2 * time.Second

// peerTimeout is how long a peer stays listed after its last packet. The vanilla
// client expires inactive peers after 15 seconds, so this mirrors it.
const peerTimeout = 15 * time.Second

// World describes a peer found through LAN discovery.
type World struct {
	// NetworkID is the NetherNet network ID of the peer, taken from the sender ID
	// of its discovery packet.
	NetworkID uint64
	// Addr is the UDP address the peer last sent from.
	Addr net.Addr
	// ServerData is populated when the peer answered with a ResponsePacket. Hosts
	// that only announce themselves by broadcasting leave this nil, because a
	// RequestPacket carries no payload.
	ServerData *discovery.ServerData
	// Host holds a version-tolerant decode of the same payload. It is populated
	// even when ServerData is nil, so a host running a ServerData version the
	// vendored go-nethernet cannot decode still yields its name and nonce.
	Host *HostInfo
	// RawServerData holds the undecoded ApplicationData of the ResponsePacket.
	// It stays populated even when ServerData could not be decoded, so the exact
	// layout a host uses can be inspected.
	RawServerData []byte
	// Announced reports whether the peer was seen broadcasting a RequestPacket.
	Announced bool
	// Answered reports whether the peer replied to one of our broadcasts.
	Answered bool
	// LastSeen is the time of the most recent packet from the peer.
	LastSeen time.Time
}

// Signaling implements nethernet.Signaling for LAN discovery.
type Signaling struct {
	conn      net.PacketConn
	networkID uint64
	log       *slog.Logger

	// broadcasts holds every destination a RequestPacket is sent to.
	broadcasts []*net.UDPAddr

	peersMu sync.RWMutex
	peers   map[uint64]*World

	// pongData is the ServerData we answer other clients' requests with. It is
	// optional: without it we still discover peers, we simply never advertise.
	pongData atomic.Pointer[[]byte]

	notifiersMu sync.RWMutex
	notifiers   map[uint32]nethernet.Notifier
	notifyCount uint32

	// dialTargets holds the network IDs of worlds we have sent an OFFER to.
	// A CONNECTRESPONSE / CANDIDATEADD from one of those peers is an answer to
	// our own outbound dial, not a client connecting to us. See Ingress below:
	// without this split, a single Signaling shared between the host dial and
	// the NetherNet listener makes go-nethernet log
	// "received non-offer signal for a non-existing negotiation" (CONN-002).
	dialTargets sync.Map // uint64 -> struct{}

	closed    chan struct{}
	closeOnce sync.Once
}

// Options configures a Signaling.
type Options struct {
	// Addr is the local address to bind, e.g. "0.0.0.0:7551". Empty means
	// 0.0.0.0:7551.
	Addr string
	// Log receives diagnostic messages. Defaults to slog.Default.
	Log *slog.Logger
	// NoBroadcast disables sending our own RequestPackets. Useful when another
	// process already holds 7551 and we only want to observe its traffic.
	NoBroadcast bool
	// ExtraBroadcasts are additional destinations for RequestPackets.
	ExtraBroadcasts []*net.UDPAddr
}

// networkIDSeed guarantees unique network IDs for Signaling instances created
// in the same process. time.Now().UnixNano() has a coarse resolution on Windows
// (~100 ns or worse), so two Listens a few microseconds apart used to get the
// SAME ID: readLoop then discarded the other peer's packets as "our own
// broadcast" and Signal() could never route a CONNECTRESPONSE back.
var networkIDSeed atomic.Uint64

func nextNetworkID() uint64 {
	id := uint64(time.Now().UnixNano())
	for {
		prev := networkIDSeed.Load()
		if id <= prev {
			id = prev + 1
		}
		if networkIDSeed.CompareAndSwap(prev, id) {
			return id
		}
	}
}

// Listen binds the discovery socket and starts the read and broadcast loops.
func Listen(opts Options) (*Signaling, error) {
	if opts.Addr == "" {
		opts.Addr = fmt.Sprintf("0.0.0.0:%d", DefaultPort)
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	conn, err := net.ListenPacket("udp4", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", opts.Addr, err)
	}

	local, _ := conn.LocalAddr().(*net.UDPAddr)
	s := &Signaling{
		conn:      conn,
		networkID: nextNetworkID(),
		log:       opts.Log,
		peers:     make(map[uint64]*World),
		notifiers: make(map[uint32]nethernet.Notifier),
		closed:    make(chan struct{}),
	}

	if !opts.NoBroadcast {
		// Broadcast to both the limited broadcast address and the subnet-directed
		// one: Windows routes them differently and a host may only answer one.
		s.broadcasts = []*net.UDPAddr{{IP: net.IPv4bcast, Port: DefaultPort}}
		if local != nil {
			if ip := local.IP.To4(); ip != nil && !ip.IsUnspecified() {
				s.broadcasts = append(s.broadcasts, &net.UDPAddr{
					IP:   net.IPv4(ip[0], ip[1], ip[2], 255),
					Port: DefaultPort,
				})
			}
		}
		for _, ip := range interfaceBroadcasts() {
			s.broadcasts = append(s.broadcasts, &net.UDPAddr{IP: ip, Port: DefaultPort})
		}
		s.broadcasts = append(s.broadcasts, opts.ExtraBroadcasts...)
	}

	go s.readLoop()
	go s.broadcastLoop()
	go s.expireLoop()
	// ★ Also listen on port 7551 through a SO_REUSEADDR socket. Needed when we have to bind an
	// ephemeral port because the host application already holds 7551: such a world does NOT answer
	// unicast probes, it BROADCASTS a ResponsePacket to 255.255.255.255:7551. An ephemeral socket
	// never receives datagrams addressed to port 7551, so that port must be bound as well.
	go s.readSharedLoop()
	return s, nil
}

// interfaceBroadcasts returns the subnet-directed broadcast address of every
// up, non-loopback IPv4 interface. The game filters interfaces by whether they
// are "viable for broadcast domain", so covering all of them avoids missing a
// host that chose an interface we did not guess.
func interfaceBroadcasts() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil {
				continue
			}
			mask := ipnet.Mask
			if len(mask) == 16 {
				mask = mask[12:]
			}
			if len(mask) != 4 {
				continue
			}
			b := make(net.IP, 4)
			for i := range b {
				b[i] = ip[i] | ^mask[i]
			}
			out = append(out, b)
		}
	}
	return out
}

// NetworkID returns the local NetherNet network ID as a string.
func (s *Signaling) NetworkID() string {
	return strconv.FormatUint(s.networkID, 10)
}

// PongData sets the ServerData used to answer incoming RequestPackets.
//
// Two input formats are accepted:
//   - the classic RakNet "MCPE;...;" pong string. This is what a gophertunnel
//     listener produces, and what a NetherNet ingress listener forwards here
//     every few seconds through nethernet.Listener.PongData (CONN-002);
//   - an already-encoded ServerData blob (the original contract of this method).
//
// A LAN ResponsePacket carries BINARY ServerData, not the MCPE string, so the
// former has to be converted here or LAN clients cannot display this server.
func (s *Signaling) PongData(b []byte) {
	var raw []byte
	if strings.HasPrefix(string(b), "MCPE;") {
		converted, err := serverDataFromPong(b)
		if err != nil {
			s.log.Warn("lansignal: cannot convert pong to ServerData", "error", err)
			return
		}
		raw = converted
	} else {
		raw = append([]byte(nil), b...)
	}
	s.pongData.Store(&raw)
}

// serverDataFromPong converts gophertunnel's MCPE pong line into the binary
// ServerData payload a LAN ResponsePacket expects. This mirrors the parsing in
// go-nethernet/discovery.Listener.PongData, but also carries Protocol, Version
// and Nonce-free defaults so the world card has a real protocol/version.
func serverDataFromPong(b []byte) ([]byte, error) {
	parts := strings.Split(string(b), ";")
	if len(parts) < 9 {
		return nil, fmt.Errorf("pong has %d fields, want >= 9", len(parts))
	}
	proto, _ := strconv.ParseInt(parts[2], 10, 32)
	players, _ := strconv.Atoi(parts[4])
	maxPlayers, _ := strconv.Atoi(parts[5])

	var gameType int32
	switch strings.ToLower(strings.TrimSpace(parts[8])) {
	case "creative":
		gameType = discovery.GameTypeCreative
	case "adventure":
		gameType = discovery.GameTypeAdventure
	default:
		gameType = discovery.GameTypeSurvival
	}
	d := &discovery.ServerData{
		ServerName:            parts[1],
		Protocol:              int32(proto),
		Version:               parts[3],
		LevelName:             parts[7],
		GameType:              gameType,
		PlayerCount:           int32(players),
		MaxPlayerCount:        int32(maxPlayers),
		EditorWorld:           false,
		Hardcore:              false,
		AcceptsOnlineAuth:     true,
		AcceptsSelfSignedAuth: true,
		ConnectionType:        4, // LAN/NetherNet, as vanilla uses.
	}
	return d.MarshalBinary()
}

// Credentials always returns nil: LAN discovery carries no STUN/TURN servers.
func (s *Signaling) Credentials(context.Context) (*nethernet.Credentials, error) {
	select {
	case <-s.closed:
		return nil, net.ErrClosed
	default:
		return nil, nil
	}
}

// Context returns a context canceled when the Signaling is closed.
func (s *Signaling) Context() context.Context {
	return &closedContext{s.closed}
}

// DisableTrickleICE reports that ICE candidates are delivered in one batch,
// which is how LAN signaling behaves: candidates are only exchanged once the
// offer/answer pair is complete.
func (s *Signaling) DisableTrickleICE() bool { return true }

// Notify registers n for incoming signals.
func (s *Signaling) Notify(n nethernet.Notifier) (stop func()) {
	if n == nil {
		panic("lansignal: Notify(nil)")
	}
	s.notifiersMu.Lock()
	i := s.notifyCount
	s.notifiers[i] = n
	s.notifyCount++
	s.notifiersMu.Unlock()

	return sync.OnceFunc(func() {
		s.notifiersMu.Lock()
		delete(s.notifiers, i)
		s.notifiersMu.Unlock()
	})
}

// IsDialTarget reports whether networkID belongs to a world we have sent an
// OFFER to. Incoming signals from such a peer are answers to our own outbound
// dial, never offers from a client.
func (s *Signaling) IsDialTarget(networkID string) bool {
	id, err := strconv.ParseUint(networkID, 10, 64)
	if err != nil {
		return false
	}
	_, ok := s.dialTargets.Load(id)
	return ok
}

// Ingress returns a Signaling view suitable for a nethernet.Listener
// (CONN-002). Everything else is delegated to s, but notifiers registered
// through this view never see signals belonging to one of our own outbound
// world dials.
//
// Why it is needed: the relay reuses ONE Signaling both to dial the LAN world
// and to accept clients over NetherNet. deliver() broadcasts every received
// signal to every notifier. go-nethernet's dialer already filters by its own
// connection ID and stays quiet, but Listener.NotifySignal logs at ERROR level
// for any non-offer signal with no matching negotiation. The host's
// CONNECTRESPONSE therefore produced a spurious ERROR for every client
// session. Filtering at registration is the root-cause fix: outbound-dial
// traffic is simply not listener traffic.
func (s *Signaling) Ingress() nethernet.Signaling {
	return ingressSignaling{Signaling: s}
}

type ingressSignaling struct{ *Signaling }

func (s ingressSignaling) Notify(n nethernet.Notifier) (stop func()) {
	return s.Signaling.Notify(ingressNotifier{Notifier: n, sig: s.Signaling})
}

type ingressNotifier struct {
	nethernet.Notifier
	sig *Signaling
}

func (n ingressNotifier) NotifySignal(s *nethernet.Signal) bool {
	if n.sig.IsDialTarget(s.NetworkID) {
		// Silently claim the signal: it belongs to the host dial, not to a
		// client of this listener. Returning true keeps deliver() from logging
		// a rejection too.
		return true
	}
	return n.Notifier.NotifySignal(s)
}

// Signal delivers a signal to the peer with the given network ID. The address is
// the one recorded when that peer last sent us a packet.
func (s *Signaling) Signal(ctx context.Context, signal *nethernet.Signal) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-s.closed:
		return net.ErrClosed
	default:
	}

	id, err := strconv.ParseUint(signal.NetworkID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse network ID %q: %w", signal.NetworkID, err)
	}
	if signal.Type == nethernet.SignalTypeOffer {
		// This peer is a world WE are dialling. Remember it so Ingress() can
		// keep its answers/candidates away from the client-facing listener.
		s.dialTargets.Store(id, struct{}{})
	}

	s.peersMu.RLock()
	peer, ok := s.peers[id]
	var addr net.Addr
	if ok {
		addr = peer.Addr
	}
	s.peersMu.RUnlock()

	if !ok || addr == nil {
		return fmt.Errorf("no address known for network ID %d", id)
	}

	pk := discovery.Marshal(&discovery.MessagePacket{
		RecipientID: id,
		Data:        signal.String(),
	}, s.networkID)

	if _, err := s.conn.WriteTo(pk, addr); err != nil {
		return fmt.Errorf("send signal to %v: %w", addr, err)
	}
	s.log.Debug("lansignal: sent signal",
		"to", addr, "network_id", id, "type", signal.Type, "conn", signal.ConnectionID)
	return nil
}

// Worlds returns the peers seen so far, newest first.
func (s *Signaling) Worlds() []World {
	s.peersMu.RLock()
	defer s.peersMu.RUnlock()

	out := make([]World, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, *p)
	}
	// Newest first so a freshly announced host appears at the top.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].LastSeen.After(out[j-1].LastSeen); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Peers returns the number of known peers.
func (s *Signaling) Peers() int {
	s.peersMu.RLock()
	defer s.peersMu.RUnlock()
	return len(s.peers)
}

// LocalAddr returns the bound UDP address.
func (s *Signaling) LocalAddr() net.Addr { return s.conn.LocalAddr() }

// Close stops the loops and releases the socket.
func (s *Signaling) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		err = s.conn.Close()
		s.notifiersMu.Lock()
		clear(s.notifiers)
		s.notifiersMu.Unlock()
	})
	return err
}

// readLoop consumes every discovery packet and dispatches it by type.
func (s *Signaling) readLoop() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := s.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			s.log.Error("lansignal: read", "error", err)
			_ = s.Close()
			return
		}

		pk, senderID, err := discovery.Unmarshal(buf[:n])
		if err != nil {
			// Foreign or malformed traffic on 7551 is normal; keep listening.
			s.log.Debug("lansignal: decode", "error", err, "from", addr, "len", n)
			continue
		}
		if senderID == s.networkID {
			continue // our own broadcast looped back
		}

		switch pk := pk.(type) {
		case *discovery.RequestPacket:
			// A host announcing itself, or another client looking for worlds.
			// Either way its sender ID is a usable NetherNet network ID.
			s.record(senderID, addr, func(w *World) { w.Announced = true })
			s.log.Debug("lansignal: peer announced", "network_id", senderID, "from", addr)
			s.replyRequest(addr)

		case *discovery.ResponsePacket:
			raw := append([]byte(nil), pk.ApplicationData...)
			host, hostErr := DecodeHostInfo(raw)
			if hostErr != nil {
				s.log.Warn("lansignal: undecodable ServerData",
					"network_id", senderID, "error", hostErr, "from", addr,
					"bytes", len(raw))
			}

			sd := &discovery.ServerData{}
			if err := sd.UnmarshalBinary(raw); err != nil {
				s.log.Debug("lansignal: stock ServerData decoder rejected payload",
					"network_id", senderID, "error", err, "from", addr)
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
			name, level := "", ""
			if host != nil {
				name, level = host.ServerName, host.LevelName
			}
			s.log.Debug("lansignal: world answered",
				"network_id", senderID, "from", addr,
				"level", level, "host", name)

		case *discovery.MessagePacket:
			if pk.RecipientID != s.networkID {
				continue
			}
			if pk.Data == "Ping" || pk.Data == "" {
				continue
			}
			signal := &nethernet.Signal{}
			if err := signal.UnmarshalText([]byte(pk.Data)); err != nil {
				s.log.Warn("lansignal: bad signal", "error", err, "from", addr)
				continue
			}
			// CONN-002: remember the sender BEFORE delivering the signal. When this
			// Signaling also backs a nethernet.Listener, the CONNECTRESPONSE is sent
			// back through Signal(), which resolves the peer by network ID. A peer
			// that only ever sent MessagePackets would otherwise be unroutable —
			// the exact "no address known" failure seen on a LAN host that never
			// announces itself with a RequestPacket.
			s.record(senderID, addr, nil)
			signal.NetworkID = strconv.FormatUint(senderID, 10)
			s.deliver(signal)
		}
	}
}

// record stores or refreshes a peer entry.
func (s *Signaling) record(id uint64, addr net.Addr, mutate func(*World)) {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	w, ok := s.peers[id]
	if !ok {
		w = &World{NetworkID: id}
		s.peers[id] = w
	}
	// ★ The same world can be announced on SEVERAL interfaces (real Wi-Fi plus WSL/Hyper-V).
	// Keep the address with the higher score so Signal() sends WebRTC out of the right interface.
	// See addrScore.
	if addr != nil && (w.Addr == nil || addrScore(addr) > addrScore(w.Addr)) {
		w.Addr = addr
	}
	w.LastSeen = time.Now()
	if mutate != nil {
		mutate(w)
	}
}

// addrScore ranks addresses when the same world shows up on several interfaces.
//
// The lesson: a machine usually has a real Wi-Fi address (192.168.1.x) alongside WSL/Hyper-V
// (172.31.80.1) and link-local (169.254.x). Pick the wrong one and WebRTC goes out of a virtual
// interface and never connects — even though the world is real.
//
// Higher score = preferred. For THIS machine's own IPs the interface name decides (vEthernet/WSL/
// Hyper-V/VirtualBox/VMware/Tailscale score low). For other machines' IPs the address range decides.
func addrScore(addr net.Addr) int {
	udp, ok := addr.(*net.UDPAddr)
	if !ok || udp == nil {
		return 0
	}
	ip := udp.IP.To4()
	if ip == nil {
		return 0
	}
	if ip.IsLoopback() {
		return -100
	}
	if ip.IsLinkLocalUnicast() {
		return -50
	}
	if name := localInterfaceName(ip); name != "" {
		lower := strings.ToLower(name)
		for _, bad := range []string{"vethernet", "wsl", "hyper-v", "virtualbox", "vmware", "loopback", "tailscale", "zerotier"} {
			if strings.Contains(lower, bad) {
				return 1
			}
		}
	}
	switch {
	case ip[0] == 192 && ip[1] == 168:
		return 30
	case ip[0] == 10:
		return 25
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
		return 5
	default:
		return 20
	}
}

// localInterfaceName returns the name of the interface holding ip, or "" when ip does not belong to
// this machine. Cached once: interfaces rarely change during a relay session.
var (
	localIPsOnce sync.Once
	localIPName  map[string]string
)

func localInterfaceName(ip net.IP) string {
	localIPsOnce.Do(func() {
		localIPName = make(map[string]string)
		ifaces, err := net.Interfaces()
		if err != nil {
			return
		}
		for _, ifc := range ifaces {
			addrs, err := ifc.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
					localIPName[ipn.IP.To4().String()] = ifc.Name
				}
			}
		}
	})
	return localIPName[ip.String()]
}

// replyRequest answers a RequestPacket with our ServerData when one is set.
func (s *Signaling) replyRequest(addr net.Addr) {
	data := s.pongData.Load()
	if data == nil {
		return
	}
	pk := discovery.Marshal(&discovery.ResponsePacket{ApplicationData: *data}, s.networkID)
	if _, err := s.conn.WriteTo(pk, addr); err != nil {
		s.log.Debug("lansignal: reply failed", "error", err, "to", addr)
	}
}

// deliver hands a signal to every registered notifier.
func (s *Signaling) deliver(signal *nethernet.Signal) {
	s.log.Debug("lansignal: received signal",
		"from_network_id", signal.NetworkID, "type", signal.Type, "conn", signal.ConnectionID)
	s.notifiersMu.RLock()
	ns := make([]nethernet.Notifier, 0, len(s.notifiers))
	for _, n := range s.notifiers {
		ns = append(ns, n)
	}
	s.notifiersMu.RUnlock()
	for _, n := range ns {
		if !n.NotifySignal(signal) {
			s.log.Debug("lansignal: signal rejected by notifier",
				"type", signal.Type, "conn", signal.ConnectionID)
		}
	}
}

// broadcastLoop periodically sends RequestPackets so hosts that *do* listen on
// 7551 answer us, complementing the passive discovery of announcing hosts.
func (s *Signaling) broadcastLoop() {
	if len(s.broadcasts) == 0 {
		return
	}
	pk := discovery.Marshal(&discovery.RequestPacket{}, s.networkID)
	ticker := time.NewTicker(broadcastInterval)
	defer ticker.Stop()

	send := func() {
		for _, dst := range s.broadcasts {
			if _, err := s.conn.WriteTo(pk, dst); err != nil {
				select {
				case <-s.closed:
					return
				default:
				}
				s.log.Debug("lansignal: broadcast failed", "dst", dst, "error", err)
			}
		}
	}
	send() // send immediately so a short timeout still finds worlds
	for {
		select {
		case <-s.closed:
			return
		case <-ticker.C:
			send()
		}
	}
}

// expireLoop drops peers that have gone quiet.
func (s *Signaling) expireLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-peerTimeout)
			s.peersMu.Lock()
			for id, p := range s.peers {
				if p.LastSeen.Before(cutoff) {
					delete(s.peers, id)
				}
			}
			s.peersMu.Unlock()
		}
	}
}

// closedContext is a context canceled when its channel closes.
type closedContext struct{ done <-chan struct{} }

func (closedContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c closedContext) Done() <-chan struct{}     { return c.done }
func (closedContext) Value(any) any               { return nil }

func (c closedContext) Err() error {
	select {
	case <-c.done:
		return net.ErrClosed
	default:
		return nil
	}
}
