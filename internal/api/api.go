package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thatguy-za/vigil/internal/auth"
	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/discover"
	"github.com/thatguy-za/vigil/internal/hw"
	"github.com/thatguy-za/vigil/internal/live"
	"github.com/thatguy-za/vigil/internal/onvif"
	"github.com/thatguy-za/vigil/internal/recorder"
	"github.com/thatguy-za/vigil/internal/storage"
	"github.com/thatguy-za/vigil/web"
)

type Server struct {
	DB      *db.DB
	Rec     *recorder.Manager
	Live    *live.Hub
	Scan    *discover.Scanner
	HW      hw.Info
	RecDir  string
	Version string
	started time.Time
	lim     *auth.Limiter
	userMu  sync.Mutex // serialises admin changes so the last admin can't be removed
}

type cameraView struct {
	db.Camera
	Status recorder.Status `json:"status"`
}

func (s *Server) Handler() http.Handler {
	s.started = time.Now()
	s.lim = auth.NewLimiter(8, 10*time.Minute)
	m := http.NewServeMux()
	user := func(h http.HandlerFunc) http.HandlerFunc { return s.require(false, h) }
	admin := func(h http.HandlerFunc) http.HandlerFunc { return s.require(true, h) }
	m.HandleFunc("GET /api/auth/status", s.authStatus)
	m.HandleFunc("POST /api/auth/setup", s.open(s.setup))
	m.HandleFunc("POST /api/auth/login", s.open(s.login))
	m.HandleFunc("POST /api/auth/logout", s.open(s.logout))
	m.HandleFunc("POST /api/auth/password", user(s.changePassword))
	m.HandleFunc("GET /api/users", admin(s.listUsers))
	m.HandleFunc("POST /api/users", admin(s.createUser))
	m.HandleFunc("PUT /api/users/{id}", admin(s.updateUser))
	m.HandleFunc("DELETE /api/users/{id}", admin(s.deleteUser))
	m.HandleFunc("GET /api/system", admin(s.system))
	m.HandleFunc("GET /api/cameras", user(s.list))
	m.HandleFunc("POST /api/cameras", admin(s.save))
	m.HandleFunc("POST /api/cameras/test", admin(s.test))
	m.HandleFunc("GET /api/cameras/{id}", user(s.get))
	m.HandleFunc("PUT /api/cameras/{id}", admin(s.save))
	m.HandleFunc("DELETE /api/cameras/{id}", admin(s.remove))
	m.HandleFunc("GET /api/cameras/{id}/snapshot", user(s.snapshot))
	m.HandleFunc("GET /api/cameras/{id}/recordings", user(s.recordings))
	m.HandleFunc("GET /api/cameras/{id}/events", user(s.events))
	m.HandleFunc("GET /api/discovery", admin(s.discovered))
	m.HandleFunc("POST /api/discovery/scan", admin(s.scan))
	m.HandleFunc("POST /api/discovery/{id}/dismiss", admin(s.dismiss))
	m.HandleFunc("POST /api/discovery/{id}/resolve", admin(s.resolve))
	m.HandleFunc("GET /files/", user(s.file))
	m.Handle("/", http.FileServerFS(web.Public()))
	return m
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}

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
		out[i] = s.cameraFor(r, c)
	}
	jsonOut(w, 200, out)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	if c, ok := s.camera(w, r); ok {
		jsonOut(w, 200, s.cameraFor(r, c))
	}
}

