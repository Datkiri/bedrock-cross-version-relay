package lansignal

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/df-mc/go-nethernet/discovery"
)

// CONN-002: a gophertunnel listener forwards its MCPE pong STRING into
// nethernet.Listener.PongData, which lands here. A LAN ResponsePacket must
// carry BINARY ServerData, not that string.
func TestPongDataConvertsMCPEPongToServerData(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Listen(Options{Addr: "127.0.0.1:0", NoBroadcast: true, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	pong := "MCPE;Relay;765;1.26.51;2;8;123456;Cross Version;Creative;1;19132;19132;0,0;"
	s.PongData([]byte(pong))

	stored := s.pongData.Load()
	if stored == nil {
		t.Fatal("PongData did not store anything")
	}
	raw := *stored
	if bytes.HasPrefix(raw, []byte("MCPE;")) {
		t.Fatalf("pong was stored verbatim, not converted to binary ServerData: %q", raw)
	}
	d := &discovery.ServerData{}
	if err := d.UnmarshalBinary(raw); err != nil {
		t.Fatalf("stored payload is not decodable ServerData: %v", err)
	}
	if d.ServerName != "Relay" || d.Protocol != 765 || d.Version != "1.26.51" {
		t.Errorf("identity fields = %q/%d/%q, want Relay/765/1.26.51", d.ServerName, d.Protocol, d.Version)
	}
	if d.LevelName != "Cross Version" || d.GameType != discovery.GameTypeCreative {
		t.Errorf("world fields = %q/%d, want Cross Version/creative", d.LevelName, d.GameType)
	}
	if d.PlayerCount != 2 || d.MaxPlayerCount != 8 {
		t.Errorf("player counts = %d/%d, want 2/8", d.PlayerCount, d.MaxPlayerCount)
	}
	if d.ConnectionType != 4 {
		t.Errorf("ConnectionType = %d, want 4 (LAN/NetherNet)", d.ConnectionType)
	}
	if !d.AcceptsSelfSignedAuth {
		t.Error("AcceptsSelfSignedAuth = false; LAN clients would be rejected")
	}
}

// Backwards compatibility: callers that already hand us an encoded ServerData
// blob must keep working.
func TestPongDataKeepsBinaryServerData(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Listen(Options{Addr: "127.0.0.1:0", NoBroadcast: true, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	want, err := (&discovery.ServerData{ServerName: "Raw", ConnectionType: 4}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	s.PongData(want)
	if got := *s.pongData.Load(); !bytes.Equal(got, want) {
		t.Fatalf("binary ServerData was rewritten: got %x, want %x", got, want)
	}
}

// captureNotifier stands in for nethernet.Listener / nethernet.Conn.
type captureNotifier struct{ ch chan *nethernet.Signal }

func (n captureNotifier) NotifySignal(s *nethernet.Signal) bool {
	select {
	case n.ch <- s:
	default:
	}
	return true
}

// CONN-002: two Signaling instances created back-to-back must not share a
// network ID. Windows' UnixNano clock is coarse enough that the old
// time.Now().UnixNano() assignment collided, making each peer discard the
// other's packets as "our own broadcast".
func TestNetworkIDsAreUniqueAcrossRapidListens(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	seen := make(map[uint64]bool)
	for i := 0; i < 8; i++ {
		s, err := Listen(Options{Addr: "127.0.0.1:0", NoBroadcast: true, Log: log})
		if err != nil {
			t.Fatal(err)
		}
		if seen[s.networkID] {
			t.Fatalf("duplicate network ID %d at instance %d", s.networkID, i)
		}
		seen[s.networkID] = true
		_ = s.Close()
	}
}

// CONN-002: the same lansignal.Signaling that dials a host must also be able to
// answer an incoming CONNECTREQUEST. This exercises the full signaling loop
// WITHOUT WebRTC: MessagePacket -> record(sender) -> Notify -> Signal(answer).
func TestSignalingRoundTripsConnectRequestAndResponse(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := Listen(Options{Addr: "127.0.0.1:0", NoBroadcast: true, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := Listen(Options{Addr: "127.0.0.1:0", NoBroadcast: true, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if server.networkID == client.networkID {
		t.Fatalf("network ID collision: both are %d", server.networkID)
	}

	serverIn := make(chan *nethernet.Signal, 4)
	clientIn := make(chan *nethernet.Signal, 4)
	stopServer := server.Notify(captureNotifier{serverIn})
	defer stopServer()
	stopClient := client.Notify(captureNotifier{clientIn})
	defer stopClient()

	// A client that ONLY ever sent MessagePackets (no Request/Response) must
	// still be routable: Signal() resolves the peer by network ID.
	offer := (&nethernet.Signal{
		Type: nethernet.SignalTypeOffer, ConnectionID: 42, Data: "v=0",
	}).String()
	pk := discovery.Marshal(&discovery.MessagePacket{
		RecipientID: server.networkID,
		Data:        offer,
	}, client.networkID)
	// UDP has no delivery guarantee and the peer's readLoop is a goroutine that
	// may not have entered ReadFrom yet. Re-send until the notifier sees it.
	var gotOffer *nethernet.Signal
	deadline := time.Now().Add(4 * time.Second)
	for gotOffer == nil {
		if _, err := client.conn.WriteTo(pk, server.conn.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		select {
		case gotOffer = <-serverIn:
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("server never received CONNECTREQUEST")
		}
	}
	if gotOffer.Type != nethernet.SignalTypeOffer || gotOffer.ConnectionID != 42 {
		t.Fatalf("server got %q conn=%d, want CONNECTREQUEST/42", gotOffer.Type, gotOffer.ConnectionID)
	}
	if gotOffer.NetworkID != strconv.FormatUint(client.networkID, 10) {
		t.Fatalf("offer NetworkID = %q, want client %d", gotOffer.NetworkID, client.networkID)
	}

	// Verify the fix: the sender was recorded from the MessagePacket alone.
	server.peersMu.RLock()
	peer, ok := server.peers[client.networkID]
	server.peersMu.RUnlock()
	if !ok || peer.Addr == nil {
		t.Fatal("MessagePacket sender was not recorded; Signal() could not route CONNECTRESPONSE")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	answer := &nethernet.Signal{
		Type: nethernet.SignalTypeAnswer, ConnectionID: 42, Data: "v=0-answer",
		NetworkID: strconv.FormatUint(client.networkID, 10),
	}
	// Same UDP retry as the offer: a single datagram can be dropped before the
	// client's readLoop is scheduled.
	var gotAnswer *nethernet.Signal
	deadline = time.Now().Add(4 * time.Second)
	for gotAnswer == nil {
		if err := server.Signal(ctx, answer); err != nil {
			t.Fatalf("server.Signal(CONNECTRESPONSE): %v", err)
		}
		select {
		case gotAnswer = <-clientIn:
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("client never received CONNECTRESPONSE")
		}
	}
	if gotAnswer.Type != nethernet.SignalTypeAnswer || gotAnswer.Data != "v=0-answer" {
		t.Fatalf("client got %q data=%q, want CONNECTRESPONSE/v=0-answer", gotAnswer.Type, gotAnswer.Data)
	}
	if gotAnswer.NetworkID != strconv.FormatUint(server.networkID, 10) {
		t.Fatalf("answer NetworkID = %q, want server %d", gotAnswer.NetworkID, server.networkID)
	}
}
