package main

import (
	"bytes"
	"bedrock-cross-version-relay/internal/blocktranslate"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

type matrixResult struct {
	Client   int32    `json:"client_protocol"`
	Host     int32    `json:"host_protocol"`
	Checks   int      `json:"checks"`
	Findings []string `json:"findings"`
}

// moduleRoot walks up from the test's working directory until it finds go.mod, so report paths
// written by the test are stable no matter which directory the toolchain chose to run in.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// Diagnostic matrix, opt-in because known failures are useful regression cases
// rather than a claim that all protocol pairs already support gameplay.
// XVER_MATRIX_REPORT is a caller-selected JSON report path.
func TestProtocolCompatibilityMatrix(t *testing.T) {
	path := os.Getenv("XVER_MATRIX_REPORT")
	if path == "" {
		t.Skip("set XVER_MATRIX_REPORT to run the diagnostic 4x4 matrix")
	}
	// `go test` runs with the working directory set to this package, so a relative report path
	// such as dist/matrix.json would land in cmd/xvrelay/dist/. Anchor it to the module root.
	if !filepath.IsAbs(path) {
		path = filepath.Join(moduleRoot(t), path)
	}
	ids := []int32{1001, 2168, 2169, 2193}
	var results []matrixResult
	for _, clientID := range ids {
		conn := negotiatedConn(t, clientID)
		clientCodec, _ := resolveProto(clientID)
		for _, hostID := range ids {
			hostCodec, _ := resolveProto(hostID)
			result := matrixResult{Client: clientID, Host: hostID, Findings: []string{}}
			cap := &spawnCapture{}
			sess := &session{hostProto: hostID, capture: cap}
			if err := configureBlockTranslation(sess, clientID, minecraft.GameData{UseBlockNetworkIDHashes: true}); err != nil {
				t.Fatal(err)
			}
			sessions.Store(conn, sess)
			check := func(name string, run func() error) {
				result.Checks++
				if err := matrixCatch(run); err != nil {
					result.Findings = append(result.Findings, name+": "+err.Error())
				}
			}
			check("StartGame metadata", func() error {
				sg := &packet.StartGame{EntityUniqueID: 42, EntityRuntimeID: 42, WorldName: "matrix world", LevelID: "matrix-level", ServerID: "matrix-server", ServerChunkTickRadius: 6, GameVersion: hostCodec.Ver(), BaseGameVersion: "*", PropertyData: map[string]any{}, UseBlockNetworkIDHashes: true, Generator: 2}
				sg.GameRules = []protocol.GameRule{{Name: "dodaylightcycle", CanBeModifiedByPlayer: true, Value: true}, {Name: "randomtickspeed", Value: uint32(3)}}
				sg.PlayerPermissions = 2
				sg.EducationEditionOffer = 1
				sg.Experiments = []protocol.ExperimentData{{Name: "gametest", Enabled: true}}
				raw, err := matrixEncode(hostCodec, sg)
				if err != nil {
					return err
				}
				cap.startGame.Store(&raw)
				generated := &packet.StartGame{EntityUniqueID: 42, EntityRuntimeID: 42, WorldName: sg.WorldName, GameVersion: clientCodec.Ver(), PropertyData: map[string]any{}, UseBlockNetworkIDHashes: true}
				got, err := matrixTarget(conn, clientCodec, generated)
				if err != nil {
					return err
				}
				x := got.(*packet.StartGame)
				if x.LevelID != sg.LevelID || x.ServerChunkTickRadius != 6 || x.ServerID != sg.ServerID {
					return fmt.Errorf("world metadata lost: level=%q server=%q tickRadius=%d", x.LevelID, x.ServerID, x.ServerChunkTickRadius)
				}
				if !reflect.DeepEqual(x.GameRules, sg.GameRules) || !reflect.DeepEqual(x.Experiments, sg.Experiments) || x.PlayerPermissions != sg.PlayerPermissions || x.EducationEditionOffer != sg.EducationEditionOffer {
					return fmt.Errorf("world rules/experiments/permissions changed")
				}
				return nil
			})
			for _, request := range []bool{false, true} {
				name := "LevelChunk inline"
				if request {
					name = "LevelChunk request-mode"
				}
				check(name, func() error {
					pk := &packet.LevelChunk{Position: protocol.ChunkPos{2, 3}, SubChunkCount: 1, RawPayload: []byte{9, 0, 0}}
					if request {
						pk.SubChunkCount = 0
						pk.SubChunkLimit = protocol.Option(int32(9))
					}
					incoming, err := matrixSource(hostCodec, pk, false)
					if err != nil {
						return err
					}
					got, err := matrixTarget(conn, clientCodec, incoming)
					if err != nil {
						return err
					}
					x := got.(*packet.LevelChunk)
					if !bytes.Equal(x.RawPayload, pk.RawPayload) {
						return fmt.Errorf("payload %d -> %d bytes", len(pk.RawPayload), len(x.RawPayload))
					}
					if request {
						if limit, ok := x.SubChunkLimit.Value(); !ok || limit != 9 {
							return fmt.Errorf("request limit changed: %v", x.SubChunkLimit)
						}
					}
					return nil
				})
			}
			check("SubChunk payload", func() error {
				pk := &packet.SubChunk{Position: protocol.SubChunkPos{2, 0, 3}, SubChunkEntries: []protocol.SubChunkEntry{{Offset: protocol.SubChunkOffset{0, -4, 0}, Result: protocol.SubChunkResultSuccess, RawPayload: protocol.Option([]byte{9, 0, 0})}}}
				incoming, err := matrixSource(hostCodec, pk, false)
				if err != nil {
					return err
				}
				got, err := matrixTarget(conn, clientCodec, incoming)
				if err != nil {
					return err
				}
				x := got.(*packet.SubChunk)
				if len(x.SubChunkEntries) != 1 {
					return fmt.Errorf("entry count=%d", len(x.SubChunkEntries))
				}
				payload, _ := x.SubChunkEntries[0].RawPayload.Value()
				if !bytes.Equal(payload, []byte{9, 0, 0}) || x.SubChunkEntries[0].Offset[1] != -4 {
					return fmt.Errorf("subchunk payload/offset changed")
				}
				return nil
			})
			check("host chat", func() error {
				pk := &packet.Text{TextType: packet.TextTypeChat, SourceName: "MatrixHost", Message: "matrix chat"}
				incoming, err := matrixSource(hostCodec, pk, false)
				if err != nil {
					return err
				}
				got, err := matrixTarget(conn, clientCodec, incoming)
				if err != nil {
					return err
				}
				x := got.(*packet.Text)
				if x.Message != pk.Message || x.SourceName != pk.SourceName {
					return fmt.Errorf("chat fields changed")
				}
				return nil
			})
			check("inventory envelope", func() error {
				pk := &packet.InventoryContent{WindowID: 0, Content: []protocol.ItemInstance{{Stack: protocol.ItemStack{ItemType: protocol.ItemType{NetworkID: 1}, Count: 1}}}}
				incoming, err := matrixSource(hostCodec, pk, false)
				if err != nil {
					return err
				}
				got, err := matrixTarget(conn, clientCodec, incoming)
				if err != nil {
					return err
				}
				x := got.(*packet.InventoryContent)
				if len(x.Content) != 1 || x.Content[0].Stack.NetworkID != 1 || x.Content[0].Stack.Count != 1 {
					return fmt.Errorf("inventory ID/count changed")
				}
				return nil
			})
			check("client SubChunkRequest", func() error {
				pk := &packet.SubChunkRequest{Position: protocol.SubChunkPos{2, 0, 3}, Offsets: []protocol.SubChunkOffset{{0, -4, 0}, {0, 1, 0}}}
				incoming, err := matrixSource(clientCodec, pk, true)
				if err != nil {
					return err
				}
				got, err := matrixSource(hostCodec, incoming, true)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got, pk) {
					return fmt.Errorf("request fields changed")
				}
				return nil
			})
			check("block update codec-relay-codec", func() error {
				if err := configureBlockTranslation(sess, clientID, minecraft.GameData{}); err != nil {
					return err
				}
				source, _ := blocktranslate.New(hostID, hostID, false, nil)
				target, _ := blocktranslate.New(clientID, clientID, false, nil)
				id, ok := source.SourceID("minecraft:stone", nil)
				if !ok {
					return fmt.Errorf("source stone missing")
				}
				want, ok := target.SourceID("minecraft:stone", nil)
				if !ok {
					return fmt.Errorf("target stone missing")
				}
				incoming, err := matrixSource(hostCodec, &packet.UpdateBlock{NewBlockRuntimeID: id}, false)
				if err != nil {
					return err
				}
				got, err := matrixTarget(conn, clientCodec, incoming)
				if err != nil {
					return err
				}
				if got.(*packet.UpdateBlock).NewBlockRuntimeID != want {
					return fmt.Errorf("block runtime ID got%d want%d", got.(*packet.UpdateBlock).NewBlockRuntimeID, want)
				}
				return nil
			})
			sessions.Delete(conn)
			results = append(results, result)
			t.Logf("MATRIX client=%d host=%d checks=%d findings=%v", clientID, hostID, result.Checks, result.Findings)
		}
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// The report path is usually dist/matrix.json, and dist/ is gitignored ⇒ absent in a fresh
	// clone. os.WriteFile does not create parents, so `make matrix` used to fail there.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err = os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create report directory %s: %v", dir, err)
		}
	}
	if err = os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if len(r.Findings) > 0 {
			t.Errorf("client%d -> host%d: %v", r.Client, r.Host, r.Findings)
		}
	}
}

