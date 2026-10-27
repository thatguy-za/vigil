package recorder

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/motion"
)

const (
	analysisFPS   = 5
	bufSegSeconds = 5
	preRoll       = 5 * time.Second
	postRoll      = 10 * time.Second
	startFrames   = 2 // consecutive motion frames needed to start an event
	bufTimeFmt    = "20060102-150405"
)

func (m *Manager) bufDir(id int64) string { return filepath.Join(m.buf, fmt.Sprintf("cam%d", id)) }

// motionArgs builds one ffmpeg process that (1) copies the stream into short
// buffer segments and (2) decodes a small grayscale stream for analysis to stdout.
func (m *Manager) motionArgs(c db.Camera, hwDecode bool) []string {
	a := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-rtsp_transport", "tcp", "-i", c.URL}
	analysis := "0:v:0"
	if c.SubURL != "" {
		a = append(a, "-rtsp_transport", "tcp")
		analysis = "1:v:0"
	}
	if hwDecode {
		a = append(a, "-c:v", "h264_v4l2m2m")
	}
	if c.SubURL != "" {
		a = append(a, "-i", c.SubURL)
	}
	a = append(a, "-map", "0:v:0")
	a = append(a, m.codecArgs(c)...)
	a = append(a, "-f", "segment", "-segment_time", fmt.Sprint(bufSegSeconds), "-segment_atclocktime", "1",
		"-reset_timestamps", "1", "-strftime", "1", "-segment_format", "mp4",
		"-segment_format_options", "movflags=+frag_keyframe+empty_moov+default_base_moof",
		filepath.Join(m.bufDir(c.ID), "%Y%m%d-%H%M%S.mp4"))
	return append(a, "-map", analysis, "-an", "-vf",
		fmt.Sprintf("fps=%d,scale=%d:%d,format=gray", analysisFPS, motion.W, motion.H), "-f", "rawvideo", "pipe:1")
}

func (m *Manager) runMotion(ctx context.Context, c db.Camera, j *job, tail *tailBuf) error {
	dir := m.bufDir(c.ID)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	j.mu.Lock()
	useHW := m.hwDec && !j.noHW
	j.mu.Unlock()
	cmd := exec.CommandContext(ctx, "ffmpeg", m.motionArgs(c, useHW)...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 8 * time.Second
	cmd.Stderr = tail
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	ev := &events{m: m, c: c, j: j, dir: dir, pending: map[time.Time]bool{}}
	jctx, stopJanitor := context.WithCancel(ctx)
	go ev.janitor(jctx)

	det, buf := motion.New(c.MotionSensitivity, c.MotionMask), make([]byte, motion.W*motion.H)
	for {
		if _, err := io.ReadFull(out, buf); err != nil {
			break
		}
		ev.frame(det.Feed(buf), det.Pct, time.Now())
	}
	err = cmd.Wait()
	ev.close(time.Now())
	stopJanitor()
	if err != nil && useHW && time.Since(start) < 20*time.Second {
		log.Printf("recorder: hardware decode failed for %q, using software", c.Name)
		j.mu.Lock()
		j.noHW = true
		j.mu.Unlock()
	}
	return err
}

type events struct {
	m   *Manager
	c   db.Camera
	j   *job
	dir string

	consec     int
	active     bool
	start      time.Time
	lastMotion time.Time
	peak       float64

	mu      sync.Mutex
	pending map[time.Time]bool // events waiting to be assembled, keyed by start
	wg      sync.WaitGroup
}

func (e *events) setActive(v bool) {
	e.j.mu.Lock()
	e.j.motion = v
	e.j.mu.Unlock()
}

func (e *events) begin(now time.Time, pct float64) {
	e.active, e.start, e.peak = true, now, pct
	e.setActive(true)
	e.mu.Lock()
	e.pending[e.start] = true
	e.mu.Unlock()
}

func (e *events) frame(moving bool, pct float64, now time.Time) {
	if moving {
		e.consec++
		e.lastMotion = now
	} else {
		e.consec = 0
	}
	if !e.active && e.consec >= startFrames {
		e.begin(now, pct)
	}
	if !e.active {
		return
	}
	if moving && pct > e.peak {
		e.peak = pct
	}
	// One clip never exceeds a recording session (5 minutes, pre-roll included).
	tooLong := now.Sub(e.start) >= SegmentSeconds*time.Second-preRoll
	if now.Sub(e.lastMotion) > postRoll || tooLong {
		e.finish(e.start, e.lastMotion, now, e.peak)
		e.active = false
		e.setActive(false)
		if tooLong && moving {
			e.begin(now, pct)
		}
	}
}

func (e *events) close(now time.Time) {
	if e.active {
		e.finish(e.start, e.lastMotion, now, e.peak)
		e.active = false
		e.setActive(false)
	}
	e.wg.Wait()
}

// finish assembles the clip for an event in the background and records it.
// lastMotion is when movement was last seen; end is when the clip should stop.
func (e *events) finish(start, lastMotion, end time.Time, peak float64) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer func() {
			e.mu.Lock()
			delete(e.pending, start)
			e.mu.Unlock()
		}()
		// Wait for the segment containing `end` to be closed by ffmpeg.
		for i := 0; i < 20; i++ {
			segs := e.segments()
			if n := len(segs); n > 0 && segs[n-1].start.After(end) {
				break
			}
			time.Sleep(time.Second)
		}
		clip, dur, err := e.assemble(start, end)
		if err != nil {
			log.Printf("recorder: %q motion clip: %v", e.c.Name, err)
		}
		if lastMotion.Before(start) {
			lastMotion = start
		}
		if err := e.m.db.AddEvent(db.Event{CameraID: e.c.ID, Start: start, End: lastMotion, Peak: peak, Clip: clip, ClipDur: dur}); err != nil {
			log.Printf("recorder: %q save event: %v", e.c.Name, err)
		}
	}()
}

