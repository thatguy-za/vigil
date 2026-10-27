// Package recorder supervises one ffmpeg process per camera, writing
// 5-minute, clock-aligned fragmented MP4 segments.
package recorder

import (
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
}

type job struct {
	sig       string
	cancel    context.CancelFunc
	done      chan struct{}
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	lastErr   string
}

type Manager struct {
	db   *db.DB
	dir  string
	enc  hw.Encoder
	mu   sync.Mutex
	jobs map[int64]*job
	kick chan struct{}
}

func New(d *db.DB, recDir string, enc hw.Encoder) *Manager {
	return &Manager{db: d, dir: recDir, enc: enc, jobs: map[int64]*job{}, kick: make(chan struct{}, 1)}
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

func wanted(c db.Camera, now time.Time) bool {
	switch c.Mode {
	case "continuous":
		return true
	case "schedule":
		i := int(now.Weekday())*24 + now.Hour()
		return len(c.Schedule) == 168 && c.Schedule[i] == '1'
	}
	return false
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
		sig := strings.Join(m.args(c), "\x00")
		m.mu.Lock()
		j := m.jobs[c.ID]
		m.mu.Unlock()
		if wanted(c, now) {
			keep[c.ID] = true
			if j != nil && j.sig != sig {
				m.stop(c.ID)
				j = nil
			}
			if j == nil {
				m.start(ctx, c, sig)
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
	j := &job{sig: sig, cancel: cancel, done: make(chan struct{})}
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
		if c.Mode == "motion" {
			return Status{State: "idle", Error: "Motion detection is not available yet"}
		}
		return Status{State: "idle"}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.running && time.Since(j.startedAt) > 5*time.Second:
		return Status{State: "recording"}
	case j.running:
		return Status{State: "connecting"}
	}
	return Status{State: "error", Error: j.lastErr}
}

func (m *Manager) loop(ctx context.Context, c db.Camera, j *job) {
	defer close(j.done)
	backoff := 3 * time.Second
	for ctx.Err() == nil {
		os.MkdirAll(CameraDir(m.dir, c.ID), 0o755)
		cmd := exec.CommandContext(ctx, "ffmpeg", m.args(c)...)
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 8 * time.Second
		var tail tailBuf
		cmd.Stderr = &tail
		j.mu.Lock()
		j.running, j.startedAt = true, time.Now()
		j.mu.Unlock()
		start := time.Now()
		err := cmd.Run()
		j.mu.Lock()
		j.running = false
		if ctx.Err() == nil {
			j.lastErr = tail.last()
			if j.lastErr == "" && err != nil {
				j.lastErr = redact(err.Error())
			}
		}
		j.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
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
	a := []string{"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-rtsp_transport", "tcp", "-i", c.URL, "-map", "0:v:0"}
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
	return append(a, "-f", "segment", "-segment_time", fmt.Sprint(SegmentSeconds),
		"-segment_atclocktime", "1", "-reset_timestamps", "1", "-strftime", "1",
		"-segment_format", "mp4",
		"-segment_format_options", "movflags=+frag_keyframe+empty_moov+default_base_moof",
		filepath.Join(CameraDir(m.dir, c.ID), "%Y-%m-%d_%H-%M-%S.mp4"))
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