func matrixCatch(run func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return run()
}

func matrixEncode(codec minecraft.Protocol, pk packet.Packet) ([]byte, error) {
	out := codec.ConvertFromLatest(pk, nil)
	if len(out) != 1 {
		return nil, fmt.Errorf("codec%d emitted %d packets", codec.ID(), len(out))
	}
	var buf bytes.Buffer
	out[0].Marshal(codec.NewWriter(&buf, 0))
	return buf.Bytes(), nil
}

func matrixDecode(codec minecraft.Protocol, raw []byte, id uint32, listener bool) (packet.Packet, error) {
	ctor := codec.Packets(listener)[id]
	if ctor == nil {
		return nil, fmt.Errorf("codec%d has no packet%d", codec.ID(), id)
	}
	pk := ctor()
	buf := bytes.NewBuffer(raw)
	pk.Marshal(codec.NewReader(buf, 0, true))
	if buf.Len() != 0 {
		return nil, fmt.Errorf("codec%d left %d unread bytes", codec.ID(), buf.Len())
	}
	out := codec.ConvertToLatest(pk, nil)
	if len(out) != 1 {
		return nil, fmt.Errorf("codec%d decoded %d packets", codec.ID(), len(out))
	}
	return out[0], nil
}

func matrixSource(codec minecraft.Protocol, pk packet.Packet, listener bool) (packet.Packet, error) {
	raw, err := matrixEncode(codec, pk)
	if err != nil {
		return nil, err
	}
	return matrixDecode(codec, raw, pk.ID(), listener)
}

func matrixTarget(conn *minecraft.Conn, codec minecraft.Protocol, pk packet.Packet) (packet.Packet, error) {
	if value, ok := sessions.Load(conn); ok {
		translated, err := translateSessionPacket(value.(*session), pk)
		if err != nil {
			return nil, err
		}
		pk = translated
	}
	p := clientVersionProtocol{Protocol: codec, enabled: true}
	out := p.ConvertFromLatest(pk, conn)
	if len(out) != 1 {
		return nil, fmt.Errorf("relay emitted %d packets", len(out))
	}
	var buf bytes.Buffer
	out[0].Marshal(codec.NewWriter(&buf, 0))
	return matrixDecode(codec, buf.Bytes(), pk.ID(), false)
}
