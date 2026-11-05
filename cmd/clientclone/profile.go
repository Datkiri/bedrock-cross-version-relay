package main

// Terminal-client profile. This is deliberately NOT a copy of Cinnabar's
// profile.go (PolyForm Noncommercial). The field list and the "a real client
// never sends zero here" rule come from the measured behavior documented in
// reverse-engineering/TPBOT-CLIENT-PROFILE.md; the skin is generated here.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"strings"

	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

const skinWidth, skinHeight = 64, 64

func uuidFromName(name, salt string) uuid.UUID {
	sum := sha256.Sum256([]byte("clientclone/" + salt + "/" + name))
	var id uuid.UUID
	copy(id[:], sum[:16])
	return id
}

// deterministicID derives stable device/skin identifiers from the player name.
// A real client keeps its device across sessions; changing it on every join is
// itself a bot signal.
func deterministicID(name, salt string) string {
	return strings.ToLower(strings.ReplaceAll(uuidFromName(name, salt).String(), "-", ""))
}

func deterministicInt64(name, salt string) int64 {
	sum := sha256.Sum256([]byte("clientclone/int/" + salt + "/" + name))
	v := int64(binary.BigEndian.Uint64(sum[:8]))
	if v == 0 {
		return 1
	}
	return v
}

// makeSkin builds a 64x64 RGBA classic skin. The only hard requirement is that
// the byte length matches SkinImageWidth*SkinImageHeight*4 (login.Validate),
// but a solid-black skin is exactly what a stock headless Dialer sends and is
// an obvious bot marker. Fill head/body/legs with non-black colours instead.
func makeSkin(name string) []byte {
	rgba := make([]byte, skinWidth*skinHeight*4)
	set := func(x, y int, r, g, b, a byte) {
		i := (y*skinWidth + x) * 4
		rgba[i], rgba[i+1], rgba[i+2], rgba[i+3] = r, g, b, a
	}
	// Base: skin tone everywhere; overlay regions stay fully opaque too, which
	// is fine for a classic (non-persona) skin.
	for y := 0; y < skinHeight; y++ {
		for x := 0; x < skinWidth; x++ {
			set(x, y, 0xb8, 0x86, 0x6b, 0xff)
		}
	}
	// Hair / hat region (top-left 32x16 of the head UV area).
	for y := 0; y < 16; y++ {
		for x := 0; x < 32; x++ {
			set(x, y, 0x3b, 0x28, 0x1c, 0xff)
		}
	}
	// Torso: shirt colour.
	for y := 16; y < 32; y++ {
		for x := 0; x < 40; x++ {
			set(x, y, 0x2f, 0x6f, 0xa8, 0xff)
		}
	}
	// Legs: trousers.
	for y := 32; y < 48; y++ {
		for x := 0; x < 40; x++ {
			set(x, y, 0x33, 0x3a, 0x55, 0xff)
		}
	}
	// Eyes on the face (a solid colour block would look like a faceless mob).
	set(10, 8, 0x25, 0x25, 0x25, 0xff)
	set(11, 8, 0xff, 0xff, 0xff, 0xff)
	set(20, 8, 0x25, 0x25, 0x25, 0xff)
	set(21, 8, 0xff, 0xff, 0xff, 0xff)
	return rgba
}

// clientData returns the login profile of a real-looking Android client.
// minecraft.Dialer.defaultClientData() fills the remaining fields (ServerAddress,
// SkinResourcePatch, SkinGeometry, ...) so the JWT chain stays valid.
func clientData(name string) login.ClientData {
	skin := makeSkin(name)
	skinB64 := base64.StdEncoding.EncodeToString(skin)
	hash := sha256.Sum256(skin)

	return login.ClientData{
		DeviceModel:                      "WorkBuddy ClientClone",
		DeviceOS:                         protocol.DeviceAndroid,
		DeviceID:                         login.DeviceID(deterministicID(name, "device")),
		ClientRandomID:                   deterministicInt64(name, "random"),
		SelfSignedID:                     uuidFromName(name, "ssid").String(),
		LanguageCode:                     "en_US",
		CurrentInputMode:                 packet.InputModeTouch,
		DefaultInputMode:                 packet.InputModeTouch,
		UIProfile:                        1, // Pocket
		GUIScale:                         0,
		MaxViewDistance:                  16,
		MemoryTier:                       3,
		GraphicsMode:                     1, // Fancy
		PlatformType:                     1,
		CompatibleWithClientSideChunkGen: true,
		SkinID:                           deterministicID(name, "skin"),
		SkinData:                         skinB64,
		SkinImageWidth:                   skinWidth,
		SkinImageHeight:                  skinHeight,
		SkinColour:                       "#b8866b",
		ArmSize:                          "wide",
		SkinResourcePatch:                "", // filled by Dialer default
		ProfileHash:                      base64.StdEncoding.EncodeToString(hash[:]),
		TrustedSkin:                      true,
		PersonaSkin:                      false,
		OverrideSkin:                     false,
		CapeData:                         "",
		// GameVersion is intentionally left empty: setAndroidData() overwrites it
		// with protocol.CurrentVersion anyway, so putting a historical version
		// here would be misleading. The codec's Ver() is what actually decides
		// the wire protocol during login.
	}
}
