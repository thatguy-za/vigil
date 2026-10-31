// Package recorder supervises one ffmpeg process per camera, writing
// 5-minute, clock-aligned fragmented MP4 segments.
package recorder

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/hw"
)

const SegmentSeconds = 300

type Status struct {
	State string `json:"state"` // off | idle | connecting | recording | error
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"` // a warning that doesn't stop recording
}

type job struct {
	sig       string
	cancel    context.CancelFunc
	done      chan struct{}
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	lastErr   string
	mode      string // what this run is doing now: continuous or motion
	note      string
	noHW      bool // hardware decode failed once; use software for motion analysis
	motion    bool // an event is being recorded

	lastData  time.Time          // when output or frames last arrived from the camera
	health    string             // "", "up" or "down": what was last reported
	stalled   bool               // the watchdog killed ffmpeg for not producing data
	cancelRun context.CancelFunc // stops the current ffmpeg run only
}

func (j *job) touch() {
	j.mu.Lock()
	j.lastData = time.Now()
	j.mu.Unlock()
}

type Manager struct {
	db    *db.DB
	dir   string
	buf   string // scratch dir for motion pre-roll segments
	enc   hw.Encoder
	hwDec bool
	mu    sync.Mutex
	jobs  map[int64]*job
	kick  chan struct{}
}

func New(d *db.DB, recDir, bufDir string, enc hw.Encoder, hwDecode bool) *Manager {
	return &Manager{db: d, dir: recDir, buf: bufDir, enc: enc, hwDec: hwDecode, jobs: map[int64]*job{}, kick: make(chan struct{}, 1)}
}

func CameraDir(root string, id int64) string { return filepath.Join(root, fmt.Sprintf("cam%d", id)) }

// Reconcile asks the manager to re-evaluate cameras now (after an API change).
func (m *Manager) Reconcile() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		m.reconcile(ctx)
		select {
		case <-ctx.Done():
			m.stopAll()
			return
		case <-t.C:
		case <-m.kick:
		}
	}
}

// effective is what a camera should be doing right now: "continuous", "motion", or ""
// for nothing. A schedule can switch between the two hour by hour.
func effective(c db.Camera, now time.Time) string {
	switch c.Mode {
	case "continuous", "motion":
		return c.Mode
	case "schedule":
		if len(c.Schedule) != 168 {
			return ""
		}
		switch c.Schedule[int(now.Weekday())*24+now.Hour()] {
		case '1':
			return "continuous"
		case '2':
			return "motion"
		}
	}
	return ""
}

func (m *Manager) reconcile(ctx context.Context) {
	cams, err := m.db.Cameras()
	if err != nil {
		log.Printf("recorder: %v", err)
		return
	}
	now := time.Now()
	keep := map[int64]bool{}
	for _, c := range cams {
		eff := effective(c, now)
		ec := c // the camera as it should run this hour
		ec.Mode = eff
		sig := strings.Join(m.args(ec), "\x00") + fmt.Sprint(c.MotionSensitivity) + c.MotionMask + c.MotionSource + c.OnvifURL
		m.mu.Lock()
		j := m.jobs[c.ID]
		m.mu.Unlock()
		if eff != "" {
			keep[c.ID] = true
			if j != nil && j.sig != sig {
				m.stop(c.ID)
				j = nil
			}
			if j == nil {
				m.start(ctx, ec, sig)
			}
		}
	}
	m.mu.Lock()
	var drop []int64
	for id := range m.jobs {
		if !keep[id] {
			drop = append(drop, id)
		}
	}
	m.mu.Unlock()
	for _, id := range drop {
		m.stop(id)
	}
}

func (m *Manager) start(ctx context.Context, c db.Camera, sig string) {
	cctx, cancel := context.WithCancel(ctx)
	j := &job{sig: sig, cancel: cancel, done: make(chan struct{}), mode: c.Mode}
	m.mu.Lock()
	m.jobs[c.ID] = j
	m.mu.Unlock()
	log.Printf("recorder: start %q (id %d)", c.Name, c.ID)
	go m.loop(cctx, c, j)
}

