package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/hw"
	"github.com/thatguy-za/vigil/internal/live"
	"github.com/thatguy-za/vigil/internal/recorder"
	"github.com/thatguy-za/vigil/internal/storage"
	"github.com/thatguy-za/vigil/web"
)

type Server struct {
	DB      *db.DB
	Rec     *recorder.Manager
	Live    *live.Hub
	HW      hw.Info
	RecDir  string
	Version string
	started time.Time
}

type cameraView struct {
	db.Camera
	Status recorder.Status `json:"status"`
}

func (s *Server) Handler() http.Handler {
	s.started = time.Now()
	m := http.NewServeMux()
	m.HandleFunc("GET /api/system", s.system)
	m.HandleFunc("GET /api/cameras", s.list)
	m.HandleFunc("POST /api/cameras", s.save)
	m.HandleFunc("POST /api/cameras/test", s.test)
	m.HandleFunc("GET /api/cameras/{id}", s.get)
	m.HandleFunc("PUT /api/cameras/{id}", s.save)
	m.HandleFunc("DELETE /api/cameras/{id}", s.remove)
	m.HandleFunc("GET /api/cameras/{id}/snapshot", s.snapshot)
	m.HandleFunc("GET /api/cameras/{id}/recordings", s.recordings)
	m.HandleFunc("GET /files/", s.file)
	m.Handle("/", http.FileServerFS(web.Public()))
	return m
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) { jsonOut(w, code, map[string]string{"error": msg}) }

func (s *Server) camera(w http.ResponseWriter, r *http.Request) (db.Camera, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := s.DB.Camera(id)
	if err != nil {
		fail(w, 404, "camera not found")
		return c, false
	}
	return c, true
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	jsonOut(w, 200, map[string]any{
		"version": s.Version, "uptime_s": int(time.Since(s.started).Seconds()),
		"hw": s.HW, "disk": storage.Usage(s.RecDir), "vigil_mem_mb": ms.Sys >> 20,
		"segment_s": recorder.SegmentSeconds,
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	cams, err := s.DB.Cameras()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	out := make([]cameraView, len(cams))
	for i, c := range cams {
		out[i] = cameraView{c, s.Rec.Status(c)}
	}
	jsonOut(w, 200, out)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	if c, ok := s.camera(w, r); ok {
		jsonOut(w, 200, cameraView{c, s.Rec.Status(c)})
	}
}

var scheduleRe = regexp.MustCompile(`^[01]{168}$`)

func validURL(u string, optional bool) bool {
	if u == "" {
		return optional
	}
	p, err := url.Parse(u)
	return err == nil && p.Host != "" && (p.Scheme == "rtsp" || p.Scheme == "rtsps" || p.Scheme == "http" || p.Scheme == "https")
}

func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	var c db.Camera
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&c); err != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	c.Name, c.URL, c.SubURL = strings.TrimSpace(c.Name), strings.TrimSpace(c.URL), strings.TrimSpace(c.SubURL)
	switch {
	case c.Name == "":
		fail(w, 400, "name is required")
		return
	case !validURL(c.URL, false):
		fail(w, 400, "stream URL must start with rtsp:// (or http://, https://)")
		return
	case !validURL(c.SubURL, true):
		fail(w, 400, "sub-stream URL must start with rtsp:// (or http://, https://)")
		return
	case c.Mode != "off" && c.Mode != "continuous" && c.Mode != "schedule" && c.Mode != "motion":
		fail(w, 400, "invalid mode")
		return
	case c.Mode == "schedule" && !scheduleRe.MatchString(c.Schedule):
		fail(w, 400, "invalid schedule")
		return
	case c.MaxHeight < 0 || c.MaxHeight > 2160 || c.RetentionDays < 0 || c.RetentionDays > 3650:
		fail(w, 400, "value out of range")
		return
	}
	if r.Method == http.MethodPut {
		c.ID, _ = strconv.ParseInt(r.PathValue("id"), 10, 64)
		if _, err := s.DB.Camera(c.ID); err != nil {
			fail(w, 404, "camera not found")
			return
		}
		if err := s.DB.UpdateCamera(c); err != nil {
			fail(w, 500, err.Error())
			return
		}
	} else {
		id, err := s.DB.CreateCamera(c)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		c.ID = id
	}
	s.Rec.Reconcile()
	jsonOut(w, 200, c)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	if err := s.DB.DeleteCamera(c.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.Rec.Reconcile()
	if r.URL.Query().Get("purge") == "1" {
		os.RemoveAll(recorder.CameraDir(s.RecDir, c.ID))
	}
	w.WriteHeader(204)
}

func (s *Server) test(w http.ResponseWriter, r *http.Request) {
	var b struct{ URL string `json:"url"` }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&b) != nil || !validURL(b.URL, false) {
		fail(w, 400, "enter a valid rtsp:// URL")
		return
	}
	img, err := recorder.Snapshot(r.Context(), b.URL)
	if err != nil {
		fail(w, 422, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(img)
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	img, err := s.Live.Frame(c)
	if err != nil {
		fail(w, 503, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(img)
}

type clip struct {
	Path  string    `json:"path"`
	Start time.Time `json:"start"`
	Bytes int64     `json:"bytes"`
	Live  bool      `json:"live"`
}

var clipRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})_(\d{2}-\d{2}-\d{2})\.mp4$`)

func (s *Server) recordings(w http.ResponseWriter, r *http.Request) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	dir := recorder.CameraDir(s.RecDir, c.ID)
	files, _ := os.ReadDir(dir)
	seen := map[string]bool{}
	dates := []string{}
	byDate := map[string][]clip{}
	for _, f := range files {
		m := clipRe.FindStringSubmatch(f.Name())
		info, err := f.Info()
		if m == nil || err != nil {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02 15-04-05", m[1]+" "+m[2], time.Local)
		if err != nil {
			continue
		}
		if !seen[m[1]] {
			seen[m[1]] = true
			dates = append(dates, m[1])
		}
		byDate[m[1]] = append(byDate[m[1]], clip{
			Path: "/files/" + filepath.Base(dir) + "/" + f.Name(),
			Start: t, Bytes: info.Size(), Live: time.Since(info.ModTime()) < 30*time.Second,
		})
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	date := r.URL.Query().Get("date")
	if date == "" && len(dates) > 0 {
		date = dates[0]
	}
	clips := byDate[date]
	if clips == nil {
		clips = []clip{}
	}
	sort.Slice(clips, func(i, j int) bool { return clips[i].Start.After(clips[j].Start) })
	jsonOut(w, 200, map[string]any{"dates": dates, "date": date, "clips": clips})
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	rel := filepath.Clean("/" + strings.TrimPrefix(r.URL.Path, "/files/"))
	if !strings.HasSuffix(rel, ".mp4") {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.RecDir, rel))
}
