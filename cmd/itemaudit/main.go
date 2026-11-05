// Command itemaudit answers one question with evidence instead of a guess:
// which items would a Bedrock client NOT see in its inventory when the relay
// translates between a native 2193 host and a historical client?
//
// It is deliberately OFFLINE. A real Bedrock client cannot be used for this
// measurement here: Cinnabar is a GUI desktop client (make play), pinned to
// protocol 2225, and licensed PolyForm Noncommercial, so it is not a headless
// multi-version harness. The source of truth is instead the exact codec the
// relay installs (NewWithRegistries), exercised end-to-end on the WIRE:
//
//	native item stack -> codec.ConvertFromLatest -> codec.NewWriter (encode)
//	-> codec.Packets(false) pool + codec.NewReader (decode, exactly like
//	   Conn.ReadPacket) -> codec.ConvertToLatest (unwrap the historical
//	   adapters' translatedPacket) -> inspect NetworkID.
//
// NetworkID=0 after that round trip means the client decodes AIR. Every such
// item is logged by NAME and runtime ID. The result is cross-checked against
// MappingReport().ItemFallbacks where the adapter exports it, so the tool
// cannot silently disagree with the mapper it is auditing.
//
// NOTE: this enumerates exactly which items would be missing. It does NOT
// claim that a real Bedrock client rendered them.
//
// Usage:
//
//	go run ./cmd/itemaudit                        # all four protocols, every lost item
//	go run ./cmd/itemaudit -proto 2169 -limit 20
//	go run ./cmd/itemaudit -json dist/item-audit.json
//	go run ./cmd/itemaudit -fail-on-loss         # non-zero exit when anything is lost
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"bedrock-cross-version-relay/internal/blocktranslate"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	v1_26_30 "github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
	v1_26_44 "github.com/shawtymarco/go-multiversion/protocols/v1_26_44"
	v1_26_45 "github.com/shawtymarco/go-multiversion/protocols/v1_26_45"
)

var allProtocols = []int32{1001, 2168, 2169, 2193}

const (
	reasonNoTarget   = "NO_TARGET_ENTRY" // native item has no same-name entry in the target registry
	reasonMappedAIR  = "MAPPED_TO_AIR"   // an entry exists, but the codec still produced AIR
	reasonWrongID    = "WRONG_TARGET_ID" // visible, but the round trip changed the runtime ID
	reasonUnknownAIR = "UNKNOWN_ID_AIR"  // runtime ID absent from the native registry, turned into AIR
)

type lostItem struct {
	Name              string `json:"name"`
	NativeRuntimeID   int32  `json:"native_runtime_id"`
	ExpectedTargetID  int32  `json:"expected_target_id"`
	ObservedNetworkID int32  `json:"observed_network_id"`
	Reason            string `json:"reason"`
}

type unknownProbe struct {
	InputRuntimeID    int32 `json:"input_runtime_id"`
	ObservedNetworkID int32 `json:"observed_network_id"`
	BecameAir         bool  `json:"became_air"`
}

// wireRangeProbe documents a real protocol constraint, not an invisible item:
// ItemInstance writes NetworkID as int16, so IDs outside [-32768, 32767] are
// truncated on the wire even by the identity 2193 codec.
type wireRangeProbe struct {
	InputRuntimeID    int32 `json:"input_runtime_id"`
	ObservedNetworkID int32 `json:"observed_network_id"`
	Truncated         bool  `json:"truncated"`
}

type protoReport struct {
	Protocol        int32           `json:"protocol"`
	Codec           string          `json:"codec"`
	NativeItems     int             `json:"native_items"`
	TargetItems     int             `json:"target_items"`
	Sent            int             `json:"sent"`
	Visible         int             `json:"visible"`
	Lost            int             `json:"lost"`
	WrongID         int             `json:"wrong_id"`
	PacketDropped   bool            `json:"packet_dropped"`
	WireBytes       int             `json:"wire_bytes"`
	LostItems       []lostItem      `json:"lost_items"`
	UnknownProbes   []unknownProbe  `json:"unknown_probes"`
	RangeProbe      *wireRangeProbe `json:"range_probe,omitempty"`
	TargetOnlyNames []string        `json:"target_only_names"`
	MapperFallbacks int             `json:"mapper_fallbacks"`
	MapperReported  bool            `json:"mapper_reported"`
	CrossCheck      string          `json:"cross_check"`
	Error           string          `json:"error,omitempty"`
}

