package main

// Command interface. One JSON object per line, read from -script or stdin.
// Every command answers with one JSON line, so the client can be driven from a
// terminal, a shell pipe, or a test harness without a GUI.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

type command struct {
	Cmd      string   `json:"cmd"`
	Addr     string   `json:"addr,omitempty"`
	Direct   *bool    `json:"direct,omitempty"`
	Proto    int32    `json:"proto,omitempty"`
	Name     string   `json:"name,omitempty"`
	MS       int      `json:"ms,omitempty"`
	X        *float32 `json:"x,omitempty"`
	Y        *float32 `json:"y,omitempty"`
	Z        *float32 `json:"z,omitempty"`
	Yaw      *float32 `json:"yaw,omitempty"`
	Pitch    *float32 `json:"pitch,omitempty"`
	On       *bool    `json:"on,omitempty"`
	Slot     *int     `json:"slot,omitempty"`
	Message  string   `json:"message,omitempty"`
	FormID   *uint32  `json:"form_id,omitempty"`
	Response string   `json:"response,omitempty"`
	Metric   string   `json:"metric,omitempty"`
	Op       string   `json:"op,omitempty"`
	Value    any      `json:"value,omitempty"`
}

type result struct {
	OK       bool            `json:"ok"`
	Cmd      string          `json:"cmd"`
	Detail   string          `json:"detail,omitempty"`
	Error    string          `json:"error,omitempty"`
	Quit     bool            `json:"quit,omitempty"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
}

func okResult(cmd, detail string) result {
	return result{OK: true, Cmd: cmd, Detail: detail}
}

func errResult(cmd string, err error) result {
	return result{OK: false, Cmd: cmd, Error: err.Error()}
}

func (s *session) requireJoined(cmd string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.joined {
		return fmt.Errorf("%s requires a successful join first", cmd)
	}
	return nil
}

// compare evaluates one assert. Numeric comparisons coerce both sides to
// float64; bool/string use equality or substring matching.
func compare(actual any, op string, want any) (bool, string) {
	asFloat := func(v any) (float64, bool) {
		switch n := v.(type) {
		case float64:
			return n, true
		case float32:
			return float64(n), true
		case int:
			return float64(n), true
		case int32:
			return float64(n), true
		case int64:
			return float64(n), true
		case uint64:
			return float64(n), true
		case json.Number:
			f, err := n.Float64()
			return f, err == nil
		}
		return 0, false
	}
	af, aNum := asFloat(actual)
	wf, wNum := asFloat(want)
	if aNum && wNum {
		switch op {
		case ">":
			return af > wf, fmt.Sprintf("%v %s %v", af, op, wf)
		case ">=":
			return af >= wf, fmt.Sprintf("%v %s %v", af, op, wf)
		case "<":
			return af < wf, fmt.Sprintf("%v %s %v", af, op, wf)
		case "<=":
			return af <= wf, fmt.Sprintf("%v %s %v", af, op, wf)
		case "==", "=", "":
			return af == wf, fmt.Sprintf("%v == %v", af, wf)
		case "!=":
			return af != wf, fmt.Sprintf("%v != %v", af, wf)
		}
	}
	as, aStr := actual.(string)
	ws, wStr := want.(string)
	if op == "contains" && aStr && wStr {
		return strings.Contains(as, ws), fmt.Sprintf("%q contains %q", as, ws)
	}
	switch op {
	case "==", "=", "":
		return fmt.Sprint(actual) == fmt.Sprint(want), fmt.Sprintf("%v == %v", actual, want)
	case "!=":
		return fmt.Sprint(actual) != fmt.Sprint(want), fmt.Sprintf("%v != %v", actual, want)
	default:
		return false, fmt.Sprintf("operator %q not applicable to %T vs %T", op, actual, want)
	}
}

func (s *session) exec(c command) result {
	cmd := strings.ToLower(strings.TrimSpace(c.Cmd))
	switch cmd {
	case "join":
		addr := c.Addr
		if addr == "" {
			addr = s.cfg.addr
		}
		if addr == "" {
			return errResult(cmd, fmt.Errorf("join needs an addr (command field or -addr/-direct)"))
		}
		direct := s.cfg.direct
		if c.Direct != nil {
			direct = *c.Direct
		}
		if err := s.join(addr, direct, c.Proto, c.Name); err != nil {
			s.mu.Lock()
			s.errorText = err.Error()
			s.mu.Unlock()
			return errResult(cmd, err)
		}
		return okResult(cmd, fmt.Sprintf("joined %s proto=%d spawn_ms=%d direct=%t",
			addr, s.proto, s.spawnMS, direct))

	case "wait":
		if c.MS <= 0 {
			return errResult(cmd, fmt.Errorf("wait needs ms > 0"))
		}
		t := time.NewTimer(time.Duration(c.MS) * time.Millisecond)
		defer t.Stop()
		select {
		case <-t.C:
			return okResult(cmd, fmt.Sprintf("waited %d ms", c.MS))
		case <-s.dead:
			return errResult(cmd, fmt.Errorf("connection died during wait"))
		case <-s.ctx.Done():
			return errResult(cmd, fmt.Errorf("stopped during wait"))
		}

	case "move":
		if err := s.requireJoined(cmd); err != nil {
			return errResult(cmd, err)
		}
		s.mu.Lock()
		if c.X != nil {
			s.pos[0] = *c.X
		}
		if c.Y != nil {
			s.pos[1] = *c.Y
		}
		if c.Z != nil {
			s.pos[2] = *c.Z
		}
		p := []float32{s.pos.X(), s.pos.Y(), s.pos.Z()}
		s.mu.Unlock()
		return okResult(cmd, fmt.Sprintf("position=%v (applied on next PlayerAuthInput)", p))

	case "look":
		if err := s.requireJoined(cmd); err != nil {
			return errResult(cmd, err)
		}
		s.mu.Lock()
		if c.Yaw != nil {
			s.yaw = *c.Yaw
			s.headYaw = *c.Yaw
		}
		if c.Pitch != nil {
			s.pitch = *c.Pitch
		}
		yaw, pitch := s.yaw, s.pitch
		s.mu.Unlock()
		return okResult(cmd, fmt.Sprintf("yaw=%.3f pitch=%.3f", yaw, pitch))

	case "jump":
		if err := s.requireJoined(cmd); err != nil {
			return errResult(cmd, err)
		}
		s.mu.Lock()
		s.wantJump = true
		s.mu.Unlock()
		return okResult(cmd, "jump queued for the next PlayerAuthInput")

	case "sneak", "sprint":
		if err := s.requireJoined(cmd); err != nil {
			return errResult(cmd, err)
		}
		on := true
		if c.On != nil {
			on = *c.On
		}
		s.mu.Lock()
		if cmd == "sneak" {
			s.sneaking = on
		} else {
			s.sprinting = on
		}
		s.mu.Unlock()
		return okResult(cmd, fmt.Sprintf("%s=%t", cmd, on))

	case "chat":
		if err := s.requireJoined(cmd); err != nil {
			return errResult(cmd, err)
		}
		if c.Message == "" {
			return errResult(cmd, fmt.Errorf("chat needs a message"))
		}
		err := s.send(&packet.Text{
			TextType:   packet.TextTypeChat,
			SourceName: s.name,
			Message:    c.Message,
		})
		if err != nil {
			return errResult(cmd, err)
		}
		return okResult(cmd, fmt.Sprintf("sent chat %q", c.Message))

	case "hotbar":
		if err := s.requireJoined(cmd); err != nil {
			return errResult(cmd, err)
		}
		if c.Slot == nil || *c.Slot < 0 || *c.Slot > 8 {
			return errResult(cmd, fmt.Errorf("hotbar needs slot in 0..8"))
		}
		slot := byte(*c.Slot)
		err := s.send(&packet.MobEquipment{
			EntityRuntimeID: s.gd.EntityRuntimeID,
			NewItem:         protocol.ItemInstance{},
			InventorySlot:   slot,
			HotBarSlot:      slot,
			WindowID:        0,
		})
		if err != nil {
			return errResult(cmd, err)
		}
		return okResult(cmd, fmt.Sprintf("hotbar slot=%d", slot))

	case "form":
		if err := s.requireJoined(cmd); err != nil {
			return errResult(cmd, err)
		}
		if c.FormID == nil {
			return errResult(cmd, fmt.Errorf("form needs form_id"))
		}
		response := c.Response
		if response == "" {
			response = "null"
		}
		err := s.send(&packet.ModalFormResponse{
			FormID:       *c.FormID,
			ResponseData: protocol.Option([]byte(response)),
		})
		if err != nil {
			return errResult(cmd, err)
		}
		return okResult(cmd, fmt.Sprintf("form %d answered with %s", *c.FormID, response))

	case "assert":
		if c.Metric == "" {
			return errResult(cmd, fmt.Errorf("assert needs a metric"))
		}
		snap := s.snapshot()
		actual, found := snap[c.Metric]
		if !found {
			// Packet counters are exposed as packet_<Name>/sent_<Name>.
			return errResult(cmd, fmt.Errorf("unknown metric %q (use dump to list metrics)", c.Metric))
		}
		pass, detail := compare(actual, c.Op, c.Value)
		if !pass {
			return result{OK: false, Cmd: cmd,
				Error: fmt.Sprintf("assert FAILED: %s = %v, want %s %v (%s)", c.Metric, actual, c.Op, c.Value, detail)}
		}
		return okResult(cmd, fmt.Sprintf("assert %s: %s", c.Metric, detail))

	case "dump":
		return result{OK: true, Cmd: cmd, Snapshot: s.snapshotJSON()}

	case "quit":
		return result{OK: true, Cmd: cmd, Quit: true, Detail: "shutting down"}

	default:
		return errResult(cmd, fmt.Errorf("unknown command %q (join|wait|move|look|jump|sneak|sprint|chat|hotbar|form|assert|dump|quit)", c.Cmd))
	}
}
