package lansignal

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func putVaruint(b *bytes.Buffer, v uint32) {
	for v >= 0x80 {
		b.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	b.WriteByte(byte(v))
}

func putVarint(b *bytes.Buffer, v int32) {
	putVaruint(b, uint32(v<<1)^uint32(v>>31))
}

func putStr(b *bytes.Buffer, s string) {
	putVaruint(b, uint32(len(s)))
	b.WriteString(s)
}

func putFixed(b *bytes.Buffer, v int32) {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], uint32(v))
	b.Write(tmp[:])
}

func fixtureV5() []byte {
	b := &bytes.Buffer{}
	b.WriteByte(5)
	putStr(b, "Dedicated Server")
	putStr(b, "Bedrock level")
	putVarint(b, 0)  // GameType
	putFixed(b, 0)   // PlayerCount
	putFixed(b, 10)  // MaxPlayerCount
	b.WriteByte(0)   // EditorWorld
	b.WriteByte(1)   // Hardcore
	b.WriteByte(1)   // AcceptsOnlineAuth
	b.WriteByte(0)   // AcceptsSelfSignedAuth
	putVarint(b, 2)  // TransportLayer
	putVarint(b, 4)  // ConnectionType
	return b.Bytes()
}

func fixtureV6() []byte {
	b := &bytes.Buffer{}
	b.WriteByte(6)
	putStr(b, "Host Six")
	putStr(b, "World Six")
	putVarint(b, 1)  // GameType
	putFixed(b, 2)   // PlayerCount
	putFixed(b, 8)   // MaxPlayerCount
	b.WriteByte(0)   // EditorWorld
	b.WriteByte(1)   // Hardcore
	b.WriteByte(1)   // AcceptsOnlineAuth
	b.WriteByte(1)   // AcceptsSelfSignedAuth
	putStr(b, "nonce-six")
	putVarint(b, 2)  // TransportLayer
	putVarint(b, 4)  // ConnectionType
	return b.Bytes()
}

func fixtureV7() []byte {
	b := &bytes.Buffer{}
	b.WriteByte(7)
	putStr(b, "Host Seven")
	putVarint(b, 2193) // Protocol
	putStr(b, "1.26.51")
	putStr(b, "World Seven")
	putVarint(b, 3)  // PlayerCount
	putVarint(b, 10) // MaxPlayerCount
	putVarint(b, 0)  // GameType
	b.WriteByte(0)   // EditorWorld
	b.WriteByte(1)   // Hardcore
	b.WriteByte(1)   // AcceptsOnlineAuth
	b.WriteByte(0)   // AcceptsSelfSignedAuth
	putStr(b, "nonce-seven")
	putVarint(b, 4)  // ConnectionType
	return b.Bytes()
}

// TestDecodeHostInfoValid pins the three ServerData layouts the relay claims to understand,
// including the version-dependent fields (Protocol/GameVersion in v7, no Nonce in v5).
func TestDecodeHostInfoValid(t *testing.T) {
	cases := []struct {
		name    string
		raw     []byte
		want    HostInfo
		wantLen int
	}{
		{
			name: "v5 BDS",
			raw:  fixtureV5(),
			want: HostInfo{
				Version: 5, ServerName: "Dedicated Server", LevelName: "Bedrock level",
				MaxPlayerCount: 10, Hardcore: true, AcceptsOnlineAuth: true,
				AuthFlagsSeparate: true, TransportLayer: 2, ConnectionType: 4,
			},
			wantLen: 47,
		},
		{
			name: "v6",
			raw:  fixtureV6(),
			want: HostInfo{
				Version: 6, ServerName: "Host Six", LevelName: "World Six", GameType: 1,
				PlayerCount: 2, MaxPlayerCount: 8, Hardcore: true,
				AcceptsOnlineAuth: true, AcceptsSelfSignedAuth: true,
				AuthFlagsSeparate: true, Nonce: "nonce-six", TransportLayer: 2, ConnectionType: 4,
			},
		},
		{
			name: "v7",
			raw:  fixtureV7(),
			want: HostInfo{
				Version: 7, ServerName: "Host Seven", Protocol: 2193, GameVersion: "1.26.51",
				LevelName: "World Seven", PlayerCount: 3, MaxPlayerCount: 10, Hardcore: true,
				AcceptsOnlineAuth: true, AuthFlagsSeparate: true, Nonce: "nonce-seven",
				ConnectionType: 4,
			},
		},
	}
	for _, c := range cases {
		if c.wantLen != 0 && len(c.raw) != c.wantLen {
			t.Errorf("%s fixture is %d bytes, want %d", c.name, len(c.raw), c.wantLen)
		}
		got, err := DecodeHostInfo(c.raw)
		if err != nil {
			t.Fatalf("%s: DecodeHostInfo: %v", c.name, err)
		}
		if *got != c.want {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, *got, c.want)
		}
		t.Logf("%s: %d bytes, nonce=%q proto=%d ver=%q", c.name, len(c.raw), got.Nonce, got.Protocol, got.GameVersion)
	}
}

// TestDecodeHostInfoMalformed is CONN-003's malformed-parser requirement: no malformed
// ServerData may panic or silently produce a half-filled HostInfo that the joiner would trust.
func TestDecodeHostInfoMalformed(t *testing.T) {
	valid := fixtureV7()
	cases := []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"unknown v4", []byte{4, 0}},
		{"unknown v8", []byte{8, 0}},
		{"unknown v255", []byte{255, 0}},
		{"version byte only", []byte{7}},
		{"truncated v7", valid[:len(valid)/2]},
		{"truncated v6", fixtureV6()[:5]},
		{"trailing bytes", append(append([]byte{}, valid...), 0xde, 0xad)},
		{"string length overruns", append([]byte{7}, 0x40, 'x')},
		{"unterminated varuint", []byte{7, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80}},
	}
	for _, c := range cases {
		got, err := DecodeHostInfo(c.raw)
		if err == nil {
			t.Errorf("%s: expected an error, got %+v", c.name, got)
			continue
		}
		if got != nil {
			t.Errorf("%s: returned a non-nil HostInfo together with an error", c.name)
		}
		t.Logf("%s: %v", c.name, err)
	}
}

// TestNonceOf guards the nil case used all over the dial path's logging.
func TestNonceOf(t *testing.T) {
	if got := nonceOf(nil); got != "" {
		t.Errorf("nonceOf(nil) = %q", got)
	}
	i := &HostInfo{Nonce: "abc"}
	if got := nonceOf(i); got != "abc" {
		t.Errorf("nonceOf = %q", got)
	}
}
