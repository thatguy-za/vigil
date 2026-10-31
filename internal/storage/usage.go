package storage

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thatguy-za/vigil/internal/recorder"
)

// CamUsage is how much space one camera's recordings take.
type CamUsage struct {
	Bytes          int64     `json:"bytes"`
	Clips          int       `json:"clips"`
	ProtectedBytes int64     `json:"protected_bytes"`
	Last24hBytes   int64     `json:"last_24h_bytes"`
	Oldest         time.Time `json:"oldest"`
}

func CameraUsage(dir string, id int64, protected map[string]bool) CamUsage {
	var u CamUsage
	day := time.Now().Add(-24 * time.Hour)
	filepath.WalkDir(recorder.CameraDir(dir, id), func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(p, ".mp4") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		u.Bytes += info.Size()
		u.Clips++
		if protected[filepath.Base(p)] {
			u.ProtectedBytes += info.Size()
		}
		if info.ModTime().After(day) {
			u.Last24hBytes += info.Size()
		}
		if u.Oldest.IsZero() || info.ModTime().Before(u.Oldest) {
			u.Oldest = info.ModTime()
		}
		return nil
	})
	return u
}

// MountInfo says what the recordings folder is stored on.
type MountInfo struct {
	Source string `json:"source"` // e.g. /dev/sda1
	FSType string `json:"fs_type"`
	Point  string `json:"mount_point"`
	SDCard bool   `json:"sd_card"`          // an SD/eMMC card wears out under constant writes
	Inside bool   `json:"inside_container"` // not a mounted drive: data is lost if the container is recreated
}

func Mount(dir string) MountInfo {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return MountInfo{}
	}
	return parseMount(string(b), dir)
}

// parseMount finds the mount that contains dir (the longest matching mount point).
func parseMount(mountinfo, dir string) MountInfo {
	var best MountInfo
	for _, line := range strings.Split(mountinfo, "\n") {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		lf, rf := strings.Fields(left), strings.Fields(right)
		if len(lf) < 5 || len(rf) < 2 {
			continue
		}
		point := lf[4]
		if point != "/" && dir != point && !strings.HasPrefix(dir, point+"/") {
			continue
		}
		if len(point) >= len(best.Point) {
			best = MountInfo{Source: rf[1], FSType: rf[0], Point: point}
		}
	}
	best.SDCard = strings.Contains(best.Source, "mmcblk")
	best.Inside = best.Point == "/" || best.FSType == "overlay" || best.FSType == "tmpfs"
	return best
}
