package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
)

// ErrUnsupported means the camera's video is neither H.264 nor H.265, so a browser's
// Media Source can't play it.
var ErrUnsupported = errors.New("live video needs an H.264 or H.265 stream")

// ErrBusy means the Pi is already transcoding as many live streams as it should.
var ErrBusy = errors.New("too many streams are being converted for the browser, try again shortly")

// maxTranscodes bounds the live streams converted to H.264 at once.
const maxTranscodes = 2

type streamKey struct {
	id        int64
	transcode bool
}

// SetEncoder chooses the H.264 encoder used when a browser can't play a camera's H.265.
func (h *Hub) SetEncoder(name string) { h.encoder = name }

// stream is one ffmpeg per camera that copies the video into fragmented MP4.
// Every viewer shares it: each gets the init segment, then each fragment
// (one GOP, starting on a keyframe) as it arrives. It stops 10 s after the
// last viewer leaves.
type stream struct {
	url       string
	transcode bool // convert to H.264 (for browsers that can't play the camera's H.265)
	encoder   string
	cancel    context.CancelFunc

	mu        sync.Mutex
	subs      map[*Sub]struct{}
	init      []byte
	codec     string
	err       error
	idleTimer *time.Timer

	ready     chan struct{} // closed once the init segment is parsed, or on failure
	readyOnce sync.Once
}

// Sub is one viewer.
type Sub struct {
	C     chan []byte // fragments; closed when the stream ends or the viewer is too slow
	Init  []byte
	Codec string // e.g. avc1.640028, for MediaSource.addSourceBuffer
	s     *stream
}

// Subscribe joins the camera's live stream. With transcode set the video is converted to
// H.264 first; otherwise it is passed through as the camera sends it.
func (h *Hub) Subscribe(c db.Camera, transcode bool) (*Sub, error) {
	url := c.SubURL
	if url == "" {
		url = c.URL
	}
	key := streamKey{c.ID, transcode}
	h.smu.Lock()
	s := h.streams[key]
	if s != nil && s.url != url {
		s.cancel()
		s = nil
	}
	if s == nil {
		if transcode {
			n := 0
			for k := range h.streams {
				if k.transcode {
					n++
				}
			}
			if n >= maxTranscodes {
				h.smu.Unlock()
				return nil, ErrBusy
			}
		}
		s = h.startStream(key, url, transcode)
	}
	sub := &Sub{C: make(chan []byte, 64), s: s}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.mu.Unlock()
	h.smu.Unlock()

	select {
	case <-s.ready:
	case <-time.After(15 * time.Second):
		sub.Close()
		return nil, errors.New("the camera did not start streaming")
	case <-h.ctx.Done():
		return nil, h.ctx.Err()
	}
	s.mu.Lock()
	err, init, codec := s.err, s.init, s.codec
	s.mu.Unlock()
	if err != nil {
		sub.Close()
		return nil, err
	}
	sub.Init, sub.Codec = init, codec
	return sub, nil
}

func (sub *Sub) Close() {
	s := sub.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	if len(s.subs) == 0 && s.idleTimer == nil {
		s.idleTimer = time.AfterFunc(10*time.Second, func() {
			s.mu.Lock()
			n := len(s.subs)
			s.mu.Unlock()
			if n == 0 {
				s.cancel()
			}
		})
	}
}

func (h *Hub) startStream(key streamKey, url string, transcode bool) *stream {
	ctx, cancel := context.WithCancel(h.ctx)
	s := &stream{url: url, transcode: transcode, encoder: h.encoder, cancel: cancel, subs: map[*Sub]struct{}{}, ready: make(chan struct{})}
	h.streams[key] = s
	go func() {
		s.run(ctx)
		cancel()
		s.fail(errors.New("the camera stream ended"))
		s.mu.Lock()
		for sub := range s.subs {
			close(sub.C)
		}
		s.subs = map[*Sub]struct{}{}
		s.mu.Unlock()
		h.smu.Lock()
		if h.streams[key] == s {
			delete(h.streams, key)
		}
		h.smu.Unlock()
	}()
	return s
}