func (m *Manager) stop(id int64) {
	m.mu.Lock()
	j := m.jobs[id]
	delete(m.jobs, id)
	m.mu.Unlock()
	if j == nil {
		return
	}
	j.cancel()
	select {
	case <-j.done:
	case <-time.After(15 * time.Second):
	}
	j.mu.Lock()
	was := j.health
	j.mu.Unlock()
	if was != "" {
		m.db.AddHealth(id, "stopped", "")
	}
	log.Printf("recorder: stopped id %d", id)
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	ids := make([]int64, 0, len(m.jobs))
	for id := range m.jobs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.stop(id)
	}
}

func (m *Manager) Status(c db.Camera) Status {
	if c.Mode == "off" {
		return Status{State: "off"}
	}
	m.mu.Lock()
	j := m.jobs[c.ID]
	m.mu.Unlock()
	if j == nil {
		return Status{State: "idle"}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	st := Status{State: "error", Error: j.lastErr, Note: j.note}
	switch {
	case j.running && j.health == "up" && j.mode == "motion" && !j.motion:
		st.State, st.Error = "watching", ""
	case j.running && j.health == "up":
		st.State, st.Error = "recording", ""
	case j.running && j.health == "down": // still retrying after an outage
	case j.running:
		st.State, st.Error = "connecting", ""
	}
	return st
}

// LastData is when the camera last delivered anything, and whether it is being recorded at all.
func (m *Manager) LastData(id int64) (time.Time, bool) {
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j == nil {
		return time.Time{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastData, true
}

const (
	stallAfter = 45 * time.Second // no data for this long and ffmpeg is restarted
	watchEvery = 3 * time.Second
)

// setHealth records a change between up and down (once per change).
func (m *Manager) setHealth(c db.Camera, j *job, to, detail string) {
	j.mu.Lock()
	if j.health == to {
		j.mu.Unlock()
		return
	}
	j.health = to
	j.mu.Unlock()
	kind := "online"
	if to == "down" {
		kind = "offline"
	}
	m.db.AddHealth(c.ID, kind, detail)
}

// watch marks the camera up once data arrives, and restarts ffmpeg if it hangs
// (a dead RTSP connection can leave the process alive but silent forever).
func (m *Manager) watch(ctx context.Context, c db.Camera, j *job) {
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		j.mu.Lock()
		running, started, last, cancel := j.running, j.startedAt, j.lastData, j.cancelRun
		j.mu.Unlock()
		if !running {
			continue
		}
		ref := started
		if last.After(started) {
			ref = last
			if time.Since(last) < stallAfter {
				m.setHealth(c, j, "up", "")
			}
		}
		if time.Since(ref) > stallAfter && cancel != nil {
			log.Printf("recorder: %q sent no data for %s, restarting", c.Name, stallAfter)
			j.mu.Lock()
			j.stalled = true
			j.mu.Unlock()
			m.setHealth(c, j, "down", fmt.Sprintf("No data for %d s, restarted", int(stallAfter.Seconds())))
			cancel()
		}
	}
}

func (m *Manager) loop(ctx context.Context, c db.Camera, j *job) {
	defer close(j.done)
	go m.watch(ctx, c, j)
	backoff := 3 * time.Second
	for ctx.Err() == nil {
		os.MkdirAll(CameraDir(m.dir, c.ID), 0o755)
		var tail tailBuf
		rctx, rcancel := context.WithCancel(ctx) // this run only; the watchdog can end it
		j.mu.Lock()
		j.running, j.startedAt, j.cancelRun = true, time.Now(), rcancel
		j.mu.Unlock()
		start := time.Now()
		var err error
		if c.Mode == "motion" {
			err = m.runMotion(rctx, c, j, &tail)
		} else {
			cmd := exec.CommandContext(rctx, "ffmpeg", m.args(c)...)
			cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
			cmd.WaitDelay = 8 * time.Second
			cmd.Stderr = &tail
			// -progress prints a line block every ~0.5 s while data flows; silence means a stall
			if out, perr := cmd.StdoutPipe(); perr == nil {
				go func() {
					sc := bufio.NewScanner(out)
					for sc.Scan() {
						j.touch()
					}
				}()
			}
			err = cmd.Run()
		}
		rcancel()
		j.mu.Lock()
		j.running = false
		stalled := j.stalled
		j.stalled = false
		msg := ""
		if ctx.Err() == nil {
			msg = tail.last()
			if msg == "" && err != nil {
				msg = redact(err.Error())
			}
			if stalled {
				msg = fmt.Sprintf("No data for %d s, restarted", int(stallAfter.Seconds()))
			}
			j.lastErr = msg
		}
		j.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		m.setHealth(c, j, "down", msg)
		if time.Since(start) > time.Minute {
			backoff = 3 * time.Second
		}
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

// args builds the ffmpeg command line. Copy mode remuxes the camera stream
// untouched; transcode mode re-encodes with the best encoder found at startup.
func (m *Manager) args(c db.Camera) []string {
	if c.Mode == "motion" {
		return m.motionArgs(c, m.hwDec)
	}
	a := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-progress", "pipe:1", "-nostats",
		"-rtsp_transport", "tcp", "-i", c.URL, "-map", "0:v:0"}
	a = append(a, m.codecArgs(c)...)
	return append(a, "-f", "segment", "-segment_time", fmt.Sprint(SegmentSeconds),
		"-segment_atclocktime", "1", "-reset_timestamps", "1", "-strftime", "1",
		"-segment_format", "mp4",
		"-segment_format_options", "movflags=+frag_keyframe+empty_moov+default_base_moof",
		filepath.Join(CameraDir(m.dir, c.ID), "%Y-%m-%d_%H-%M-%S.mp4"))
}

// codecArgs are the audio/video encoding options shared by both recording modes.
func (m *Manager) codecArgs(c db.Camera) []string {
	var a []string
	if c.Audio {
		a = append(a, "-map", "0:a:0?", "-c:a", "aac", "-b:a", "64k")
	} else {
		a = append(a, "-an")
	}
	if c.Transcode && m.enc.Name != "none" {
		if c.MaxHeight > 0 {
			a = append(a, "-vf", fmt.Sprintf("scale=-2:%d", c.MaxHeight))
		}
		switch m.enc.Name {
		case "h264_v4l2m2m":
			a = append(a, "-c:v", "h264_v4l2m2m", "-b:v", "2M", "-pix_fmt", "yuv420p", "-g", "50")
		case "libx264":
			a = append(a, "-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-threads", "2", "-pix_fmt", "yuv420p", "-g", "50")
		default:
			a = append(a, "-c:v", m.enc.Name, "-b:v", "2M", "-pix_fmt", "yuv420p", "-g", "50")
		}
	} else {
		a = append(a, "-c:v", "copy")
	}
	return a
}

var credRe = regexp.MustCompile(`://[^/@\s]*@`)

func redact(s string) string { return credRe.ReplaceAllString(s, "://***@") }

type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuf) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := bytes.Split(bytes.TrimSpace(t.b), []byte("\n"))
	return redact(string(lines[len(lines)-1]))
}

// Snapshot grabs one JPEG frame from a stream (used to test a new camera).
func Snapshot(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var tail tailBuf
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-rtsp_transport", "tcp", "-i", url, "-frames:v", "1", "-vf", "scale=-2:360",
		"-q:v", "5", "-f", "image2pipe", "-c:v", "mjpeg", "pipe:1")
	cmd.Stderr = &tail
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		msg := tail.last()
		if ctx.Err() != nil {
			msg = "timed out connecting to the stream"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return out, nil
}
