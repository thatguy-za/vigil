package api

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thatguy-za/vigil/internal/recorder"
)

const maxExport = 2 * time.Hour

// One export at a time: copying is cheap, but a Pi has one SD card or disk to read from.
var exportSem = make(chan struct{}, 1)

type span struct {
	path       string
	start, end time.Time
	guess      bool // end is assumed to be a full session, not known
}

// clipGuesses ends each guessed span where the next file starts. A recorder that restarted
// leaves a short file, and counting it as 5 minutes would make it overlap the next one.
func clipGuesses(spans []span) {
	for i := 0; i+1 < len(spans); i++ {
		if spans[i].guess && spans[i].end.After(spans[i+1].start) {
			spans[i].end = spans[i+1].start
		}
	}
}

// spansIn lists the recorded files that overlap [from, to], oldest first.
func (s *Server) spansIn(id int64, from, to time.Time) []span {
	dir := recorder.CameraDir(s.RecDir, id)
	files, _ := os.ReadDir(dir)
	durs := map[string]float64{}
	if evs, err := s.DB.Events(id, from.Add(-time.Hour), to.Add(time.Hour)); err == nil {
		for _, ev := range evs {
			if ev.Clip != "" {
				durs[ev.Clip] = ev.ClipDur
			}
		}
	}
	var out []span
	for _, f := range files {
		m := clipRe.FindStringSubmatch(f.Name())
		if m == nil {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02 15-04-05", m[1]+" "+m[2], time.Local)
		if err != nil {
			continue
		}
		dur := durs[f.Name()]
		guess := dur == 0
		if dur == 0 {
			dur = recorder.SegmentSeconds
			if info, err := f.Info(); err == nil && time.Since(info.ModTime()) < 30*time.Second {
				dur = min(dur, time.Since(t).Seconds())
			}
		}
		end := t.Add(time.Duration(dur * float64(time.Second)))
		out = append(out, span{filepath.Join(dir, f.Name()), t, end, guess})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	clipGuesses(out)
	var in []span
	for _, sp := range out {
		if sp.end.After(from) && sp.start.Before(to) {
			in = append(in, sp)
		}
	}
	return in
}

// concatList builds an ffmpeg concat script that trims the first and last file to [from, to].
func concatList(spans []span, from, to time.Time) string {
	var b strings.Builder
	for _, sp := range spans {
		fmt.Fprintf(&b, "file 'file:%s'\n", sp.path) // "file:" stops ffmpeg treating the names as part of the pipe we read the list from
		if sp.start.Before(from) {
			fmt.Fprintf(&b, "inpoint %.2f\n", from.Sub(sp.start).Seconds())
		}
		if sp.end.After(to) {
			fmt.Fprintf(&b, "outpoint %.2f\n", to.Sub(sp.start).Seconds())
		}
	}
	return b.String()
}

// export streams one MP4 covering ?from=&to= (unix seconds), joined from the camera's files.
// Nothing is written to disk: a two-hour range can be several GB.
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f, err1 := strconv.ParseInt(q.Get("from"), 10, 64)
	t, err2 := strconv.ParseInt(q.Get("to"), 10, 64)
	from, to := time.Unix(f, 0), time.Unix(t, 0)
	if err1 != nil || err2 != nil || !to.After(from) {
		fail(w, 400, "choose a start and an end, with the end after the start")
		return
	}
	if to.Sub(from) > maxExport {
		fail(w, 400, "an export can cover at most 2 hours")
		return
	}
	spans := s.spansIn(c.ID, from, to)
	if len(spans) == 0 {
		fail(w, 404, "nothing was recorded in that time")
		return
	}
	select {
	case exportSem <- struct{}{}:
		defer func() { <-exportSem }()
	default:
		fail(w, 409, "another export is running, try again in a minute")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "concat", "-safe", "0", "-protocol_whitelist", "file,pipe", "-i", "pipe:0",
		"-c", "copy", "-avoid_negative_ts", "make_zero",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	cmd.Stdin = strings.NewReader(concatList(spans, from, to))
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s_%s_to_%s.mp4"`,
		slug(c.Name), from.Format("2006-01-02_15-04-05"), to.Format("15-04-05")))
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		// Headers are already sent if any video went out; nothing more to tell the client.
		log.Printf("export %q: %v: %s", c.Name, err, strings.TrimSpace(stderr.String()))
	}
}
