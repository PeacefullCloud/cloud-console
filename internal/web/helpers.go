package web

import (
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/monitoring"
)

// monitoringHostFallback keeps the UI renderable when Incus cannot report host
// capacity (for example on a fresh install with no pools yet).
var monitoringHostFallback = monitoring.Host{}

// timeNow is a seam for tests.
func timeNow() time.Time { return time.Now() }

// atoiDefault parses an integer form value, falling back to def.
func atoiDefault(value string, def int) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return def
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return def
	}
	return parsed
}

func monitoringPercentSeries(samples []models.Metric) monitoring.Series {
	return monitoring.PercentSeries(samples)
}

func monitoringMemorySeries(samples []models.Metric) monitoring.Series {
	return monitoring.MemorySeries(samples)
}

// poolDrivers returns storage pool names mapped to their driver, cached for a
// minute so the instances list does not hammer the daemon.
var (
	poolDriverMu     sync.Mutex
	poolDriverCache  map[string]string
	poolDriverExpiry time.Time
)

func (s *Server) poolDrivers() map[string]string {
	poolDriverMu.Lock()
	defer poolDriverMu.Unlock()

	if poolDriverCache != nil && time.Now().Before(poolDriverExpiry) {
		return poolDriverCache
	}

	out := map[string]string{}
	if pools, err := s.Instances.StoragePools(); err == nil {
		for _, p := range pools {
			out[p.Name] = p.Driver
		}
	}

	poolDriverCache = out
	poolDriverExpiry = time.Now().Add(time.Minute)
	return out
}

// attachedDisks lists the instance's non-root disk devices.
func (s *Server) attachedDisks(name string) []diskView {
	inst, err := s.Instances.RawInstance(name)
	if err != nil {
		return nil
	}

	devices := inst.ExpandedDevices
	if len(devices) == 0 {
		devices = inst.Devices
	}

	var out []diskView
	for _, deviceName := range sortedKeys(devices) {
		device := devices[deviceName]

		// Root and non-disk devices are not "attached disks".
		if device["type"] != "disk" || device["path"] == "/" {
			continue
		}

		source := device["source"]
		if source == "" {
			source = device["path"]
		}

		out = append(out, diskView{
			Name:   deviceName,
			Type:   "disk",
			Pool:   device["pool"],
			Source: source,
			Path:   device["path"],
		})
	}
	return out
}

// networkDevices describes the instance's NICs.
func (s *Server) networkDevices(name string) []networkView {
	inst, err := s.Instances.RawInstance(name)
	if err != nil {
		return nil
	}

	devices := inst.ExpandedDevices
	if len(devices) == 0 {
		devices = inst.Devices
	}

	var out []networkView
	for _, deviceName := range sortedKeys(devices) {
		device := devices[deviceName]
		if device["type"] != "nic" {
			continue
		}

		network := device["network"]
		if network == "" {
			network = device["parent"]
		}

		out = append(out, networkView{
			Name:    deviceName,
			Type:    "nic",
			Network: network,
			MAC:     device["hwaddr"],
			State:   "up",
		})
	}
	return out
}

// sortedKeys returns a device map's keys in a stable order.
func sortedKeys(devices map[string]map[string]string) []string {
	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// ansiSequences matches terminal escape sequences: CSI (cursor moves,
// colours, clears), OSC (titles, hyperlinks) and other ESC introductions.
var ansiSequences = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[()#][0-9A-Za-z]|\x1b[=>MEHc789]")

// ansiControls matches stray control characters. Newlines and tabs survive.
var ansiControls = regexp.MustCompile("[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]")

// stripANSI removes terminal escape sequences and control characters from
// console output so it renders as readable text.
func stripANSI(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = ansiSequences.ReplaceAllString(s, "")
	return ansiControls.ReplaceAllString(s, "")
}

// consoleTail reads the last n lines of an instance's console log.
//
// The read is capped so a chatty instance cannot exhaust the console's memory,
// and a missing log is not treated as an error the user needs to see.
func (s *Server) consoleTail(name string, n int) (lines []string, problem string) {
	rc, err := s.App.Incus.ConsoleLog(name)
	if err != nil {
		if incus.IsNotFound(err) {
			return nil, ""
		}
		return nil, "Console log is unavailable for this instance."
	}
	defer rc.Close()

	const maxRead = 256 << 10
	body, err := io.ReadAll(io.LimitReader(rc, maxRead))
	if err != nil && len(body) == 0 {
		return nil, "Could not read the console log."
	}

	all := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(all) == 1 && all[0] == "" {
		return nil, ""
	}

	// The console carries raw terminal output, including cursor moves, clear
	// sequences and firmware chatter. Strip the escape sequences so the log
	// reads as text instead of a wall of "[2J[001;001H" noise.
	for i, line := range all {
		all[i] = stripANSI(line)
	}

	// Keep only the tail, and mark that output was truncated.
	if len(all) > n {
		lines = append(lines, "… "+strconv.Itoa(len(all)-n)+" earlier lines omitted")
		all = all[len(all)-n:]
	}
	return append(lines, all...), ""
}
