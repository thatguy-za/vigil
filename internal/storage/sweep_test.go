package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
)

// mk makes a sparse file of the given size whose mtime is age ago.
func mk(t *testing.T, dir, name string, size int64, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(size)
	f.Close()
	mt := time.Now().Add(-age)
	os.Chtimes(p, mt, mt)
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestSweepRetentionProtectionAndQuota(t *testing.T) {
	root := t.TempDir()
	d, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := d.CreateCamera(db.Camera{Name: "c", URL: "rtsp://x/y", Mode: "off", RetentionDays: 7, MaxGB: 1, MotionSource: "vigil", MotionSensitivity: 5})
	dir := filepath.Join(root, "cam"+itoa(id))
	os.MkdirAll(filepath.Join(dir, "low"), 0o755)
	day := 24 * time.Hour
	const mb = int64(1) << 20

	oldPlain := mk(t, dir, "2026-01-01_00-00-00.mp4", 10*mb, 30*day) // past retention: deleted
	oldKept := mk(t, dir, "2026-01-01_01-00-00.mp4", 10*mb, 30*day)  // past retention but protected: kept
	d.SetProtected(id, "2026-01-01_01-00-00.mp4", true)
	a := mk(t, dir, "2026-01-05_00-00-00.mp4", 600*mb, 3*day)         // two clips of 600 MB under a 1 GB limit:
	b := mk(t, dir, "2026-01-06_00-00-00.mp4", 600*mb, 2*day)         // the older one must go
	fresh := mk(t, dir, "2026-01-07_00-00-00.mp4", 1*mb, time.Minute) // still being written: never touched
	lowOld := mk(t, filepath.Join(dir, "low"), "2026-01-01_00-00-00.mp4", 10*mb, 30*day)

	sweep(d, root)

	if exists(oldPlain) || exists(lowOld) {
		t.Error("clips past retention should be deleted (including low-res)")
	}
	if !exists(oldKept) {
		t.Error("a protected clip must survive retention")
	}
	if exists(a) {
		t.Error("the older clip should be removed to get under the size limit")
	}
	if !exists(b) || !exists(fresh) {
		t.Error("the newer clip and the one still being written must be kept")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}
