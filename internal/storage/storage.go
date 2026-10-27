// Package storage enforces retention: per-camera age limits, then a
// minimum-free-space floor by deleting the oldest recordings first.
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
}

func sweep(d *db.DB, dir string) {
	cams, err := d.Cameras()
	if err != nil {
		return
	}
	defDays := d.Int("default_retention_days", 14)
	minFree := d.Int("min_free_pct", 5)
	var all []file
	removed := 0
	for _, c := range cams {
		days := c.RetentionDays
		if days <= 0 {
			days = defDays
		}
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		filepath.WalkDir(recorder.CameraDir(dir, c.ID), func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(p, ".mp4") {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return nil
			}
			if info.ModTime().Before(cutoff) {
				remove(p)
				removed++
			} else {
				all = append(all, file{p, info.ModTime()})
			}
			return nil
		})
	}
	// Free-space floor: never touch segments that may still be open.
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for i, f := range all {
		if i%20 == 0 && freePct(dir) >= float64(minFree) {
			break
		}
		if time.Since(f.mod) < 10*time.Minute {
			break
		}
		remove(f.path)
		removed++
	}
	if removed > 0 {
		log.Printf("storage: removed %d old recordings", removed)
	}
}

func remove(p string) {
	os.Remove(p)
}

func freePct(dir string) float64 {
	u := Usage(dir)
	if u.TotalBytes == 0 {
		return 100
	}
	return float64(u.FreeBytes) / float64(u.TotalBytes) * 100
}
