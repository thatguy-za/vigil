package db

import (
	"testing"
	"time"
)

func TestUptimePct(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Hour)
	at := func(h int, kind string) HealthEvent {
		return HealthEvent{Time: from.Add(time.Duration(h) * time.Hour), Kind: kind}
	}

	// up 0-4h, down 4-6h, up 6-10h  => 8 of 10 hours
	got := UptimePct("online", []HealthEvent{at(4, "offline"), at(6, "online")}, from, to)
	if got < 79.99 || got > 80.01 {
		t.Fatalf("got %.2f, want 80", got)
	}
	// stopped 0-5h (not monitored), online 5-10h => 100%
	if got := UptimePct("stopped", []HealthEvent{at(5, "online")}, from, to); got != 100 {
		t.Fatalf("got %.2f, want 100", got)
	}
	// never monitored
	if got := UptimePct("", nil, from, to); got != -1 {
		t.Fatalf("got %.2f, want -1", got)
	}
}
