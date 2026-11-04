package main

import (
	"testing"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// TestProtocolForVersion is the table-driven oracle for PROTO-001. Every version the README
// advertises must resolve to the protocol the codec actually implements; unknown versions must
// return 0 so verifyHostProtocol declines to draw a conclusion instead of rejecting a good host.
func TestProtocolForVersion(t *testing.T) {
	cases := []struct {
		version string
		want    int32
	}{
		{"1.26.30", 1001}, {"1.26.31", 1001}, {"1.26.32", 1001}, {"1.26.33", 1001},
		{"1.26.34", 1001}, {"1.26.35", 1001}, {"1.26.36", 1001},
		{"1.26.40", 2168}, {"1.26.41", 2168}, {"1.26.42", 2168}, {"1.26.43", 2168}, {"1.26.44", 2168},
		{"1.26.45", 2169},
		{"1.26.50", 2193}, {"1.26.51", 2193}, {"1.26.52", 2193},
		{"1.26.46", 0}, {"1.26.49", 0}, {"1.27.0", 0}, {"", 0}, {"*", 0}, {"not-a-version", 0},
	}
	for _, c := range cases {
		if got := protocolForVersion(c.version); got != c.want {
			t.Errorf("protocolForVersion(%q) = %d, want %d", c.version, got, c.want)
		}
	}

	// Every mapped protocol must have a codec; a table entry pointing at a protocol the relay
	// cannot serve would let verifyHostProtocol accept a session that cannot decode the wire.
	for _, id := range []int32{1001, 2168, 2169, 2193} {
		p, ok := resolveProto(id)
		if !ok {
			t.Errorf("resolveProto(%d) has no codec", id)
			continue
		}
		if p.ID() != id {
			t.Errorf("resolveProto(%d).ID() = %d", id, p.ID())
		}
	}
}

// TestVerifyHostProtocol checks the "mismatched host" case: a host that accepted a foreign
// protocol still declares its real version in StartGame, and the relay must notice.
func TestVerifyHostProtocol(t *testing.T) {
	codec := minecraft.DefaultProtocol // 2193, the native StartGame layout

	encode := func(version string) *spawnCapture {
		t.Helper()
		sg := &packet.StartGame{
			EntityUniqueID: 1, EntityRuntimeID: 1, WorldName: "version-probe",
			GameVersion: version, BaseGameVersion: "*",
		}
		raw, err := matrixEncode(codec, sg)
		if err != nil {
			t.Fatalf("encode StartGame(%q): %v", version, err)
		}
		decoded, err := decodeRawStartGameWith(codec, raw)
		if err != nil {
			t.Fatalf("decode StartGame(%q): %v", version, err)
		}
		if decoded.GameVersion != version {
			t.Fatalf("StartGame version changed on the wire: %q -> %q", version, decoded.GameVersion)
		}
		cap := &spawnCapture{}
		cap.startGame.Store(&raw)
		return cap
	}

	cases := []struct {
		name    string
		cap     *spawnCapture
		pid     int32
		wantVer string
		wantOK  bool
	}{
		{"matching 2193", encode("1.26.51"), 2193, "1.26.51", true},
		{"matching 1.26.52", encode("1.26.52"), 2193, "1.26.52", true},
		{"host is really 1001", encode("1.26.33"), 2193, "1.26.33", false},
		{"host is really 2168", encode("1.26.44"), 2193, "1.26.44", false},
		{"host is really 2169", encode("1.26.45"), 2193, "1.26.45", false},
		{"unknown version", encode("1.26.46"), 2193, "1.26.46", true},
		{"wildcard version", encode("*"), 2193, "*", true},
		{"empty version", encode(""), 2193, "", true},
		{"no StartGame captured", &spawnCapture{}, 2193, "", true},
		{"no codec for pid", encode("1.26.51"), 1234, "", false},
	}
	for _, c := range cases {
		gotVer, gotOK := verifyHostProtocol(c.cap, c.pid)
		if gotVer != c.wantVer || gotOK != c.wantOK {
			t.Errorf("%s: verifyHostProtocol = (%q, %v), want (%q, %v)",
				c.name, gotVer, gotOK, c.wantVer, c.wantOK)
		} else {
			t.Logf("%s: (%q, %v)", c.name, gotVer, gotOK)
		}
	}
}

// TestResolveHostProto pins the auto/forced/invalid behaviour of -host-proto.
func TestResolveHostProto(t *testing.T) {
	cases := []struct {
		flag      string
		announced int32
		wantID    int32
		wantKnown bool
	}{
		{"auto", 2169, 2169, true},
		{"auto", 0, 2193, false},
		{"auto", 99999, 2193, false},
		{"auto", -1, 2193, false},
		{"1001", 2169, 1001, true},
		{"2168", 0, 2168, true},
		{"garbage", 2169, 2193, false},
		{"7777", 0, 2193, false},
	}
	for _, c := range cases {
		id, known := resolveHostProto(c.flag, c.announced)
		if id != c.wantID || known != c.wantKnown {
			t.Errorf("resolveHostProto(%q, %d) = (%d, %v), want (%d, %v)",
				c.flag, c.announced, id, known, c.wantID, c.wantKnown)
		}
	}
}
