// Package mcport locates the UDP ports a local Minecraft process is listening on.
//
// Minecraft 1.26.45 no longer binds the well-known LAN discovery port 7551. It
// opens a single socket on an ephemeral port and both announces itself from it
// and answers discovery requests on it. A client that only knows about 7551
// therefore never sees the world.
//
// When the world runs on the same machine as the joiner, that port can be read
// straight from the OS. This is not a general replacement for discovery over a
// real network — it exists so a locally hosted world is still joinable, and is
// used only to add probe targets alongside the normal broadcast.
package mcport

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Find returns every UDP port owned by a process whose name contains
// "Minecraft". The list is empty when the game is not running, which is not an
// error: discovery then simply relies on broadcasting.
//
// Windows only. On other platforms it returns an empty list so callers degrade
// to broadcast-only discovery.
func Find() []int {
	pids := minecraftPIDs()
	if len(pids) == 0 {
		return nil
	}

	out, err := exec.Command("netstat", "-ano", "-p", "UDP").Output()
	if err != nil {
		return nil
	}

	seen := make(map[int]bool)
	var ports []int
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// Expected: Proto  LocalAddress  ForeignAddress  PID
		if len(fields) < 4 || !strings.EqualFold(fields[0], "UDP") {
			continue
		}
		pid, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil || !pids[pid] {
			continue
		}
		idx := strings.LastIndex(fields[1], ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.Atoi(fields[1][idx+1:])
		if err != nil || port == 0 || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	return ports
}

// minecraftPIDs returns the set of PIDs whose image name mentions Minecraft.
func minecraftPIDs() map[int]bool {
	out, err := exec.Command("tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return nil
	}
	pids := make(map[int]bool)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(strings.ToLower(line), "minecraft") {
			continue
		}
		// CSV: "Image Name","PID","Session Name",...
		parts := strings.Split(line, `","`)
		if len(parts) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.Trim(parts[1], `"`))
		if err == nil {
			pids[pid] = true
		}
	}
	return pids
}

// Describe renders the ports for logging.
func Describe(ports []int) string {
	if len(ports) == 0 {
		return "(none)"
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return fmt.Sprintf("%s", strings.Join(parts, ", "))
}
