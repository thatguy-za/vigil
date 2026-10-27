// Package motion implements a cheap frame-differencing motion detector for
// small grayscale frames (the camera's sub-stream scaled down by ffmpeg).
package motion

import "strings"

const (
	W, H     = 320, 180 // analysis frame size
	MaskCols = 16       // the ignore-mask is a 16x9 grid of 20x20 pixel cells
	MaskRows = 9

	scale     = 6  // background is stored as pixel<<scale for sub-integer precision
	rate      = 4  // background adapts by 1/16 per frame
	pixThresh = 25 // per-pixel difference (0-255) that counts as changed
	warmup    = 10 // frames used to learn the background before reporting
	lighting  = 60 // % of pixels changed at once = lighting/IR switch, not motion
)

// areaPct maps sensitivity 1..10 to the % of the (unmasked) frame that must change.
var areaPct = [...]float64{3.0, 2.0, 1.4, 1.0, 0.7, 0.5, 0.35, 0.25, 0.15, 0.08}

type Detector struct {
	bg    []uint16
	n     int
	area  float64
	skip  []bool  // per pixel, true = masked out
	valid int     // number of unmasked pixels
	Pct   float64 // % of unmasked pixels that changed in the last frame
}

// New builds a detector. mask is MaskCols*MaskRows chars, '1' = ignore that cell.
func New(sensitivity int, mask string) *Detector {
	if sensitivity < 1 || sensitivity > 10 {
		sensitivity = 5
	}
	d := &Detector{area: areaPct[sensitivity-1], valid: W * H}
	if len(mask) == MaskCols*MaskRows && strings.Contains(mask, "1") {
		d.skip, d.valid = make([]bool, W*H), 0
		cw, ch := W/MaskCols, H/MaskRows
		for y := 0; y < H; y++ {
			for x := 0; x < W; x++ {
				m := mask[(y/ch)*MaskCols+x/cw] == '1'
				d.skip[y*W+x] = m
				if !m {
					d.valid++
				}
			}
		}
	}
	return d
}

// Feed returns true when the frame differs enough from the learned background.
func (d *Detector) Feed(f []byte) bool {
	d.Pct = 0
	if len(d.bg) != len(f) {
		d.reset(f)
		return false
	}
	changed := 0
	for i, p := range f {
		if d.skip != nil && d.skip[i] {
			continue
		}
		cur := int(p) << scale
		diff := (cur - int(d.bg[i])) >> scale
		if diff < 0 {
			diff = -diff
		}
		if diff > pixThresh {
			changed++
		}
		d.bg[i] = uint16(int(d.bg[i]) + (cur-int(d.bg[i]))>>rate)
	}
	if d.n < warmup {
		d.n++
		return false
	}
	if d.valid == 0 {
		return false
	}
	d.Pct = float64(changed) * 100 / float64(d.valid)
	if d.Pct > lighting {
		d.reset(f)
		d.Pct = 0
		return false
	}
	return d.Pct >= d.area
}

func (d *Detector) reset(f []byte) {
	d.bg = make([]uint16, len(f))
	for i, p := range f {
		d.bg[i] = uint16(p) << scale
	}
	d.n = 0
}
