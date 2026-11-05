package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestCodecForAllProtocols(t *testing.T) {
	for _, id := range []int32{1001, 2168, 2169, 2193} {
		codec, err := codecFor(id)
		if err != nil {
			t.Fatalf("codecFor(%d): %v", id, err)
		}
		if codec == nil || codec.Ver() == "" {
			t.Fatalf("codecFor(%d) returned an unusable codec", id)
		}
	}
	if _, err := codecFor(9999); err == nil {
		t.Fatal("codecFor(9999) must fail")
	}
}

func TestClientDataProfileIsNotStockZeros(t *testing.T) {
	d := clientData("Tester")
	checks := map[string]any{
		"DeviceModel":                      d.DeviceModel,
		"DeviceID":                         string(d.DeviceID),
		"SelfSignedID":                     d.SelfSignedID,
		"SkinID":                           d.SkinID,
		"LanguageCode":                     d.LanguageCode,
		"SkinColour":                       d.SkinColour,
		"ArmSize":                          d.ArmSize,
		"ProfileHash":                      d.ProfileHash,
		"ClientRandomID":                   d.ClientRandomID,
		"UIProfile":                        d.UIProfile,
		"MaxViewDistance":                  d.MaxViewDistance,
		"MemoryTier":                       d.MemoryTier,
		"GraphicsMode":                     d.GraphicsMode,
		"CompatibleWithClientSideChunkGen": d.CompatibleWithClientSideChunkGen,
		"TrustedSkin":                      d.TrustedSkin,
	}
	for name, v := range checks {
		switch x := v.(type) {
		case string:
			if x == "" {
				t.Errorf("%s is empty (a stock headless Dialer leaves it at zero)", name)
			}
		case int:
			if x == 0 {
				t.Errorf("%s is 0", name)
			}
		case int64:
			if x == 0 {
				t.Errorf("%s is 0", name)
			}
		case bool:
			if !x {
				t.Errorf("%s is false", name)
			}
		}
	}
	if d.DeviceModel != "WorkBuddy ClientClone" {
		t.Fatalf("DeviceModel=%q", d.DeviceModel)
	}
	skin, err := base64.StdEncoding.DecodeString(d.SkinData)
	if err != nil {
		t.Fatalf("SkinData is not base64: %v", err)
	}
	if want := skinWidth * skinHeight * 4; len(skin) != want {
		t.Fatalf("SkinData decoded to %d bytes, want %d", len(skin), want)
	}
	// A stock Dialer sends an all-black (0,0,0,255) skin. Ours must not.
	nonBlack := false
	for i := 0; i < len(skin); i += 4 {
		if skin[i] != 0 || skin[i+1] != 0 || skin[i+2] != 0 {
			nonBlack = true
			break
		}
	}
	if !nonBlack {
		t.Fatal("SkinData is a solid black skin")
	}
}

func TestClientDataIdentifiersAreStable(t *testing.T) {
	a, b := clientData("SameName"), clientData("SameName")
	if a.DeviceID != b.DeviceID || a.SelfSignedID != b.SelfSignedID ||
		a.SkinID != b.SkinID || a.ClientRandomID != b.ClientRandomID {
		t.Fatal("the same player name must produce the same device/skin identity")
	}
	c := clientData("OtherName")
	if a.DeviceID == c.DeviceID || a.SkinID == c.SkinID {
		t.Fatal("different player names must not share a device identity")
	}
}