type seg struct {
	start time.Time
	path  string
}

func (e *events) segments() []seg {
	files, _ := os.ReadDir(e.dir)
	var out []seg
	for _, f := range files {
		t, err := time.ParseInLocation(bufTimeFmt, strings.TrimSuffix(f.Name(), ".mp4"), time.Local)
		if err == nil && strings.HasSuffix(f.Name(), ".mp4") {
			out = append(out, seg{t, filepath.Join(e.dir, f.Name())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

// assemble joins the buffered segments covering [start-preRoll, end] into one
// clip in the camera folder and returns its file name and duration.
func (e *events) assemble(start, end time.Time) (string, float64, error) {
	from := start.Add(-preRoll)
	var use []seg
	for _, s := range e.segments() {
		if s.start.After(end) {
			break
		}
		if s.start.After(from) {
			use = append(use, s)
		} else {
			use = []seg{s} // last segment starting at/before the pre-roll point
		}
	}
	if len(use) == 0 {
		return "", 0, fmt.Errorf("no buffered segments for event at %s", start.Format(time.TimeOnly))
	}
	list := strings.Builder{}
	for _, s := range use {
		fmt.Fprintf(&list, "file '%s'\n", s.path)
	}
	listPath := filepath.Join(e.dir, "ev-"+start.Format(bufTimeFmt)+".txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o644); err != nil {
		return "", 0, err
	}
	defer os.Remove(listPath)

	camDir := CameraDir(e.m.dir, e.c.ID)
	os.MkdirAll(camDir, 0o755)
	base := use[0].start.Format("2006-01-02_15-04-05") + ".mp4"
	name := filepath.Join(camDir, base)
	part := name + ".part"
	var tail tailBuf
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-f", "concat", "-safe", "0",
		"-i", listPath, "-c", "copy", "-movflags", "+faststart", "-f", "mp4", part)
	cmd.Stderr = &tail
	if err := cmd.Run(); err != nil {
		os.Remove(part)
		return "", 0, fmt.Errorf("concat: %s", tail.last())
	}
	if err := os.Rename(part, name); err != nil {
		return "", 0, err
	}
	return base, probeDuration(name, end.Sub(use[0].start)), nil
}

func probeDuration(path string, fallback time.Duration) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if d, perr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil && perr == nil {
		return d
	}
	return fallback.Seconds()
}

// janitor deletes buffer segments that no pending or active event can need.
func (e *events) janitor(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cutoff := time.Now().Add(-(preRoll + 2*bufSegSeconds*time.Second + 20*time.Second))
		e.mu.Lock()
		for s := range e.pending {
			if p := s.Add(-(preRoll + 2*bufSegSeconds*time.Second + 20*time.Second)); p.Before(cutoff) {
				cutoff = p
			}
		}
		e.mu.Unlock()
		for _, s := range e.segments() {
			if s.start.Before(cutoff) {
				os.Remove(s.path)
			}
		}
	}
}
