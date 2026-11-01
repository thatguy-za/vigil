package db

import (
	"testing"
	"time"
)

func TestEventsSinceFilters(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, e := range []Event{
		{CameraID: 1, Start: now.Add(-3 * time.Hour), Source: "vigil"},
		{CameraID: 1, Start: now.Add(-2 * time.Hour), Source: "camera"},
		{CameraID: 2, Start: now.Add(-1 * time.Hour), Source: "both"},
		{CameraID: 2, Start: now.Add(-48 * time.Hour)},
	} {
		e.End = e.Start
		if err := d.AddEvent(e); err != nil {
			t.Fatal(i, err)
		}
	}
	count := func(cam int64, src string) int {
		evs, err := d.EventsSince(cam, now.Add(-24*time.Hour), src, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	for _, c := range []struct {
		cam  int64
		src  string
		want int
	}{{0, "", 3}, {1, "", 2}, {0, "camera", 2}, {0, "vigil", 2}, {2, "vigil", 1}, {1, "camera", 1}} {
		if got := count(c.cam, c.src); got != c.want {
			t.Errorf("cam %d src %q: got %d want %d", c.cam, c.src, got, c.want)
		}
	}
}