type auditReport struct {
	Note      string        `json:"note"`
	Protocols []protoReport `json:"protocols"`
}

// codecFor mirrors cmd/xvrelay.resolveProto: registry-aware for the three
// historical protocols, plain DefaultProtocol for the native 2193 client.
// It is duplicated here on purpose — the audit must be an INDEPENDENT binary
// that does not import the relay's package main.
func codecFor(id int32) (minecraft.Protocol, string, error) {
	reg, err := blocktranslate.NativeRegistry(2193)
	if err != nil {
		return nil, "", fmt.Errorf("native 2193 block registry: %w", err)
	}
	items, err := blocktranslate.NativeItems()
	if err != nil {
		return nil, "", fmt.Errorf("native 2193 item registry: %w", err)
	}
	switch id {
	case 1001:
		p, err := v1_26_30.NewWithRegistries(reg, items)
		return p, "v1_26_30.NewWithRegistries", err
	case 2168:
		p, err := v1_26_44.NewWithRegistries(reg, items)
		return p, "v1_26_44.NewWithRegistries (delegates to v1_26_45)", err
	case 2169:
		p, err := v1_26_45.NewWithRegistries(reg, items)
		return p, "v1_26_45.NewWithRegistries", err
	case 2193:
		return minecraft.DefaultProtocol, "minecraft.DefaultProtocol (native, identity)", nil
	default:
		return nil, "", fmt.Errorf("unsupported protocol %d", id)
	}
}

// mapperFallbackNames pulls ItemFallbacks out of the adapters that export a
// MappingReport. v1_26_44 wraps v1_26_45 without exporting the report, so for
// 2168 the wire measurement itself is the evidence.
func mapperFallbackNames(p minecraft.Protocol) ([]string, bool) {
	var names []string
	switch r := p.(type) {
	case interface {
		MappingReport() v1_26_30.MappingReport
	}:
		for _, f := range r.MappingReport().ItemFallbacks {
			names = append(names, f.Name)
		}
		return names, true
	case interface {
		MappingReport() v1_26_45.MappingReport
	}:
		for _, f := range r.MappingReport().ItemFallbacks {
			names = append(names, f.Name)
		}
		return names, true
	}
	return nil, false
}

func itemStack(id int32) protocol.ItemInstance {
	return protocol.ItemInstance{
		Stack: protocol.ItemStack{
			ItemType: protocol.ItemType{NetworkID: id},
			Count:    1,
		},
	}
}

func nameIndex(entries []protocol.ItemEntry) map[string]int32 {
	out := make(map[string]int32, len(entries))
	for _, e := range entries {
		if _, exists := out[e.Name]; !exists {
			out[e.Name] = int32(e.RuntimeID)
		}
	}
	return out
}

// unusedInt16IDs finds runtime IDs that are absent from the native registry but
// still fit in the int16 wire field of ItemInstance. Those are genuine
// "unknown item" probes; IDs outside int16 are a separate wire constraint.
func unusedInt16IDs(native []protocol.ItemEntry, n int) []int32 {
	used := make(map[int32]bool, len(native))
	for _, e := range native {
		used[int32(e.RuntimeID)] = true
	}
	out := make([]int32, 0, n)
	for id := int32(20000); len(out) < n && id <= 32767; id++ {
		if !used[id] {
			out = append(out, id)
		}
	}
	return out
}

