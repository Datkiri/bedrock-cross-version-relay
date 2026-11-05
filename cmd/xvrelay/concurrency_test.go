package main

// CONN-004: concurrency harness.
//
// The relay keeps per-client state in two sync.Maps (sessions keyed by *minecraft.Conn,
// clientGates keyed by remote address) plus a spawnCapture / clientGate full of atomics.
// The existing isolation tests (compatibility_test.go) drive those paths SEQUENTIALLY.
// These tests drive them from many goroutines at once, over REAL loopback RakNet
// connections, and are meant to be run under `go test -race` to actually prove there is
// no data race and no state bleeding between concurrent sessions.
//
// No BDS, no Xbox service, no LAN discovery: negotiatedConn() builds an in-process
// listener + dialer, the same helper the compatibility tests use.

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

const (
	concurrentSessionCount = 8
	concurrentIterations   = 50
)

func countSyncMap(m *sync.Map) int {
	n := 0
	m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

type concurrentFixture struct {
	conn   *minecraft.Conn
	addr   net.Addr
	sess   *session
	gate   *clientGate
	jigsaw []byte
	voxel  []byte
}

// registerConcurrentSession mirrors the relay's handleClient setup (main.go ~2175):
// one session + one gate per connection, stored in both global maps.
func registerConcurrentSession(c *minecraft.Conn, i int) concurrentFixture {
	jigsaw := []byte(fmt.Sprintf("jigsaw-session-%02d", i))
	voxel := []byte(fmt.Sprintf("voxel-session-%02d", i))
	s := &session{
		hostProto: 2193,
		capture:   &spawnCapture{},
		gate:      newSpawnPrefixGate(),
		reqs:      newSubChunkRequests(),
	}
	s.capture.jigsaw.Store(&jigsaw)
	s.capture.voxelShapes.Store(&voxel)
	sessions.Store(c, s)
	cg := &clientGate{gate: s.gate}
	clientGates.Store(c.RemoteAddr().String(), cg)
	return concurrentFixture{conn: c, addr: c.RemoteAddr(), sess: s, gate: cg, jigsaw: jigsaw, voxel: voxel}
}

func (f concurrentFixture) cleanup() {
	sessions.Delete(f.conn)
	// Identity-guarded delete, exactly as handleClient does: a newer session that reused
	// this address must not lose its gate when this one ends.
	if cur, ok := clientGates.Load(f.addr.String()); ok && cur == f.gate {
		clientGates.Delete(f.addr.String())
	}
}

// assertOwnPrelude checks the per-session spawn prelude really came from THIS session's
// spawnCapture. If two sessions shared a capture, every goroutine would see session 0's bytes.
func assertOwnPrelude(out []packet.Packet, want []byte, label string) error {
	if len(out) != 1 {
		return fmt.Errorf("%s: got %d packets, want 1", label, len(out))
	}
	raw, ok := out[0].(*packet.Unknown)
	if !ok {
		return fmt.Errorf("%s: got %T, want *packet.Unknown", label, out[0])
	}
	if string(raw.Payload) != string(want) {
		return fmt.Errorf("%s: payload %q leaked from another session, want %q", label, raw.Payload, want)
	}
	return nil
}

// TestConcurrentSessionsIsolated runs N negotiated loopback clients at the SAME time.
// Each must resolve its own session, emit its OWN captured prelude bytes, keep its OWN
// packet counter, and leave both global maps back at their baseline after cleanup.
func TestConcurrentSessionsIsolated(t *testing.T) {
	baseSessions := countSyncMap(&sessions)
	baseGates := countSyncMap(&clientGates)
	prevHostProto := hostProtoID.Load()
	hostProtoID.Store(2193)
	defer hostProtoID.Store(prevHostProto)

	// negotiatedConn uses t.Fatal/t.Cleanup, so build all connections on the test goroutine.
	conns := make([]*minecraft.Conn, concurrentSessionCount)
	for i := range conns {
		conns[i] = negotiatedConn(t, 2193)
	}
	fx := make([]concurrentFixture, concurrentSessionCount)
	for i, c := range conns {
		fx[i] = registerConcurrentSession(c, i)
	}
	cleanup := func() {
		for _, f := range fx {
			f.cleanup()
		}
	}
	defer cleanup() // safety net; also called explicitly below for the leak assertion

	errs := make(chan error, concurrentSessionCount*4)
	var wg sync.WaitGroup
	for _, f := range fx {
		wg.Add(1)
		go func(f concurrentFixture) {
			defer wg.Done()

			// 1. sessions.Load must return THIS session, never a neighbour's.
			if v, ok := sessions.Load(f.conn); !ok || v != f.sess {
				errs <- fmt.Errorf("session lookup returned %v (present=%v), want the session's own pointer", v, ok)
				return
			}

			// 2. Per-session captured prelude bytes, through the real
			//    clientVersionProtocol.ConvertFromLatest path (which does sessions.Load).
			proto := f.conn.Proto()
			if err := assertOwnPrelude(
				proto.ConvertFromLatest(&packet.JigsawStructureData{}, f.conn), f.jigsaw,
				"JigsawStructureData"); err != nil {
				errs <- err
			}
			if err := assertOwnPrelude(
				proto.ConvertFromLatest(&packet.VoxelShapes{}, f.conn), f.voxel,
				"VoxelShapes"); err != nil {
				errs <- err
			}

			// 3. The listener-side PacketFunc must increment only THIS session's counter.
			clientPacketFunc(packet.Header{PacketID: packet.IDSetLocalPlayerAsInitialised},
				make([]byte, 9), f.addr, nil)

			// 4. Per-session atomics are also touched concurrently.
			f.sess.reqLogged.Add(1)
			f.sess.clientRequested.CompareAndSwap(false, true)
		}(f)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	for i, f := range fx {
		if n := f.gate.count.Load(); n != 1 {
			t.Errorf("session %d gate counter = %d, want 1 (another session's packets leaked in)", i, n)
		}
		if !f.gate.saw113.Load() {
			t.Errorf("session %d did not record its own id=113", i)
		}
		if n := f.sess.reqLogged.Load(); n != 1 {
			t.Errorf("session %d reqLogged = %d, want 1", i, n)
		}
		if !f.sess.clientRequested.Load() {
			t.Errorf("session %d clientRequested was not set", i)
		}
		if !f.sess.capture.jigsawSent.Load() || !f.sess.capture.voxelSent.Load() {
			t.Errorf("session %d did not mark its own prelude as sent", i)
		}
	}

	cleanup()
	if got := countSyncMap(&sessions); got != baseSessions {
		t.Errorf("sessions leaked: %d entries after cleanup, baseline %d", got, baseSessions)
	}
	if got := countSyncMap(&clientGates); got != baseGates {
		t.Errorf("clientGates leaked: %d entries after cleanup, baseline %d", got, baseGates)
	}
}

// TestConcurrentClientPacketFuncSharedAddress hammers ONE gate from many goroutines.
// count is a per-session atomic (it used to be a global), so the total must be exact.
func TestConcurrentClientPacketFuncSharedAddress(t *testing.T) {
	addr := &net.UDPAddr{Port: 41011}
	g := storeClientGate(addr, newSpawnPrefixGate())
	defer clientGates.Delete(addr.String())

	var wg sync.WaitGroup
	for i := 0; i < concurrentSessionCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < concurrentIterations; j++ {
				// payload is NOT the 2-zero-bytes CreativeContent marker, so this only
				// exercises the counter path, not signalClientGate.
				clientPacketFunc(packet.Header{PacketID: packet.IDCreativeContent},
					[]byte{1, 2, 3}, addr, nil)
			}
		}()
	}
	wg.Wait()
	clientPacketFunc(packet.Header{PacketID: packet.IDSetLocalPlayerAsInitialised}, make([]byte, 9), addr, nil)

	want := int64(concurrentSessionCount*concurrentIterations + 1)
	if got := g.count.Load(); got != want {
		t.Errorf("shared-address counter = %d, want %d (lost increment under concurrency)", got, want)
	}
	if !g.saw113.Load() {
		t.Error("id=113 was not recorded on the shared gate")
	}
}

