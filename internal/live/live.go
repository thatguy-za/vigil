// Package live serves live pictures and video from cameras. This file keeps a
// low-rate JPEG preview per camera: one ffmpeg process decodes only the
// keyframes of the sub-stream while someone is looking, and stops after a short
// idle period, so previews cost almost nothing. stream.go does live video.
package live

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
)

const idle = 20 * time.Second

type feed struct {
	url     string
	cancel  context.CancelFunc
	mu      sync.Mutex
	frame   []byte
	lastReq time.Time
}

type Hub struct {
	ctx   context.Context
	mu    sync.Mutex
	feeds map[int64]*feed

	smu     sync.Mutex // guards streams (see stream.go)
	streams map[int64]*stream
}

func New(ctx context.Context) *Hub {
	return &Hub{ctx: ctx, feeds: map[int64]*feed{}, streams: map[int64]*stream{}}
}

func (h *Hub) Frame(c db.Camera) ([]byte, error) {
	url := c.SubURL
	if url == "" {
		url = c.URL
	}
	h.mu.Lock()
	f := h.feeds[c.ID]
	if f != nil && f.url != url {
		f.cancel()
		f = nil
	}
	if f == nil {
		ctx, cancel := context.WithCancel(h.ctx)
		f = &feed{url: url, cancel: cancel}
		h.feeds[c.ID] = f
		go f.run(ctx)
	}
	h.mu.Unlock()

	f.mu.Lock()
	f.lastReq = time.Now()
	f.mu.Unlock()
	for i := 0; i < 100; i++ {
		f.mu.Lock()
		fr := f.frame
		f.mu.Unlock()
		if fr != nil {
			return fr, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New("no frame yet")
}

func (f *feed) run(ctx context.Context) {
	defer f.cancel()
	for ctx.Err() == nil {
		f.mu.Lock()
		expired := time.Since(f.lastReq) > idle
		f.mu.Unlock()
		if expired {
			return
		}
		f.once(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func (f *feed) once(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-rtsp_transport", "tcp", "-skip_frame", "nokey", "-i", f.url, "-an",
		"-vf", "scale=-2:360", "-fps_mode", "passthrough", "-q:v", "6", "-c:v", "mjpeg", "-f", "image2pipe", "pipe:1")
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return
	}
	defer cmd.Wait()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 256<<10), 4<<20)
	sc.Split(func(d []byte, eof bool) (int, []byte, error) {
		if i := bytes.Index(d, []byte{0xFF, 0xD9}); i >= 0 {
			return i + 2, d[:i+2], nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		fr := append([]byte(nil), sc.Bytes()...)
		f.mu.Lock()
		f.frame = fr
		expired := time.Since(f.lastReq) > idle
		f.mu.Unlock()
		if expired {
			return
		}
	}
}
