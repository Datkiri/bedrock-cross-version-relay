package lansignal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// HostInfo is a version-tolerant decode of the ServerData carried in a
// discovery ResponsePacket.
//
// The stock discovery package only understands one ServerData version at a
// time, so a host running a different game version leaves the payload
// undecodable and the joiner learns nothing about the world. Minecraft 1.26.45
// advertises version 6 while the vendored go-nethernet decodes version 7, so
// every field - most importantly Nonce - was being dropped.
//
// Nonce is the field that decides whether a join is accepted at all: the host
// rejects any client whose ClientData.Nonce does not match the value it
// advertised with a Disconnect carrying reason NotAuthenticated (46).
type HostInfo struct {
	// Version is the ServerData layout version advertised by the host.
	Version uint8
	// ServerName is the player name of the host.
	ServerName string
	// Protocol is the Minecraft protocol version of the host, only present in
	// ServerData v7 and later. Zero when the host did not send it.
	Protocol int32
	// GameVersion is the game version string of the host, only present in
	// ServerData v7 and later. Empty when the host did not send it.
	GameVersion string
	// LevelName is the world name.
	LevelName string
	// GameType is the default game mode.
	GameType int32
	// PlayerCount and MaxPlayerCount are the occupancy of the world.
	PlayerCount    int32
	MaxPlayerCount int32
	// EditorWorld and Hardcore are world flags.
	EditorWorld bool
	Hardcore    bool
	// AcceptsOnlineAuth reports whether the host accepts Xbox Live
	// authenticated clients.
	AcceptsOnlineAuth bool
	// AcceptsSelfSignedAuth reports whether the host accepts clients that
	// authenticate with their own self-signed certificate.
	AcceptsSelfSignedAuth bool
	// AuthFlagsSeparate reports whether the two auth flags were encoded as
	// distinct booleans. ServerData v6 as sent by 1.26.45 carries a single
	// auth byte, in which case it is reported in both fields and this is false.
	AuthFlagsSeparate bool
	// Nonce must be echoed back in ClientData.Nonce on login.
	Nonce string
	// TransportLayer is 2 (NetherNet) in vanilla.
	TransportLayer int32
	// ConnectionType is 4 (LAN signalling) in vanilla.
	ConnectionType int32
}

// nonceOf returns the nonce of a HostInfo, or "" when it is nil. It exists so
// log statements can report the nonce without a nil check at every call site.
func nonceOf(i *HostInfo) string {
	if i == nil {
		return ""
	}
	return i.Nonce
}

// DecodeHostInfo decodes the ApplicationData of a ResponsePacket, trying every
// known ServerData layout and returning the first that consumes the payload
// exactly.
func DecodeHostInfo(data []byte) (*HostInfo, error) {
	if len(data) == 0 {
		return nil, errors.New("empty ServerData")
	}
	decoders := map[uint8]struct {
		name string
		fn   func(*bytes.Buffer) (*HostInfo, error)
	}{
		5: {"v5", decodeV5},
		6: {"v6", decodeV6},
		7: {"v7", decodeV7},
	}
	d, ok := decoders[data[0]]
	if !ok {
		return nil, fmt.Errorf("unknown ServerData version %d", data[0])
	}
	// Prefer the layout with separate auth flags when both parse cleanly, since
	// a single byte cannot distinguish online from self-signed auth.
	buf := bytes.NewBuffer(data)
	info, err := d.fn(buf)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", d.name, err)
	}
	if buf.Len() != 0 {
		return nil, fmt.Errorf("decode %s: %d unread bytes", d.name, buf.Len())
	}
	return info, nil
}

// decodeV7 decodes the go-nethernet v1.0.23 layout, which adds Protocol and
// GameVersion right after ServerName and keeps both auth flags.
func decodeV7(buf *bytes.Buffer) (*HostInfo, error) {
	i := &HostInfo{AuthFlagsSeparate: true}
	var err error
	if i.Version, err = buf.ReadByte(); err != nil {
		return nil, err
	}
	if i.ServerName, err = readStr(buf); err != nil {
		return nil, err
	}
	if i.Protocol, err = readVarint32(buf); err != nil {
		return nil, err
	}
	if i.GameVersion, err = readStr(buf); err != nil {
		return nil, err
	}
	if i.LevelName, err = readStr(buf); err != nil {
		return nil, err
	}
	if i.PlayerCount, err = readVarint32(buf); err != nil {
		return nil, err
	}
	if i.MaxPlayerCount, err = readVarint32(buf); err != nil {
		return nil, err
	}
	if i.GameType, err = readVarint32(buf); err != nil {
		return nil, err
	}
	if i.EditorWorld, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.Hardcore, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.AcceptsOnlineAuth, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.AcceptsSelfSignedAuth, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.Nonce, err = readStr(buf); err != nil {
		return nil, err
	}
	if i.ConnectionType, err = readVarint32(buf); err != nil {
		return nil, err
	}
	return i, nil
}

