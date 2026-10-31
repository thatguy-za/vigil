package sysinfo

import (
	"reflect"
	"testing"
)

func TestParseThrottled(t *testing.T) {
	if got := ParseThrottled("0x0"); len(got) != 0 {
		t.Fatalf("clean state gave %v", got)
	}
	got := ParseThrottled("0x50005")
	want := []string{"Under-voltage right now", "Throttled right now", "Under-voltage since boot", "Was throttled since boot"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if ParseThrottled("junk") != nil {
		t.Fatal("junk should give nil")
	}
}

func TestParseMemInfo(t *testing.T) {
	total, avail := ParseMemInfo("MemTotal:  948224 kB\nMemFree: 100 kB\nMemAvailable:  512000 kB\n")
	if total != 926 || avail != 500 {
		t.Fatalf("got %d %d", total, avail)
	}
}