var (
	scheduleRe = regexp.MustCompile(`^[01]{168}$`)
	maskRe     = regexp.MustCompile(`^[01]{144}$`) // motion.MaskCols * motion.MaskRows
)

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
	case c.MotionMask != "" && !maskRe.MatchString(c.MotionMask):
		fail(w, 400, "invalid motion mask")
		return
	case c.MotionSensitivity < 0 || c.MotionSensitivity > 10:
		fail(w, 400, "sensitivity must be 1-10")
		return
	case c.MaxHeight < 0 || c.MaxHeight > 2160 || c.RetentionDays < 0 || c.RetentionDays > 3650:
		fail(w, 400, "value out of range")
		return
	}
	if c.MotionSensitivity == 0 {
		c.MotionSensitivity = 5
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
	if id := r.URL.Query().Get("discovery"); id != "" && r.Method == http.MethodPost {
		s.DB.LinkDiscovered(id, c.ID)
	}
	for _, h := range []string{hostOf(c.URL), hostOf(c.SubURL)} {
		if h != "" {
			s.DB.MarkAdded(h)
		}
	}
	s.Rec.Reconcile()
	jsonOut(w, 200, c)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	for _, h := range []string{hostOf(c.URL), hostOf(c.SubURL)} {
		if h != "" {
			s.DB.ReleaseAdded(h)
		}
	}
	s.DB.ReleaseCamera(c.ID)
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
	var b struct {
		URL string `json:"url"`
	}
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
	Dur   float64   `json:"duration"` // seconds
}

var clipRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})_(\d{2}-\d{2}-\d{2})\.mp4$`)

func (s *Server) recordings(w http.ResponseWriter, r *http.Request) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	dir := recorder.CameraDir(s.RecDir, c.ID)
	files, _ := os.ReadDir(dir)
	durs := map[string]float64{} // motion clips have their own length
	if evs, err := s.DB.Events(c.ID, time.Unix(0, 0), time.Now().Add(24*time.Hour)); err == nil {
		for _, ev := range evs {
			if ev.Clip != "" {
				durs[ev.Clip] = ev.ClipDur
			}
		}
	}
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
		live := time.Since(info.ModTime()) < 30*time.Second
		dur := durs[f.Name()]
		if dur == 0 {
			dur = recorder.SegmentSeconds
			if live {
				dur = min(dur, time.Since(t).Seconds())
			}
		}
		byDate[m[1]] = append(byDate[m[1]], clip{
			Path:  "/files/" + filepath.Base(dir) + "/" + f.Name(),
			Start: t, Bytes: info.Size(), Live: live, Dur: dur,
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

type eventView struct {
	ID    int64     `json:"id"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Peak  float64   `json:"peak"`
	Clip  string    `json:"clip"` // playable path, "" if the clip has been deleted
}

// events lists a camera's motion events for one local day (?date=YYYY-MM-DD).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	day, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("date"), time.Local)
	if err != nil {
		fail(w, 400, "date must be YYYY-MM-DD")
		return
	}
	evs, err := s.DB.Events(c.ID, day, day.AddDate(0, 0, 1))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	dir := recorder.CameraDir(s.RecDir, c.ID)
	out := make([]eventView, len(evs))
	for i, e := range evs {
		out[i] = eventView{e.ID, e.Start, e.End, e.Peak, ""}
		if e.Clip != "" {
			if _, err := os.Stat(filepath.Join(dir, e.Clip)); err == nil {
				out[i].Clip = "/files/" + filepath.Base(dir) + "/" + e.Clip
			}
		}
	}
	jsonOut(w, 200, out)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// discovered lists cameras found on the network that have not been added yet.
func (s *Server) discovered(w http.ResponseWriter, r *http.Request) {
	list, err := s.DB.Discovered()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	jsonOut(w, 200, list)
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.Scan.Scan(ctx); err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.discovered(w, r)
}

func (s *Server) dismiss(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.DismissDiscovered(r.PathValue("id")); err != nil {
		fail(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// resolve logs in to a discovered camera over ONVIF and returns its stream URLs.
// The address comes from the database, never from the request.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	d, err := s.DB.DiscoveredByID(r.PathValue("id"))
	if err != nil {
		fail(w, 404, "camera not found")
		return
	}
	var b struct{ Username, Password string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&b) != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	mainURL, subURL, err := onvif.Resolve(r.Context(), d.XAddr, b.Username, b.Password)
	switch {
	case errors.Is(err, onvif.ErrAuth):
		fail(w, 401, "Wrong username or password")
	case err != nil:
		fail(w, 502, err.Error())
	default:
		jsonOut(w, 200, map[string]string{"name": d.Name, "url": mainURL, "sub_url": subURL})
	}
}