// wireContent encodes pk exactly like Conn.WritePacket and decodes it with the
// target protocol's own packet pool and reader — i.e. the bytes a client of
// that version would parse. The historical adapters return a translatedPacket
// wrapper whose wire layout differs from the latest protocol; decoding into a
// plain packet.InventoryContent would misalign (measured: 30219 unread bytes
// for protocol 1001). ConvertToLatest unwraps that wrapper and brings the IDs
// back to native shape, so each stack can be compared to the ID we sent.
// NetworkID=0 survives both directions: it is AIR.
func wireContent(codec minecraft.Protocol, pk packet.Packet, want int) ([]protocol.ItemInstance, int, error) {
	var buf bytes.Buffer
	h := packet.Header{PacketID: pk.ID()}
	if err := h.Write(&buf); err != nil {
		return nil, 0, fmt.Errorf("write header: %w", err)
	}
	pk.Marshal(codec.NewWriter(&buf, 0))
	wireBytes := buf.Len()

	var rh packet.Header
	if err := rh.Read(&buf); err != nil {
		return nil, wireBytes, fmt.Errorf("read header: %w", err)
	}
	ctor, ok := codec.Packets(false)[rh.PacketID]
	if !ok {
		return nil, wireBytes, fmt.Errorf("no packet constructor for ID %d", rh.PacketID)
	}
	decoded := ctor()
	if err := decodeSafely(codec, decoded, &buf); err != nil {
		return nil, wireBytes, err
	}
	if buf.Len() != 0 {
		return nil, wireBytes, fmt.Errorf("wire decode misaligned: %d unread bytes", buf.Len())
	}
	latest := codec.ConvertToLatest(decoded, nil)
	if len(latest) == 0 {
		return nil, wireBytes, fmt.Errorf("ConvertToLatest dropped the decoded packet")
	}
	ic, ok := latest[0].(*packet.InventoryContent)
	if !ok {
		return nil, wireBytes, fmt.Errorf("decoded packet is %T", latest[0])
	}
	if len(ic.Content) != want {
		return nil, wireBytes, fmt.Errorf("decoded %d stacks, want %d", len(ic.Content), want)
	}
	return ic.Content, wireBytes, nil
}

// decodeSafely turns a decoder panic (invalid wire data) into an error.
func decodeSafely(codec minecraft.Protocol, pk packet.Packet, buf *bytes.Buffer) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("decode %T panicked: %v", pk, p)
		}
	}()
	pk.Marshal(codec.NewReader(buf, 0, false))
	return nil
}

