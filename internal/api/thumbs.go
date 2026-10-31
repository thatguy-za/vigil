package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/thatguy-za/vigil/internal/recorder"
)

// One thumbnail at a time: a Pi can't decode many frames at once and stay responsive.
var thumbSem = make(chan struct{}, 1)

// thumb returns a small JPEG from a clip at t seconds in (?clip=NAME&t=12[&low=1]).
// They are made on demand and cached in the camera's .thumbs folder.
func (s *Server) thumb(w http.ResponseWriter, r *http.Request) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	name, low := q.Get("clip"), q.Get("low") == "1"
	t, err := strconv.Atoi(q.Get("t"))
	if !clipRe.MatchString(name) || err != nil || t < 0 || t > 600 {
		fail(w, 400, "invalid thumbnail request")
		return
	}
	dir := recorder.CameraDir(s.RecDir, c.ID)
	src, cacheName := filepath.Join(dir, name), fmt.Sprintf("%s.%d.jpg", name, t)
	if low {
		src, cacheName = filepath.Join(dir, "low", name), "low."+cacheName
	}
	info, err := os.Stat(src)
	if err != nil {
		fail(w, 404, "clip not found")
		return
	}
	cache := filepath.Join(dir, ".thumbs", cacheName)
	w.Header().Set("Content-Type", "image/jpeg")
	if _, err := os.Stat(cache); err == nil {
		w.Header().Set("Cache-Control", "private, max-age=86400")
		http.ServeFile(w, r, cache)
		return
	}
	select {
	case thumbSem <- struct{}{}:
		defer func() { <-thumbSem }()
	case <-r.Context().Done():
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	img, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-ss", strconv.Itoa(t), "-i", src, "-frames:v", "1", "-vf", "scale=320:-2", "-q:v", "6", "-f", "image2", "pipe:1").Output()
	if err != nil || len(img) == 0 {
		fail(w, 422, "could not read a frame")
		return
	}
	if time.Since(info.ModTime()) > time.Minute { // don't cache clips still being written
		os.MkdirAll(filepath.Dir(cache), 0o755)
		os.WriteFile(cache, img, 0o644)
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	w.Write(img)
}
