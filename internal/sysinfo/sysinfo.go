// Package sysinfo reads the Pi's own health from /proc and /sys.
package sysinfo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Pi struct {
	TempC      *float64   `json:"temp_c"`    // nil if no sensor is readable
	Throttled  []string   `json:"throttled"` // plain-language flags, empty when all is well
	HasPower   bool       `json:"has_power"` // the firmware reports power/throttle state at all
	Load       [3]float64 `json:"load"`
	MemTotalMB int        `json:"mem_total_mb"`
	MemFreeMB  int        `json:"mem_available_mb"`
	UptimeS    int        `json:"uptime_s"`
}

func Read() Pi {
	var p Pi
	if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			t := v / 1000
			p.TempC = &t
		}
	}
	for _, g := range []string{"/sys/devices/platform/*/get_throttled", "/sys/devices/platform/*/*/get_throttled"} {
		if m, _ := filepath.Glob(g); len(m) > 0 {
			if b, err := os.ReadFile(m[0]); err == nil {
				p.Throttled, p.HasPower = ParseThrottled(strings.TrimSpace(string(b))), true
			}
			break
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Sscanf(string(b), "%f %f %f", &p.Load[0], &p.Load[1], &p.Load[2])
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		p.MemTotalMB, p.MemFreeMB = ParseMemInfo(string(b))
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		var s float64
		fmt.Sscanf(string(b), "%f", &s)
		p.UptimeS = int(s)
	}
	return p
}

// ParseThrottled decodes the Pi firmware's get_throttled value (hex, e.g. "0x50005").
func ParseThrottled(s string) []string {
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 32)
	if err != nil {
		return nil
	}
	flags := []struct {
		bit  uint
		text string
	}{
		{0, "Under-voltage right now"},
		{1, "CPU speed capped right now"},
		{2, "Throttled right now"},
		{3, "Temperature limit reached right now"},
		{16, "Under-voltage since boot"},
		{17, "CPU speed was capped since boot"},
		{18, "Was throttled since boot"},
		{19, "Temperature limit was reached since boot"},
	}
	out := []string{}
	for _, f := range flags {
		if v&(1<<f.bit) != 0 {
			out = append(out, f.text)
		}
	}
	return out
}

// ParseMemInfo returns total and available memory in MB.
func ParseMemInfo(s string) (total, avail int) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.Atoi(f[1])
		switch f[0] {
		case "MemTotal:":
			total = kb / 1024
		case "MemAvailable:":
			avail = kb / 1024
		}
	}
	return
}