func auditProtocol(id int32, logf, plainf func(string, ...any)) protoReport {
	r := protoReport{Protocol: id, LostItems: []lostItem{}, UnknownProbes: []unknownProbe{}, TargetOnlyNames: []string{}}

	native, err := blocktranslate.NativeItems()
	if err != nil {
		r.Error = fmt.Sprintf("NativeItems: %v", err)
		return r
	}
	target, err := blocktranslate.Items(id)
	if err != nil {
		r.Error = fmt.Sprintf("Items(%d): %v", id, err)
		return r
	}
	codec, codecName, err := codecFor(id)
	if err != nil {
		r.Error = fmt.Sprintf("codecFor(%d): %v", id, err)
		return r
	}
	r.Codec = codecName
	r.NativeItems = len(native)
	r.TargetItems = len(target)
	targetByName := nameIndex(target)
	nativeByName := nameIndex(native)

	// Target-only names exist in the historical registry but not in the native
	// one: the client can hold them, but the host has no semantic equivalent.
	for name := range targetByName {
		if _, ok := nativeByName[name]; !ok {
			r.TargetOnlyNames = append(r.TargetOnlyNames, name)
		}
	}
	sort.Strings(r.TargetOnlyNames)

	// Push EVERY native item through the real codec in one packet, then decode
	// it from the wire with the client protocol's own packet pool.
	stacks := make([]protocol.ItemInstance, len(native))
	for i, e := range native {
		stacks[i] = itemStack(int32(e.RuntimeID))
	}
	r.Sent = len(stacks)
	out := codec.ConvertFromLatest(&packet.InventoryContent{Content: stacks}, (*minecraft.Conn)(nil))
	if len(out) == 0 {
		r.PacketDropped = true
		r.Error = "ConvertFromLatest dropped the whole InventoryContent packet"
		plainf("proto=%d PACKET DROPPED — no item reached the client at all", id)
		return r
	}
	content, wireBytes, err := wireContent(codec, out[0], len(stacks))
	r.WireBytes = wireBytes
	if err != nil {
		r.Error = fmt.Sprintf("wire round trip: %v", err)
		return r
	}

	for i, e := range native {
		nativeID := int32(e.RuntimeID)
		observed := content[i].Stack.NetworkID
		expected, hasTarget := targetByName[e.Name]
		switch {
		case observed == 0:
			reason := reasonMappedAIR
			if !hasTarget {
				reason = reasonNoTarget
			}
			r.Lost++
			r.LostItems = append(r.LostItems, lostItem{e.Name, nativeID, expected, observed, reason})
		case observed != nativeID:
			r.WrongID++
			r.LostItems = append(r.LostItems, lostItem{e.Name, nativeID, expected, observed, reasonWrongID})
		default:
			r.Visible++
		}
	}

	// Unknown / custom runtime IDs: the host (or an add-on) sends an ID the
	// native 2193 registry does not contain. hideUnknown=true turns it into AIR
	// for registry-aware codecs; the identity 2193 codec keeps it as-is.
	// IDs stay inside int16 so the wire field itself is not the confounder.
	probeIDs := unusedInt16IDs(native, 3)
	probeStacks := make([]protocol.ItemInstance, len(probeIDs))
	for i, pid := range probeIDs {
		probeStacks[i] = itemStack(pid)
	}
	probeOut := codec.ConvertFromLatest(&packet.InventoryContent{Content: probeStacks}, (*minecraft.Conn)(nil))
	observedProbes := make([]int32, len(probeIDs))
	if len(probeOut) == 1 {
		if content2, _, err := wireContent(codec, probeOut[0], len(probeIDs)); err == nil {
			for i := range content2 {
				observedProbes[i] = content2[i].Stack.NetworkID
			}
		}
	}
	for i, pid := range probeIDs {
		observed := observedProbes[i]
		becameAir := observed == 0
		r.UnknownProbes = append(r.UnknownProbes, unknownProbe{pid, observed, becameAir})
		if becameAir {
			plainf("proto=%d UNKNOWN_ID_AIR id=%d observed=%d", id, pid, observed)
			r.LostItems = append(r.LostItems, lostItem{
				Name:              fmt.Sprintf("<custom/unknown runtime id %d>", pid),
				NativeRuntimeID:   pid,
				ObservedNetworkID: observed,
				Reason:            reasonUnknownAIR,
			})
		} else {
			plainf("proto=%d UNKNOWN_ID_KEPT id=%d observed=%d (identity codec, client sees the raw ID)", id, pid, observed)
		}
	}

	// Separate probe for the int16 wire limit. This is NOT an invisible item:
	// protocol.ItemInstance writes NetworkID with Int16, so a custom ID outside
	// [-32768, 32767] is truncated before the client can look it up.
	const outOfRangeID int32 = 900001
	rangeStacks := []protocol.ItemInstance{itemStack(outOfRangeID)}
	rangeOut := codec.ConvertFromLatest(&packet.InventoryContent{Content: rangeStacks}, (*minecraft.Conn)(nil))
	if len(rangeOut) == 1 {
		if c, _, err := wireContent(codec, rangeOut[0], 1); err == nil {
			observed := c[0].Stack.NetworkID
			// observed==0 means hideUnknown turned it into AIR before encoding;
			// only a non-zero, non-original value is a wire truncation.
			truncated := observed != 0 && observed != outOfRangeID
			r.RangeProbe = &wireRangeProbe{outOfRangeID, observed, truncated}
			if truncated {
				plainf("proto=%d WIRE_INT16_TRUNCATION input=%d observed=%d (ItemInstance writes NetworkID as int16; a wire constraint, not a missing item)",
					id, outOfRangeID, observed)
			} else {
				plainf("proto=%d WIRE_RANGE_PROBE_BECAME_AIR input=%d observed=0 (hideUnknown replaced the out-of-range ID before encoding)",
					id, outOfRangeID)
			}
		}
	}

	// Cross-check against the mapper the adapter actually uses.
	fallbackNames, reported := mapperFallbackNames(codec)
	r.MapperReported = reported
	r.MapperFallbacks = len(fallbackNames)
	if reported {
		measured := make(map[string]bool)
		for _, li := range r.LostItems {
			if li.Reason == reasonNoTarget {
				measured[li.Name] = true
			}
		}
		declared := make(map[string]bool, len(fallbackNames))
		for _, n := range fallbackNames {
			declared[n] = true
		}
		missing, extra := 0, 0
		for n := range declared {
			if !measured[n] {
				missing++
			}
		}
		for n := range measured {
			if !declared[n] {
				extra++
			}
		}
		if missing == 0 && extra == 0 {
			r.CrossCheck = fmt.Sprintf("OK: %d mapper ItemFallbacks == %d items measured as AIR", len(fallbackNames), len(measured))
		} else {
			r.CrossCheck = fmt.Sprintf("MISMATCH: mapper=%d measured=%d missing=%d extra=%d", len(fallbackNames), len(measured), missing, extra)
		}
	} else if id == 2168 {
		r.CrossCheck = "not available: v1_26_44.Protocol wraps v1_26_45 without exporting MappingReport; the wire measurement is the direct evidence (2168 shares the 2169 target snapshot)"
	} else {
		r.CrossCheck = "not applicable: native 2193 codec is identity"
	}

	for _, li := range r.LostItems {
		logf("proto=%d INVISIBLE name=%s native_id=%d expected_target_id=%d observed=%d reason=%s",
			id, li.Name, li.NativeRuntimeID, li.ExpectedTargetID, li.ObservedNetworkID, li.Reason)
	}
	for _, name := range r.TargetOnlyNames {
		logf("proto=%d TARGET_ONLY (rejected serverbound) name=%s", id, name)
	}
	return r
}

