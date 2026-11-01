package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/thatguy-za/vigil/internal/recorder"
)

type feedEvent struct {
	ID        int64     `json:"id"`
	CameraID  int64     `json:"camera_id"`
	Camera    string    `json:"camera"`
	Start     time.Time `json:"start"`
	Duration  float64   `json:"duration"`
	Peak      float64   `json:"peak"`
	Source    string    `json:"source"`
	Clip      string    `json:"clip"`      // playable path
	ClipName  string    `json:"clip_name"` // file name, for thumbnails
	Protected bool      `json:"protected"`
}

// feed lists motion events across cameras, newest first:
// ?camera=ID &type=vigil|camera|protected &days=7 &limit=100
func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cam, _ := strconv.ParseInt(q.Get("camera"), 10, 64)
	days, err := strconv.Atoi(q.Get("days"))
	if err != nil || days < 1 {
		days = 7
	}
	days = min(days, 365)
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 {
		limit = 100
	}
	limit = min(limit, 300)
	typ := q.Get("type")
	if typ != "" && typ != "vigil" && typ != "camera" && typ != "protected" {
		fail(w, 400, "unknown event type")
		return
	}
	cams, err := s.DB.Cameras()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	names := map[int64]string{}
	for _, c := range cams {
		names[c.ID] = c.Name
	}
	src := typ
	if typ == "protected" {
		src = ""
	}
	// Protected needs a second pass, so fetch more before filtering.
	fetch := limit
	if typ == "protected" {
		fetch = 1000
	}
	evs, err := s.DB.EventsSince(cam, time.Now().AddDate(0, 0, -days), src, fetch)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	prot := map[int64]map[string]bool{}
	out := []feedEvent{}
	for _, e := range evs {
		name, known := names[e.CameraID]
		if !known || len(out) >= limit {
			continue
		}
		if prot[e.CameraID] == nil {
			prot[e.CameraID] = s.DB.ProtectedClips(e.CameraID)
		}
		isProt := e.Clip != "" && prot[e.CameraID][e.Clip]
		if typ == "protected" && !isProt {
			continue
		}
		fe := feedEvent{ID: e.ID, CameraID: e.CameraID, Camera: name, Start: e.Start, Duration: e.ClipDur,
			Peak: e.Peak, Source: e.Source, Protected: isProt}
		if e.Clip != "" {
			dir := recorder.CameraDir(s.RecDir, e.CameraID)
			if _, err := os.Stat(filepath.Join(dir, e.Clip)); err == nil {
				fe.Clip, fe.ClipName = "/files/"+filepath.Base(dir)+"/"+e.Clip, e.Clip
			}
		}
		out = append(out, fe)
	}
	jsonOut(w, 200, out)
}