// fail records an error if the stream never became ready, and wakes waiters.
func (s *stream) fail(err error) {
	s.readyOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.ready)
	})
}

func (s *stream) run(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "ffmpeg", s.args()...)
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return
	}
	defer cmd.Wait()
	r := bufio.NewReaderSize(out, 256<<10)
	var initBuf, moof []byte
	started := false
	for {
		typ, box, err := readBox(r)
		if err != nil {
			return
		}
		switch typ {
		case "ftyp":
			initBuf = append(initBuf, box...)
		case "moov":
			if started {
				continue
			}
			initBuf = append(initBuf, bytes.Replace(box, []byte("hev1"), []byte("hvc1"), 1)...) // Safari only takes hvc1
			codec, err := codecOf(box)
			if err != nil {
				s.fail(err)
				return
			}
			s.mu.Lock()
			s.init, s.codec = initBuf, codec
			s.mu.Unlock()
			started = true
			s.readyOnce.Do(func() { close(s.ready) })
		case "moof":
			moof = box
		case "mdat":
			if started && moof != nil {
				s.broadcast(append(moof, box...))
			}
			moof = nil
		}
	}
}

func (s *stream) args() []string {
	a := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-fflags", "nobuffer", "-flags", "low_delay", "-rtsp_transport", "tcp", "-i", s.url, "-an"}
	if !s.transcode {
		a = append(a, "-c:v", "copy")
	} else {
		// Small and light: this runs on a Pi while someone watches, next to the recorders.
		a = append(a, "-vf", "scale=-2:'min(540,ih)'", "-pix_fmt", "yuv420p", "-g", "25", "-bf", "0")
		if s.encoder == "h264_v4l2m2m" {
			a = append(a, "-c:v", "h264_v4l2m2m", "-b:v", "1500k")
		} else {
			a = append(a, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-crf", "30", "-threads", "2")
		}
	}
	return append(a, "-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof", "-flush_packets", "1", "pipe:1")
}

func (s *stream) broadcast(frag []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		select {
		case sub.C <- frag:
		default: // viewer can't keep up: drop it rather than stall everyone
			delete(s.subs, sub)
			close(sub.C)
		}
	}
}

// readBox reads one MP4 box (size, type, payload) from r.
func readBox(r io.Reader) (string, []byte, error) {
	var h [8]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return "", nil, err
	}
	size := binary.BigEndian.Uint32(h[:4])
	if size < 8 || size > 64<<20 {
		return "", nil, fmt.Errorf("unexpected box size %d", size)
	}
	box := make([]byte, size)
	copy(box, h[:])
	if _, err := io.ReadFull(r, box[8:]); err != nil {
		return "", nil, err
	}
	return string(h[4:8]), box, nil
}

// codecOf builds the codec string for MediaSource.addSourceBuffer from the avcC (H.264) or
// hvcC (H.265) box inside a moov box.
func codecOf(moov []byte) (string, error) {
	if i := bytes.Index(moov, []byte("avcC")); i >= 0 && i+8 <= len(moov) {
		return fmt.Sprintf("avc1.%02X%02X%02X", moov[i+5], moov[i+6], moov[i+7]), nil
	}
	i := bytes.Index(moov, []byte("hvcC"))
	if i < 0 || i+17 > len(moov) {
		return "", ErrUnsupported
	}
	c := moov[i+4 : i+17] // version, profile byte, 4 compatibility bytes, 6 constraint bytes, level
	space := []string{"", "A", "B", "C"}[c[1]>>6]
	tier := "L"
	if c[1]&0x20 != 0 {
		tier = "H"
	}
	compat := bits.Reverse32(binary.BigEndian.Uint32(c[2:6]))
	cons := c[6:12]
	n := len(cons)
	for n > 1 && cons[n-1] == 0 { // trailing zero bytes are left out
		n--
	}
	parts := make([]string, n)
	for k := range parts {
		parts[k] = fmt.Sprintf("%X", cons[k])
	}
	return fmt.Sprintf("hvc1.%s%d.%X.%s%d.%s", space, c[1]&0x1f, compat, tier, c[12], strings.Join(parts, ".")), nil
}