func TestCommandParsingAndDispatch(t *testing.T) {
	s := newSession(config{proto: 1001, name: "Tester", spawnTimeout: 1})

	for _, line := range []string{
		`{"cmd":"join","addr":"127.0.0.1:19164","proto":1001}`,
		`{"cmd":"wait","ms":250}`,
		`{"cmd":"move","x":1,"y":64,"z":-3}`,
		`{"cmd":"look","yaw":1.5,"pitch":-0.25}`,
		`{"cmd":"sneak","on":true}`,
		`{"cmd":"chat","message":"hi"}`,
		`{"cmd":"hotbar","slot":4}`,
		`{"cmd":"assert","metric":"packet_LevelChunk","op":">","value":0}`,
	} {
		var c command
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		if c.Cmd == "" {
			t.Fatalf("parse %s: empty cmd", line)
		}
	}

	// Commands that work without a live connection.
	if res := s.exec(command{Cmd: "dump"}); !res.OK || len(res.Snapshot) == 0 {
		t.Fatalf("dump: ok=%t snapshot=%s", res.OK, res.Snapshot)
	}
	if res := s.exec(command{Cmd: "assert", Metric: "joined", Op: "==", Value: false}); !res.OK {
		t.Fatalf("assert joined==false failed: %s", res.Error)
	}
	if res := s.exec(command{Cmd: "quit"}); !res.OK || !res.Quit {
		t.Fatalf("quit: ok=%t quit=%t", res.OK, res.Quit)
	}
	if res := s.exec(command{Cmd: "join"}); res.OK {
		t.Fatal("join without an addr must fail")
	}
	if res := s.exec(command{Cmd: "chat", Message: "hi"}); res.OK {
		t.Fatal("chat before join must fail")
	}
	if res := s.exec(command{Cmd: "nope"}); res.OK {
		t.Fatal("unknown command must fail")
	}
	if res := s.exec(command{Cmd: "assert", Metric: "not_a_metric", Op: "==", Value: 1}); res.OK {
		t.Fatal("assert on an unknown metric must fail")
	}
}

func TestCompareOperators(t *testing.T) {
	cases := []struct {
		actual, want any
		op           string
		pass         bool
	}{
		{float64(5), float64(3), ">", true},
		{float64(2), float64(3), ">", false},
		{uint64(3), float64(3), ">=", true},
		{uint64(0), float64(0), "==", true},
		{true, true, "==", true},
		{false, false, "!=", false},
		{"host_disconnect: x", "host_disconnect", "contains", true},
	}
	for i, c := range cases {
		pass, _ := compare(c.actual, c.op, c.want)
		if pass != c.pass {
			t.Errorf("case %d: compare(%v, %q, %v) = %t, want %t", i, c.actual, c.op, c.want, pass, c.pass)
		}
	}
}

func TestDimRangeAndChunkColumn(t *testing.T) {
	minY, height := dimRange(0)
	first, last := minY>>4, (minY+height-1)>>4
	if first != -4 || last != 19 {
		t.Fatalf("overworld column = [%d,%d], want [-4,19]", first, last)
	}
	// Offsets must fit int8 (SubChunkOffset is [3]int8).
	if last-first+1 > 127 {
		t.Fatalf("column has %d sub-chunks, offsets would overflow int8", last-first+1)
	}
}

func TestInputPacketIsSafeForEveryProtocol(t *testing.T) {
	s := newSession(config{proto: 1001})
	pk := s.inputPacket()
	if pk.InputMode != packet.InputModeTouch {
		t.Fatalf("InputMode=%d, want touch", pk.InputMode)
	}
	if !pk.InputData.Present() {
		t.Fatal("InputData must be present (a zero-length flag list is valid wire)")
	}
	for i := 0; i < packet.InputFlagCount; i++ {
		if pk.InputData.Load(i) {
			t.Fatalf("flag %d is set by default: newer flag IDs can trip UnknownEnumOption on an older server", i)
		}
	}
	if pk.Tick != 1 {
		t.Fatalf("first tick = %d, want 1", pk.Tick)
	}
	// Two consecutive ticks must be monotonically increasing.
	if s.inputPacket().Tick != 2 {
		t.Fatal("tick did not advance")
	}
}

func TestPacketName(t *testing.T) {
	if got := packetName(&packet.LevelChunk{}); got != "LevelChunk" {
		t.Fatalf("packetName = %q", got)
	}
	if got := packetName(&packet.NetworkStackLatency{}); !strings.Contains(got, "NetworkStackLatency") {
		t.Fatalf("packetName = %q", got)
	}
}
