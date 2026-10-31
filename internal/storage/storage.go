// Package storage enforces retention (age, per-camera size limit, and a
// minimum-free-space floor) and reports where recordings live and how big they are.
package storage

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/recorder"
)

type Disk struct {
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

func Usage(dir string) Disk {
	var s syscall.Statfs_t
	if syscall.Statfs(dir, &s) != nil {
		return Disk{}
	}
	return Disk{TotalBytes: uint64(s.Blocks) * uint64(s.Bsize), FreeBytes: uint64(s.Bavail) * uint64(s.Bsize)}
}

func Run(ctx context.Context, d *db.DB, dir string) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		sweep(d, dir)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type file struct {
	path string
	mod  time.Time
	size int64
}

// busy is how recently a file must have changed to count as still being written.
const busy = 10 * time.Minute

func sweep(d *db.DB, dir string) {
	cams, err := d.Cameras()
	if err != nil {
		return
	}
	minFree := d.Int("min_free_pct", 5)
	d.PruneHealth(time.Now().Add(-30 * 24 * time.Hour))
	var all []file // everything that could be deleted to free space
	removed := 0
	for _, c := range cams {
		protected := d.ProtectedClips(c.ID)
		cutoff := time.Now().Add(-time.Duration(c.RetentionDays) * 24 * time.Hour)
		if c.RetentionDays > 0 {
			d.DeleteEventsBefore(c.ID, cutoff)
		}
		var keep []file
		var total int64
		filepath.WalkDir(recorder.CameraDir(dir, c.ID), func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(p, ".mp4") {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return nil
			}
			total += info.Size()
			switch {
			case protected[filepath.Base(p)] && filepath.Base(filepath.Dir(p)) != "low":
				// kept until someone unprotects it
			case c.RetentionDays > 0 && info.ModTime().Before(cutoff):
				remove(p)
				total -= info.Size()
				removed++
			default:
				keep = append(keep, file{p, info.ModTime(), info.Size()})
			}
			return nil
		})
		// Per-camera size limit: oldest first.
		if c.MaxGB > 0 {
			limit := int64(c.MaxGB) << 30
			sort.Slice(keep, func(i, j int) bool { return keep[i].mod.Before(keep[j].mod) })
			for len(keep) > 0 && total > limit && time.Since(keep[0].mod) > busy {
				remove(keep[0].path)
				total -= keep[0].size
				keep = keep[1:]
				removed++
			}
		}
		cleanThumbs(recorder.CameraDir(dir, c.ID))
		all = append(all, keep...)
	}
	// Free-space floor, oldest first across all cameras.
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for i, f := range all {
		if i%20 == 0 && freePct(dir) >= float64(minFree) {
			break
		}
		if time.Since(f.mod) < busy {
			break
		}
		remove(f.path)
		removed++
	}
	if removed > 0 {
		log.Printf("storage: removed %d old recordings", removed)
	}
}

func remove(p string) { os.Remove(p) }

func freePct(dir string) float64 {
	u := Usage(dir)
	if u.TotalBytes == 0 {
		return 100
	}
	return float64(u.FreeBytes) / float64(u.TotalBytes) * 100
}

// cleanThumbs removes cached thumbnails whose clip no longer exists.
// A thumbnail is named <clip>.<seconds>.jpg, or low.<clip>.<seconds>.jpg.
func cleanThumbs(camDir string) {
	ents, _ := os.ReadDir(filepath.Join(camDir, ".thumbs"))
	for _, e := range ents {
		base := strings.TrimSuffix(e.Name(), ".jpg")
		i := strings.LastIndex(base, ".")
		if i < 0 {
			continue
		}
		clip, dir := base[:i], camDir
		if rest, ok := strings.CutPrefix(clip, "low."); ok {
			clip, dir = rest, filepath.Join(camDir, "low")
		}
		if _, err := os.Stat(filepath.Join(dir, clip)); err != nil {
			os.Remove(filepath.Join(camDir, ".thumbs", e.Name()))
		}
	}
}
