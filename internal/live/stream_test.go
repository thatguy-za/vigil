package live

import (
	"bytes"
	"encoding/binary"
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
