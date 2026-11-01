package api

import (
	"strings"
	"testing"
	"time"
)

func TestConcatListTrimsEnds(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)
	spans := []span{
		{"/r/cam1/a.mp4", t0, t0.Add(300 * time.Second), false},
		{"/r/cam1/b.mp4", t0.Add(300 * time.Second), t0.Add(600 * time.Second), false},
	}
	got := concatList(spans, t0.Add(100*time.Second), t0.Add(450*time.Second))
	want := "file 'file:/r/cam1/a.mp4'\ninpoint 100.00\nfile 'file:/r/cam1/b.mp4'\noutpoint 150.00\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if strings.Contains(concatList(spans[:1], t0, t0.Add(time.Hour)), "point") {
		t.Fatal("a fully covered file should not be trimmed")
	}
}

func TestClipGuesses(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)
	spans := []span{
		{"a", t0, t0.Add(300 * time.Second), true},                         // recorder restarted after 100 s
		{"b", t0.Add(100 * time.Second), t0.Add(400 * time.Second), false}, // known length, left alone
		{"c", t0.Add(200 * time.Second), t0.Add(500 * time.Second), true},
	}
	clipGuesses(spans)
	if !spans[0].end.Equal(t0.Add(100 * time.Second)) {
		t.Fatalf("a ends %v", spans[0].end)
	}
	if !spans[1].end.Equal(t0.Add(400*time.Second)) || !spans[2].end.Equal(t0.Add(500*time.Second)) {
		t.Fatal("known and last spans must not change")
	}
}
