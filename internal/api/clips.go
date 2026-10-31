package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thatguy-za/vigil/internal/recorder"
	"github.com/thatguy-za/vigil/internal/storage"
)

// slug makes a camera name safe for a file name.
func slug(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	if out := strings.Trim(b.String(), "-"); out != "" {
		return out
	}
	return "camera"
}

// clipFile validates a clip name from the URL and returns its path on disk.
func (s *Server) clipFile(w http.ResponseWriter, r *http.Request) (id int64, name, path string, ok bool) {
	c, ok := s.camera(w, r)
	if !ok {
		return 0, "", "", false
	}
	name = r.PathValue("name")
	if !clipRe.MatchString(name) {
		fail(w, 400, "invalid clip name")
		return 0, "", "", false
	}
	return c.ID, name, filepath.Join(recorder.CameraDir(s.RecDir, c.ID), name), true
}

func (s *Server) protectClip(w http.ResponseWriter, r *http.Request) {
	id, name, path, ok := s.clipFile(w, r)
	if !ok {
		return
	}
	var b struct{ Protected bool }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&b) != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	if _, err := os.Stat(path); err != nil {
		fail(w, 404, "clip not found")
		return
	}
	if err := s.DB.SetProtected(id, name, b.Protected); err != nil {
		fail(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteClip(w http.ResponseWriter, r *http.Request) {
	id, name, path, ok := s.clipFile(w, r)
	if !ok {
		return
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		fail(w, 404, "clip not found")
	case s.DB.ProtectedClips(id)[name]:
		fail(w, 409, "This clip is protected. Unprotect it first.")
	case time.Since(info.ModTime()) < 30*time.Second:
		fail(w, 409, "This clip is still being recorded.")
	default:
		if err := os.Remove(path); err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.DB.ForgetClip(id, name)
		w.WriteHeader(204)
	}
}

// ---------- settings ----------

var settingDefs = []struct {
	Key           string
	Def, Min, Max int
}{
	{"default_retention_days", 14, 0, 3650},
	{"min_free_pct", 5, 1, 50},
	{"preroll_s", 5, 0, 30},
	{"postroll_s", 10, 1, 120},
	{"session_days", 30, 1, 365},
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"timezone": time.Local.String(), "segment_s": recorder.SegmentSeconds}
	for _, d := range settingDefs {
		out[d.Key] = s.DB.Int(d.Key, d.Def)
	}
	jsonOut(w, 200, out)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var b map[string]int
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&b) != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	for _, d := range settingDefs {
		if v, ok := b[d.Key]; ok {
			if v < d.Min || v > d.Max {
				fail(w, 400, d.Key+" must be between "+strconv.Itoa(d.Min)+" and "+strconv.Itoa(d.Max))
				return
			}
		}
	}
	for _, d := range settingDefs {
		if v, ok := b[d.Key]; ok {
			s.DB.SetInt(d.Key, v)
		}
	}
	s.getSettings(w, r)
}

// ---------- storage ----------

type camStorage struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	MaxGB int    `json:"max_gb"`
	storage.CamUsage
}

var (
	usageMu    sync.Mutex
	usageAt    time.Time
	usageCache []camStorage
)

// storageInfo reports free space, where the recordings live, and per-camera usage.
// Walking the folders is slow on an SD card, so the result is cached briefly.
func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	cams, err := s.DB.Cameras()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	usageMu.Lock()
	if time.Since(usageAt) > 30*time.Second || len(usageCache) != len(cams) {
		usageCache = usageCache[:0]
		for _, c := range cams {
			usageCache = append(usageCache, camStorage{c.ID, c.Name, c.MaxGB, storage.CameraUsage(s.RecDir, c.ID, s.DB.ProtectedClips(c.ID))})
		}
		usageAt = time.Now()
	}
	list := append([]camStorage(nil), usageCache...)
	usageMu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Bytes > list[j].Bytes })
	var rate, total int64
	for _, c := range list {
		rate += c.Last24hBytes
		total += c.Bytes
	}
	jsonOut(w, 200, map[string]any{
		"disk": storage.Usage(s.RecDir), "mount": storage.Mount(s.RecDir),
		"cameras": list, "recordings_bytes": total, "per_day_bytes": rate,
		"min_free_pct": s.DB.Int("min_free_pct", 5),
	})
}
