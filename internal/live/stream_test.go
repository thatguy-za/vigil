package live

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	return append(b, payload...)
}

func TestReadBox(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(box("ftyp", []byte("isom")))
	buf.Write(box("moof", []byte{1, 2, 3}))
	for _, want := range []string{"ftyp", "moof"} {
		typ, b, err := readBox(&buf)
		if err != nil || typ != want || len(b) < 8 {
			t.Fatalf("got %q %v, want %q", typ, err, want)
		}
	}
	if _, _, err := readBox(&buf); err == nil {
		t.Fatal("expected EOF")
	}
	if _, _, err := readBox(bytes.NewReader([]byte{0, 0, 0, 3, 'x', 'x', 'x', 'x'})); err == nil {
		t.Fatal("tiny box size should fail")
	}
}

func TestCodecOf(t *testing.T) {
	avcc := append([]byte("avcC"), 1, 0x64, 0x00, 0x28) // version, profile, compat, level
	got, err := codecOf(box("moov", avcc))
	if err != nil || got != "avc1.640028" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := codecOf(box("moov", append([]byte("hvcC"), 1, 1, 1, 1))); err != ErrUnsupported {
		t.Fatalf("hevc should be unsupported, got %v", err)
	}
}

func TestCodecOfHEVC(t *testing.T) {
	// version 1, Main profile, compat flags 0x60000000, constraint B0 00.., level 93
	hvcc := append([]byte("hvcC"), 1, 0x01, 0x60, 0, 0, 0, 0xB0, 0, 0, 0, 0, 0, 93)
	got, err := codecOf(box("moov", hvcc))
	if err != nil || got != "hvc1.1.6.L93.B0" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestTranscodeArgs(t *testing.T) {
	s := &stream{url: "rtsp://x", transcode: true, encoder: "h264_v4l2m2m"}
	if a := strings.Join(s.args(), " "); !strings.Contains(a, "h264_v4l2m2m") || strings.Contains(a, "-c:v copy") {
		t.Fatalf("hardware transcode args: %s", a)
	}
	s = &stream{url: "rtsp://x", transcode: true, encoder: "libx264"}
	if a := strings.Join(s.args(), " "); !strings.Contains(a, "libx264") {
		t.Fatalf("software transcode args: %s", a)
	}
	s = &stream{url: "rtsp://x"}
	if a := strings.Join(s.args(), " "); !strings.Contains(a, "-c:v copy") {
		t.Fatalf("copy args: %s", a)
	}
}

func TestBroadcastDropsSlowViewer(t *testing.T) {
	s := &stream{subs: map[*Sub]struct{}{}}
	slow := &Sub{C: make(chan []byte, 1), s: s}
	fast := &Sub{C: make(chan []byte, 8), s: s}
	s.subs[slow], s.subs[fast] = struct{}{}, struct{}{}
	s.broadcast([]byte("a"))
	s.broadcast([]byte("b")) // slow's buffer is full now
	if _, ok := s.subs[slow]; ok {
		t.Fatal("slow viewer should have been dropped")
	}
	if len(fast.C) != 2 {
		t.Fatalf("fast viewer got %d fragments", len(fast.C))
	}
}