func main() {
	protoFlag := flag.Int("proto", 0, "audit only this protocol (1001|2168|2169|2193); 0 = all")
	jsonPath := flag.String("json", "", "write the full JSON report to this path")
	limit := flag.Int("limit", 0, "max INVISIBLE/TARGET_ONLY detail lines per protocol (0 = all)")
	failOnLoss := flag.Bool("fail-on-loss", false, "exit non-zero when any item is invisible (opt-in for CI)")
	flag.Parse()

	var protos []int32
	if *protoFlag == 0 {
		protos = allProtocols
	} else {
		protos = []int32{int32(*protoFlag)}
	}

	report := auditReport{
		Note: "Offline wire-level audit: every native 2193 item is pushed through the real client codec, " +
			"encoded with its writer and decoded with its own packet pool/reader. NetworkID=0 means the client " +
			"decodes AIR. This is NOT a claim about what a real Bedrock client renders; it enumerates exactly " +
			"which items would be missing. Cinnabar cannot be used as a headless multi-version client (GUI, " +
			"pinned to protocol 2225, PolyForm Noncommercial).",
		Protocols: []protoReport{},
	}

	anyError := false
	for _, id := range protos {
		var lines int
		logf := func(format string, args ...any) {
			if *limit > 0 && lines >= *limit {
				return
			}
			lines++
			fmt.Printf("itemaudit: "+format+"\n", args...)
		}
		plainf := func(format string, args ...any) {
			fmt.Printf("itemaudit: "+format+"\n", args...)
		}
		r := auditProtocol(id, logf, plainf)
		report.Protocols = append(report.Protocols, r)
		plainf("SUMMARY proto=%d codec=%q native=%d target=%d sent=%d visible=%d lost=%d wrong_id=%d packet_dropped=%v wire_bytes=%d mapper_fallbacks=%d cross_check=%q",
			r.Protocol, r.Codec, r.NativeItems, r.TargetItems, r.Sent, r.Visible, r.Lost,
			r.WrongID, r.PacketDropped, r.WireBytes, r.MapperFallbacks, r.CrossCheck)
		if r.Error != "" {
			anyError = true
			plainf("ERROR proto=%d %s", id, r.Error)
		}
		if *limit > 0 {
			plainf("(detail log limited to %d lines; full list is in -json)", *limit)
		}
	}

	if *jsonPath != "" {
		if dir := filepath.Dir(*jsonPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "itemaudit: create report dir: %v\n", err)
				os.Exit(2)
			}
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "itemaudit: marshal report: %v\n", err)
			os.Exit(2)
		}
		if err := os.WriteFile(*jsonPath, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "itemaudit: write report: %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("itemaudit: report written to %s\n", *jsonPath)
	}

	totalLost, totalTargetOnly := 0, 0
	for _, r := range report.Protocols {
		totalLost += r.Lost
		totalTargetOnly += len(r.TargetOnlyNames)
	}
	fmt.Printf("itemaudit: TOTAL lost=%d target_only=%d protocols=%d\n", totalLost, totalTargetOnly, len(report.Protocols))

	if anyError {
		os.Exit(2)
	}
	if *failOnLoss && totalLost > 0 {
		os.Exit(1)
	}
}
