package recorder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/motion"
	"github.com/thatguy-za/vigil/internal/onvif"
)

const (
	analysisFPS   = 5
	bufSegSeconds = 5
	startFrames   = 2 // consecutive motion frames needed to start an event
	bufTimeFmt    = "20060102-150405"
)

func (m *Manager) bufDir(id int64) string { return filepath.Join(m.buf, fmt.Sprintf("cam%d", id)) }

// motionArgs builds one ffmpeg process that copies the stream into short buffer
// segments (the pre-roll). Depending on the motion source it also decodes a small
// grayscale stream for analysis to stdout, and with a sub-stream and c.LowRes it
// also records the sub-stream continuously, in low resolution.
func (m *Manager) motionArgs(c db.Camera, hwDecode bool) []string {
	analyse := c.MotionSource != "camera"
	a := []string{"-hide_banner", "-loglevel", "warning", "-nostdin"}
	if !analyse {
		a = append(a, "-progress", "pipe:1", "-nostats") // liveness; stdout carries no frames
	}
	decode := func() {
		if hwDecode && analyse {
			a = append(a, "-c:v", "h264_v4l2m2m") // applies to the input that follows
		}
	}
	needSub := c.SubURL != "" && (analyse || c.LowRes)
	analysis := "0:v:0"
	if !needSub {
		decode()
	}
	a = append(a, "-rtsp_transport", "tcp", "-i", c.URL)
	if needSub {
		analysis = "1:v:0"
		a = append(a, "-rtsp_transport", "tcp")
		decode()
		a = append(a, "-i", c.SubURL)
	}
	seg := []string{"-reset_timestamps", "1", "-strftime", "1", "-segment_format", "mp4",
		"-segment_format_options", "movflags=+frag_keyframe+empty_moov+default_base_moof"}
	a = append(a, "-map", "0:v:0")
	a = append(a, m.codecArgs(c)...)
	a = append(a, "-f", "segment", "-segment_time", fmt.Sprint(bufSegSeconds), "-segment_atclocktime", "1")
	a = append(a, seg...)
	a = append(a, filepath.Join(m.bufDir(c.ID), "%Y%m%d-%H%M%S.mp4"))
	if c.LowRes && c.SubURL != "" {
		a = append(a, "-map", "1:v:0", "-an", "-c:v", "copy", "-f", "segment", "-segment_time", fmt.Sprint(SegmentSeconds), "-segment_atclocktime", "1")
		a = append(a, seg...)
		a = append(a, filepath.Join(CameraDir(m.dir, c.ID), "low", "%Y-%m-%d_%H-%M-%S.mp4"))
	}
	if analyse {
		a = append(a, "-map", analysis, "-an", "-vf",
			fmt.Sprintf("fps=%d,scale=%d:%d,format=gray", analysisFPS, motion.W, motion.H), "-f", "rawvideo", "pipe:1")
	}
	return a
}

func (m *Manager) runMotion(ctx context.Context, c db.Camera, j *job, tail *tailBuf) error {
	dir := m.bufDir(c.ID)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if c.LowRes {
		os.MkdirAll(filepath.Join(CameraDir(m.dir, c.ID), "low"), 0o755)
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
	ev := &events{m: m, c: c, j: j, dir: dir, pending: map[time.Time]bool{},
		pre:  time.Duration(m.db.Int("preroll_s", 5)) * time.Second,
		post: time.Duration(m.db.Int("postroll_s", 10)) * time.Second}
	jctx, stopJanitor := context.WithCancel(ctx)
	go ev.janitor(jctx)

	// One goroutine feeds the event tracker at 5 Hz, merging Vigil's own analysis
	// with the camera's ONVIF events, so either (or both) can start an event.
	var vigilOn, camOn atomic.Bool
	var pctBits atomic.Uint64
	tctx, stopTicks := context.WithCancel(ctx)
	ticksDone := make(chan struct{})
	go func() {
		defer close(ticksDone)
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-tctx.Done():
				return
			case now := <-t.C:
				ev.frame(vigilOn.Load() || camOn.Load(), math.Float64frombits(pctBits.Load()), now)
			}
		}
	}()
	if c.MotionSource != "vigil" {
		go m.watchCamera(tctx, c, j, &camOn)
	}
	if c.MotionSource != "camera" {
		det, buf := motion.New(c.MotionSensitivity, c.MotionMask), make([]byte, motion.W*motion.H)
		for {
			if _, err := io.ReadFull(out, buf); err != nil {
				break
			}
			j.touch()
			vigilOn.Store(det.Feed(buf))
			pctBits.Store(math.Float64bits(det.Pct))
		}
	} else {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			j.touch()
		}
	}
	err = cmd.Wait()
	stopTicks()
	<-ticksDone
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

// watchCamera follows the camera's own ONVIF motion events until ctx ends,
// reconnecting with a back-off. Problems show up as a note on the camera.
func (m *Manager) watchCamera(ctx context.Context, c db.Camera, j *job, on *atomic.Bool) {
	u, _ := url.Parse(c.URL)
	user, pass := "", ""
	if u != nil && u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	setNote := func(s string) {
		j.mu.Lock()
		j.note = s
		j.mu.Unlock()
	}
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		xaddr := c.OnvifURL
		if xaddr == "" && u != nil {
			if found := onvif.FindDevice(ctx, u.Hostname(), onvif.DefaultPorts); len(found) > 0 {
				xaddr = found[0]
			}
		}
		var err error
		if xaddr == "" {
			err = errors.New("no ONVIF service found on the camera")
		} else {
			started := time.Now()
			err = onvif.WatchMotion(ctx, xaddr, user, pass, func() { setNote("") }, func(v bool) { on.Store(v) })
			if time.Since(started) > time.Minute {
				backoff = 5 * time.Second
			}
		}
		on.Store(false)
		if ctx.Err() != nil {
			return
		}
		setNote("Camera motion events unavailable: " + redact(err.Error()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}
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
	pre, post  time.Duration // seconds kept before the first and after the last movement
	closing    atomic.Bool   // ffmpeg has ended: don't wait for more segments to appear

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
	tooLong := now.Sub(e.start) >= SegmentSeconds*time.Second-e.pre
	if now.Sub(e.lastMotion) > e.post || tooLong {
		e.finish(e.start, e.lastMotion, now, e.peak)
		e.active = false
		e.setActive(false)
		if tooLong && moving {
			e.begin(now, pct)
		}
	}
}

func (e *events) close(now time.Time) {
	e.closing.Store(true)
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
		for i := 0; i < 20 && !e.closing.Load(); i++ {
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

// assemble joins the buffered segments covering [start-pre-roll, end] into one
// clip in the camera folder and returns its file name and duration.
func (e *events) assemble(start, end time.Time) (string, float64, error) {
	from := start.Add(-e.pre)
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
		cutoff := time.Now().Add(-(e.pre + 2*bufSegSeconds*time.Second + 20*time.Second))
		e.mu.Lock()
		for s := range e.pending {
			if p := s.Add(-(e.pre + 2*bufSegSeconds*time.Second + 20*time.Second)); p.Before(cutoff) {
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
