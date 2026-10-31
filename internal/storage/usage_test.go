package storage

import "testing"

const sample = `22 1 179:2 / / rw,noatime - overlay overlay rw,lowerdir=/x
36 22 8:1 /recordings /recordings rw,noatime master:1 - ext4 /dev/sda1 rw
37 22 179:2 /config /config rw,noatime master:1 - ext4 /dev/mmcblk0p2 rw
38 22 0:25 / /tmp rw - tmpfs tmpfs rw,size=131072k`

func TestParseMount(t *testing.T) {
	m := parseMount(sample, "/recordings")
	if m.Source != "/dev/sda1" || m.FSType != "ext4" || m.SDCard || m.Inside {
		t.Fatalf("usb drive: %+v", m)
	}
	m = parseMount(sample, "/config")
	if !m.SDCard {
		t.Fatalf("sd card not detected: %+v", m)
	}
	m = parseMount(sample, "/somewhere/else")
	if !m.Inside {
		t.Fatalf("container filesystem not detected: %+v", m)
	}
	if m = parseMount(sample, "/tmp/x"); m.FSType != "tmpfs" || !m.Inside {
		t.Fatalf("tmpfs: %+v", m)
	}
}
