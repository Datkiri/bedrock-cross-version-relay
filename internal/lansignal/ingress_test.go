package lansignal

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/df-mc/go-nethernet"
)

// recordingNotifier captures the signals a Signaling delivers to it.
type recordingNotifier struct {
	mu  sync.Mutex
	got []*nethernet.Signal
}

func (n *recordingNotifier) NotifySignal(s *nethernet.Signal) bool {
	n.mu.Lock()
	n.got = append(n.got, s)
	n.mu.Unlock()
	return true
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.got)
}

func (n *recordingNotifier) last() *nethernet.Signal {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.got) == 0 {
		return nil
	}
	return n.got[len(n.got)-1]
}

// newTestSignaling builds a Signaling without starting any of the background
// loops, so these tests are fully offline and deterministic.
func newTestSignaling(t *testing.T) *Signaling {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Signaling{
		conn:      conn,
		networkID: nextNetworkID(),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		peers:     make(map[uint64]*World),
		notifiers: make(map[uint32]nethernet.Notifier),
		closed:    make(chan struct{}),
	}
}

// TestIngressSignalingFiltersOutboundDialSignals pins CONN-002's root-cause
// fix: one Signaling backs BOTH the outbound world dial and the inbound
// NetherNet listener. An answer/candidate from the dialled world must reach the
// dialer notifier but never the listener notifier, or go-nethernet logs
// "received non-offer signal for a non-existing negotiation" for every join.
func TestIngressSignalingFiltersOutboundDialSignals(t *testing.T) {
	const (
		hostID   = uint64(15619059520673756992) // the real LAN world
		clientID = uint64(1793884236535645300)  // a client of the relay
	)
	hostStr := strconv.FormatUint(hostID, 10)
	clientStr := strconv.FormatUint(clientID, 10)

	s := newTestSignaling(t)
	s.record(hostID, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: DefaultPort}, nil)

	// Exactly what nethernet.Dialer.DialContext does before sending the offer.
	if err := s.Signal(context.Background(), &nethernet.Signal{
		Type:         nethernet.SignalTypeOffer,
		NetworkID:    hostStr,
		ConnectionID: 111,
		Data:         "offer",
	}); err != nil {
		t.Fatalf("signal offer: %v", err)
	}
	if !s.IsDialTarget(hostStr) {
		t.Fatal("host was not recorded as a dial target")
	}
	if s.IsDialTarget(clientStr) {
		t.Fatal("a client must not be a dial target")
	}

	ingress := &recordingNotifier{}
	dialer := &recordingNotifier{}
	stopIngress := s.Ingress().Notify(ingress)
	defer stopIngress()
	stopDialer := s.Notify(dialer)
	defer stopDialer()

	// Host CONNECTRESPONSE: only the dialer may see it.
	s.deliver(&nethernet.Signal{
		Type: nethernet.SignalTypeAnswer, NetworkID: hostStr, ConnectionID: 111,
	})
	if got := dialer.count(); got != 1 {
		t.Fatalf("dialer received %d signals, want 1", got)
	}
	if got := ingress.count(); got != 0 {
		t.Fatalf("listener received %d host-answer signals, want 0", got)
	}

	// Host CANDIDATEADD: also dial traffic, also filtered.
	s.deliver(&nethernet.Signal{
		Type: nethernet.SignalTypeCandidate, NetworkID: hostStr, ConnectionID: 111,
	})
	if got := ingress.count(); got != 0 {
		t.Fatalf("listener received %d host-candidate signals, want 0", got)
	}
	if got := dialer.count(); got != 2 {
		t.Fatalf("dialer received %d signals, want 2", got)
	}

	// A client OFFER is genuine listener traffic and must be delivered.
	s.deliver(&nethernet.Signal{
		Type: nethernet.SignalTypeOffer, NetworkID: clientStr, ConnectionID: 222,
	})
	if got := ingress.count(); got != 1 {
		t.Fatalf("listener received %d client offers, want 1", got)
	}
	if last := ingress.last(); last == nil || last.NetworkID != clientStr || last.Type != nethernet.SignalTypeOffer {
		t.Fatalf("listener got wrong signal: %+v", last)
	}
}

// TestIngressSignalingWithoutDialTargetsPassesEverything guards the default
// case: with -host-raknet there is no NetherNet outbound dial, so the listener
// must still receive all client signals.
func TestIngressSignalingWithoutDialTargetsPassesEverything(t *testing.T) {
	s := newTestSignaling(t)
	ingress := &recordingNotifier{}
	stop := s.Ingress().Notify(ingress)
	defer stop()

	s.deliver(&nethernet.Signal{
		Type: nethernet.SignalTypeOffer, NetworkID: "42", ConnectionID: 7,
	})
	s.deliver(&nethernet.Signal{
		Type: nethernet.SignalTypeCandidate, NetworkID: "42", ConnectionID: 7,
	})
	if got := ingress.count(); got != 2 {
		t.Fatalf("listener received %d signals, want 2", got)
	}
}