// decodeV6 decodes the layout a Minecraft 1.26.45 host sends, which matches
// go-nethernet v1.0.20: fixed width player counts and both auth flags.
func decodeV6(buf *bytes.Buffer) (*HostInfo, error) {
	i := &HostInfo{AuthFlagsSeparate: true}
	var err error
	if i.Version, err = buf.ReadByte(); err != nil {
		return nil, err
	}
	if i.ServerName, err = readStr(buf); err != nil {
		return nil, err
	}
	if i.LevelName, err = readStr(buf); err != nil {
		return nil, err
	}
	if i.GameType, err = readVarint32(buf); err != nil {
		return nil, err
	}
	if i.PlayerCount, err = readFixedInt32(buf); err != nil {
		return nil, err
	}
	if i.MaxPlayerCount, err = readFixedInt32(buf); err != nil {
		return nil, err
	}
	if i.EditorWorld, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.Hardcore, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.AcceptsOnlineAuth, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.AcceptsSelfSignedAuth, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.Nonce, err = readStr(buf); err != nil {
		return nil, err
	}
	if i.TransportLayer, err = readVarint32(buf); err != nil {
		return nil, err
	}
	if i.ConnectionType, err = readVarint32(buf); err != nil {
		return nil, err
	}
	return i, nil
}

// decodeV5 decodes the older ServerData layout used by some hosts (e.g. BDS 1.26.33 /
// protocol 1001). It predates the Protocol, GameVersion and Nonce fields. Player counts are
// fixed-width int32 (same as v6), and the two auth flags are SEPARATE booleans — measured against
// a real BDS 1.26.33 broadcast, the payload consumes exactly 47 bytes:
//
//	Version(1) ServerName(str) LevelName(str) GameType(varint32)
//	PlayerCount(fixed i32 LE) MaxPlayerCount(fixed i32 LE)
//	EditorWorld(bool) Hardcore(bool) AcceptsOnlineAuth(bool) AcceptsSelfSignedAuth(bool)
//	TransportLayer(varint32 zigzag) ConnectionType(varint32 zigzag)
//
// A sample decode: ServerName="Dedicated Server", LevelName="Bedrock level", GameType=0,
// PlayerCount=0, MaxPlayerCount=10, Hardcore=true, online auth=true, TransportLayer=2
// (NetherNet), ConnectionType=4 (LAN), Nonce="" — v5 has NO nonce, so the joiner must not
// require one for version < 6.
func decodeV5(buf *bytes.Buffer) (*HostInfo, error) {
	i := &HostInfo{AuthFlagsSeparate: true}
	var err error
	if i.Version, err = buf.ReadByte(); err != nil {
		return nil, err
	}
	if i.ServerName, err = readStr(buf); err != nil {
		return nil, err
	}
	if i.LevelName, err = readStr(buf); err != nil {
		return nil, err
	}
	if i.GameType, err = readVarint32(buf); err != nil {
		return nil, err
	}
	if i.PlayerCount, err = readFixedInt32(buf); err != nil {
		return nil, err
	}
	if i.MaxPlayerCount, err = readFixedInt32(buf); err != nil {
		return nil, err
	}
	if i.EditorWorld, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.Hardcore, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.AcceptsOnlineAuth, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.AcceptsSelfSignedAuth, err = readBool(buf); err != nil {
		return nil, err
	}
	if i.TransportLayer, err = readVarint32(buf); err != nil {
		return nil, err
	}
	if i.ConnectionType, err = readVarint32(buf); err != nil {
		return nil, err
	}
	return i, nil
}

func readBool(buf *bytes.Buffer) (bool, error) {
	b, err := buf.ReadByte()
	return b != 0, err
}

func readStr(buf *bytes.Buffer) (string, error) {
	length, err := readVaruint32(buf)
	if err != nil {
		return "", err
	}
	if int(length) > buf.Len() {
		return "", fmt.Errorf("string length %d exceeds remaining %d bytes", length, buf.Len())
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(buf, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func readFixedInt32(buf *bytes.Buffer) (int32, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(buf, b); err != nil {
		return 0, err
	}
	return int32(binary.LittleEndian.Uint32(b)), nil
}

func readVarint32(r io.ByteReader) (int32, error) {
	u, err := readVaruint32(r)
	if err != nil {
		return 0, err
	}
	v := int32(u >> 1)
	if u&1 != 0 {
		v = ^v
	}
	return v, nil
}

func readVaruint32(r io.ByteReader) (uint32, error) {
	var v uint32
	for i := uint(0); i < 35; i += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7f) << i
		if b&0x80 == 0 {
			return v, nil
		}
	}
	return 0, errors.New("varuint32 did not terminate after 5 bytes")
}