// TestConcurrentStaleSessionKeepsNewGate is the concurrency version of
// TestStaleSessionKeepsNewGate (log 13-33-24): the old session's deferred, identity-guarded
// Delete must not wipe the fresh session's gate, even when many old cleanups race at once.
func TestConcurrentStaleSessionKeepsNewGate(t *testing.T) {
	addr := &net.UDPAddr{Port: 41012}
	old := storeClientGate(addr, newSpawnPrefixGate())
	fresh := storeClientGate(addr, newSpawnPrefixGate())
	defer clientGates.Delete(addr.String())

	var wg sync.WaitGroup
	for i := 0; i < concurrentSessionCount*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cur, ok := clientGates.Load(addr.String()); ok && cur == old {
				clientGates.Delete(addr.String())
			}
		}()
	}
	wg.Wait()

	if cur, ok := clientGates.Load(addr.String()); !ok || cur != fresh {
		t.Fatalf("stale session deletes wiped the fresh gate: present=%v value=%v", ok, cur)
	}
	clientPacketFunc(packet.Header{PacketID: packet.IDSetLocalPlayerAsInitialised}, make([]byte, 9), addr, nil)
	if n := fresh.count.Load(); n != 1 {
		t.Errorf("fresh gate counter = %d, want 1", n)
	}
}
