// Package hw detects the board, picks the best working H.264 encoder and
// produces setup recommendations for the Pi model Vigil is running on.
package hw

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Encoder struct {
	Name     string `json:"name"`
	Hardware bool   `json:"hardware"`
}

type Recommendation struct {
	MaxCameras     int      `json:"max_cameras"`     // cameras recorded in copy mode
	TranscodeSlots int      `json:"transcode_slots"` // simultaneous transcoded streams
	Summary        string   `json:"summary"`
	Tips           []string `json:"tips"`
}

type Info struct {
	Model          string         `json:"model"`
	Board          int            `json:"board"` // 3, 4, 5 or 0 when unknown
	MemMB          int            `json:"mem_mb"`
	Cores          int            `json:"cores"`
	Encoder        Encoder        `json:"encoder"`
	HWDecode       bool           `json:"hw_decode"`
	Recommendation Recommendation `json:"recommendation"`
}

var boardRe = regexp.MustCompile(`(?:Pi|Module) (\d)`)

func Detect() Info {
	i := Info{Cores: runtime.NumCPU(), MemMB: memMB()}
	i.Model, i.Board = readModel()
	if i.Model == "" {
		i.Model = "Unknown ARM64 device"
	}
	i.Encoder = pickEncoder()
	if g, _ := filepath.Glob("/dev/video1[0-2]"); len(g) > 0 && (i.Board == 3 || i.Board == 4) {
		i.HWDecode = true
	}
	i.Recommendation = recommend(i)
	return i
}

// readModel returns the board model and generation (3, 4, 5 or 0 if unknown).
func readModel() (string, int) {
	for _, p := range []string{"/proc/device-tree/model", "/sys/firmware/devicetree/base/model"} {
		if b, err := os.ReadFile(p); err == nil {
			if m := strings.TrimRight(string(b), "\x00\n "); m != "" {
				return m, boardOf(m)
			}
		}
	}
	// Docker hides /sys/firmware, which /proc/device-tree points into, so
	// inside a container fall back to /proc/cpuinfo.
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		return parseCPUInfo(string(b))
	}
	return "", 0
}

func boardOf(model string) int {
	m := boardRe.FindStringSubmatch(model)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// parseCPUInfo reads the model from the "Model" line, or else guesses the
// generation from the SoC named on the "Hardware" line.
func parseCPUInfo(s string) (string, int) {
	var model, hardware string
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Model":
			model = strings.TrimSpace(v)
		case "Hardware":
			hardware = strings.TrimSpace(v)
		}
	}
	if model != "" {
		return model, boardOf(model)
	}
	switch hardware {
	case "BCM2712":
		return "Raspberry Pi 5", 5
	case "BCM2711":
		return "Raspberry Pi 4", 4
	case "BCM2837", "BCM2835": // 64-bit boards with this SoC name are Pi 3 class
		return "Raspberry Pi 3", 3
	}
	return "", 0
}

func memMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if strings.HasPrefix(s.Text(), "MemTotal:") {
			fs := strings.Fields(s.Text())
			if len(fs) >= 2 {
				kb, _ := strconv.Atoi(fs[1])
				return kb / 1024
			}
		}
	}
	return 0
}

// pickEncoder returns the first encoder that is both listed and actually
// encodes a test clip (h264_v4l2m2m is listed on a Pi 5 but has no device).
func pickEncoder() Encoder {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, _ := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-encoders").Output()
	cancel()
	for _, name := range []string{"h264_v4l2m2m", "libx264", "libopenh264"} {
		if strings.Contains(string(out), " "+name+" ") && probe(name) {
			return Encoder{Name: name, Hardware: name == "h264_v4l2m2m"}
		}
	}
	return Encoder{Name: "none"}
}

func probe(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi",
		"-i", "testsrc2=size=640x360:rate=10", "-frames:v", "10", "-c:v", name,
		"-pix_fmt", "yuv420p", "-f", "null", "-").Run() == nil
}

func recommend(i Info) Recommendation {
	common := []string{
		"Record in copy mode: the camera's H.264/H.265 stream is saved as-is, using almost no CPU.",
		"Store recordings on a USB SSD (or NVMe on a Pi 5), not the SD card.",
		"If a camera offers a sub-stream, add it: it is used for live view and motion detection.",
		"H.265 cameras record fine but most browsers can't play them; enable transcoding on that camera if playback fails.",
	}
	switch i.Board {
	case 3:
		return Recommendation{MaxCameras: 4, TranscodeSlots: 1,
			Summary: "Pi 3B+: copy-mode recording for up to ~4 cameras, hardware H.264 for one transcoded stream.",
			Tips: append([]string{
				"Network is limited to ~300 Mbit/s over USB2: keep total camera bitrate under ~60 Mbit/s.",
				"Use the GPU encoder (h264_v4l2m2m) only for cameras you must transcode, at 1080p or lower.",
				"Keep sub-streams at 640x360 or smaller for motion detection.",
			}, common...)}
	case 4:
		return Recommendation{MaxCameras: 8, TranscodeSlots: 2,
			Summary: "Pi 4: copy-mode recording for ~8 cameras, hardware H.264 for up to 2 transcoded streams.",
			Tips: append([]string{
				"The GPU encoder handles 1080p30 total, so 2 transcodes at 720p is a comfortable limit.",
				"Gigabit Ethernet and USB3 storage make this the best-balanced board for Vigil.",
			}, common...)}
	case 5:
		return Recommendation{MaxCameras: 12, TranscodeSlots: 1,
			Summary: "Pi 5: copy-mode recording for ~12 cameras. It has no H.264 hardware encoder, so avoid transcoding.",
			Tips: append([]string{
				"Transcoding falls back to the CPU (libx264 ultrafast): budget about one core per 720p stream.",
				"Set cameras to H.264 at the source so nothing needs to be transcoded.",
			}, common...)}
	}
	return Recommendation{MaxCameras: 4, TranscodeSlots: 1,
		Summary: "Unrecognised board: starting with conservative limits.", Tips: common}
}
