package motion

import "testing"

func frame(v byte, box int) []byte {
	f := make([]byte, 320*180)
	for i := range f {
		f[i] = v
	}
	for i := 0; i < box; i++ {
		f[i] = 255
	}
	return f
}

func TestDetector(t *testing.T) {
	d := New(5, "")
	for i := 0; i < 30; i++ {
		if d.Feed(frame(100, 0)) {
			t.Fatal("static scene triggered")
		}
	}
	if !d.Feed(frame(100, 320*180/20)) { // 5% of the frame
		t.Fatal("object not detected")
	}
	if d.Feed(frame(100, 10)) { // tiny noise (<0.1%)
		t.Fatal("noise triggered")
	}
	d.Feed(frame(100, 0))
	if d.Feed(frame(220, 0)) { // global brightness jump
		t.Fatal("lighting change triggered")
	}
}

func TestMask(t *testing.T) {
	// ignore the top-left cell (rows of 320 px: first 20 rows x first 20 columns)
	mask := "1" + string(make([]byte, 0))
	for i := 1; i < MaskCols*MaskRows; i++ {
		mask += "0"
	}
	d := New(5, mask)
	for i := 0; i < 30; i++ {
		d.Feed(frame(100, 0))
	}
	f := frame(100, 0)
	for y := 0; y < 20; y++ {
		for x := 0; x < 20; x++ {
			f[y*W+x] = 255
		}
	}
	if d.Feed(f) { // 400 px = 0.7% of frame but entirely inside the masked cell
		t.Fatal("masked area triggered")
	}
	g := frame(100, 0)
	for y := 100; y < 120; y++ {
		for x := 100; x < 130; x++ {
			g[y*W+x] = 255
		}
	}
	if !d.Feed(g) { // 600 px unmasked
		t.Fatal("unmasked area not detected")
	}
}
