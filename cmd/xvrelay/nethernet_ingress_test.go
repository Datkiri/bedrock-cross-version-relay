package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"bedrock-cross-version-relay/internal/lansignal"

	"github.com/df-mc/go-nethernet"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// CONN-002 loopback: prove that the -listen-nethernet code path
// (minecraft.ListenConfig.ListenNetwork with minecraft.NetherNet over our own
// lansignal.Signaling) really accepts a NetherNet client and completes the
// Minecraft login — with real WebRTC/ICE, real LAN discovery signalling and no
// external game, BDS or Xbox service.
//
// Two lansignal.Signaling instances stand in for the relay and one joining
// client. The relay's signaling is exactly the same object main() uses for host
// discovery/dial, so this also pins that a single Signaling can be both halves
// of a connection.
func TestNetherNetIngressLoopback(t *testing.T) {
	// Set XVER_NNET_DEBUG=1 to see the full WebRTC/ICE/Login trace while
	// diagnosing this loopback. CI keeps the default silent log.
	var out io.Writer = io.Discard
	level := slog.LevelInfo
	if os.Getenv("XVER_NNET_DEBUG") != "" {
		out, level = os.Stderr, slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))

	// Both sides need to know each other's UDP address: Signal() resolves a peer
	// by network ID and refuses with "no address known" otherwise. Each side
	// therefore broadcasts a LAN RequestPacket to the other's port.
	//
	// ★ Race fix (found under `go test -race`): the old version passed mutable
	// *UDPAddr values with Port=0 into ExtraBroadcasts and then wrote `.Port`
	// after Listen(). broadcastLoop calls send() IMMEDIATELY in its own
	// goroutine, so that write raced UDPConn.WriteTo reading the address.
	// Reserve both ports up front instead, and never mutate an address that a
	// Signaling goroutine can see.
	// Keep BOTH probe sockets open at once so the OS is forced to hand out two
	// distinct ephemeral ports (closing the first before probing the second can
	// legitimately return the same port twice).
	reservePorts := func() (int, int) {
		a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		b, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			_ = a.Close()
			t.Fatal(err)
		}
		pa := a.LocalAddr().(*net.UDPAddr).Port
		pb := b.LocalAddr().(*net.UDPAddr).Port
		_ = a.Close()
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		return pa, pb
	}
	relayPort, clientPort := reservePorts()
	peerOfRelay := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: relayPort}
	peerOfClient := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: clientPort}
	relaySig, err := lansignal.Listen(lansignal.Options{
		Addr: fmt.Sprintf("127.0.0.1:%d", relayPort), Log: log,
		ExtraBroadcasts: []*net.UDPAddr{peerOfClient},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relaySig.Close() })
	clientSig, err := lansignal.Listen(lansignal.Options{
		Addr: fmt.Sprintf("127.0.0.1:%d", clientPort), Log: log,
		ExtraBroadcasts: []*net.UDPAddr{peerOfRelay},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSig.Close() })

	relayID := mustParseID(t, relaySig.NetworkID())
	clientID := mustParseID(t, clientSig.NetworkID())
	deadline := time.Now().Add(8 * time.Second)
	for {
		relayKnowsClient, clientKnowsRelay := false, false
		for _, w := range relaySig.Worlds() {
			if w.NetworkID == clientID && w.Addr != nil {
				relayKnowsClient = true
			}
		}
		for _, w := range clientSig.Worlds() {
			if w.NetworkID == relayID && w.Addr != nil {
				clientKnowsRelay = true
			}
		}
		if relayKnowsClient && clientKnowsRelay {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("LAN discovery did not become bidirectional: relayKnowsClient=%v clientKnowsRelay=%v (relay peers=%d client peers=%d)",
				relayKnowsClient, clientKnowsRelay, relaySig.Peers(), clientSig.Peers())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The exact construction used by -listen-nethernet in main(), including
	// the Ingress() view so the loopback also exercises the dial-target filter.
	listener, err := (minecraft.ListenConfig{
		AuthenticationDisabled: true,
		ErrorLog:               log,
		AllowUnknownPackets:    true,
		AllowInvalidPackets:    true,
	}).ListenNetwork(minecraft.NetherNet{
		Signaling: relaySig.Ingress(),
		ListenConfig: nethernet.ListenConfig{
			AllowAnonymous: true, // LAN/self-signed clients send no identity assertion.
			Log:            log,
		},
		Log: log,
	}, relaySig.NetworkID())
	if err != nil {
		t.Fatalf("ListenNetwork(NetherNet): %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	if got := listener.Addr().Network(); got != "nethernet" {
		t.Fatalf("listener.Addr().Network() = %q, want nethernet", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		c, e := listener.Accept()
		accepted <- acceptResult{c, e}
	}()

	dialed := make(chan acceptResult, 1)
	go func() {
		c, e := (minecraft.Dialer{
			ErrorLog:   log,
			ClientData: login.ClientData{GameVersion: protocol.CurrentVersion},
			// DisplayName must be <= 16 chars: gophertunnel validates the Login
			// packet on the listener side and closes the transport otherwise.
			IdentityData: login.IdentityData{DisplayName: "NNetIngress"},
		}).DialContextNetwork(ctx, fixedIDNetherNet{
			NetherNet: minecraft.NetherNet{
				Signaling: clientSig,
				Log:       log,
				Dialer: nethernet.Dialer{
					AllowIdentitylessServer: true,
					DisableTrickleICE:       true,
				},
			},
			networkID: relaySig.NetworkID(),
		}, "127.0.0.1:19132")
		dialed <- acceptResult{c, e}
	}()

	// Accept() proves the NetherNet client completed Login. Just like the RakNet
	// loopback in compatibility_test.go, DialContextNetwork only returns after
	// the server explicitly calls StartGame(): it waits for PlayStatusPlayerSpawn.
	var serverConn net.Conn
	select {
	case r := <-accepted:
		if r.err != nil {
			t.Fatalf("listener.Accept(): %v", r.err)
		}
		serverConn = r.conn
	case <-ctx.Done():
		t.Fatalf("listener.Accept() timed out: %v", ctx.Err())
	}
	t.Cleanup(func() { _ = serverConn.Close() })

	startGameDone := make(chan error, 1)
	go func() {
		startGameDone <- serverConn.(*minecraft.Conn).StartGame(minecraft.GameData{WorldName: "NNetIngress"})
	}()

	var clientConn net.Conn
	select {
	case r := <-dialed:
		if r.err != nil {
			t.Fatalf("DialContextNetwork(NetherNet): %v", r.err)
		}
		clientConn = r.conn
	case <-ctx.Done():
		t.Fatalf("DialContextNetwork(NetherNet) timed out: %v", ctx.Err())
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	select {
	case err := <-startGameDone:
		if err != nil {
			t.Fatalf("listener StartGame(): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener StartGame() did not finish after the client spawned")
	}

	if got := serverConn.RemoteAddr().Network(); got != "nethernet" {
		t.Fatalf("accepted conn RemoteAddr().Network() = %q, want nethernet", got)
	}
	if _, ok := serverConn.(*minecraft.Conn); !ok {
		t.Fatalf("Accept() returned %T, want *minecraft.Conn", serverConn)
	}
	if _, ok := clientConn.(*minecraft.Conn); !ok {
		t.Fatalf("DialContextNetwork returned %T, want *minecraft.Conn", clientConn)
	}

	// A real packet must cross the WebRTC data channels after login; otherwise
	// this would only prove the listener object was constructed.
	sc := serverConn.(*minecraft.Conn)
	cc := clientConn.(*minecraft.Conn)
	const marker = "nethernet-ingress-ok"
	if err := sc.WritePacket(&packet.Text{Message: marker, TextType: packet.TextTypeRaw}); err != nil {
		t.Fatalf("server WritePacket: %v", err)
	}
	readDone := make(chan error, 1)
	go func() {
		// The spawn sequence (StartGame, JigsawStructureData, VoxelShapes,
		// ChunkRadiusUpdated, PlayStatus...) is still in flight: skip those
		// until our own Text packet arrives.
		for {
			pk, err := cc.ReadPacket()
			if err != nil {
				readDone <- err
				return
			}
			txt, ok := pk.(*packet.Text)
			if !ok {
				continue
			}
			if txt.Message != marker {
				readDone <- &unexpectedPacket{pk}
				return
			}
			readDone <- nil
			return
		}
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("client ReadPacket: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("packet did not cross the NetherNet connection after login")
	}
}

type unexpectedPacket struct{ pk packet.Packet }

func (e *unexpectedPacket) Error() string {
	return fmt.Sprintf("unexpected packet %T", e.pk)
}

func mustParseID(t *testing.T, s string) uint64 {
	t.Helper()
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("bad network ID %q: %v", s, err)
	}
	return id
}

// fixedIDNetherNet decouples the Minecraft-level ServerAddress (which
// gophertunnel's listener validates as a resolvable UDP address) from the
// NetherNet network ID (a decimal uint64). DialContextNetwork reuses ONE
// string for both: it is passed to Network.DialContext AND copied into
// ClientData.ServerAddress. A real Bedrock client sends a real IP:port in
// ServerAddress and dials the network ID out of band, so only a loopback
// dialer built on gophertunnel needs this shim. Without it the listener
// rejects Login with "ServerAddress must be resolveable as a UDP address".
type fixedIDNetherNet struct {
	minecraft.NetherNet
	networkID string
}

func (n fixedIDNetherNet) DialContext(ctx context.Context, _ string) (net.Conn, error) {
	return n.NetherNet.DialContext(ctx, n.networkID)
}
