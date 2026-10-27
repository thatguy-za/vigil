package detect

import "testing"

func TestParseHost(t *testing.T) {
	ok := map[string][2]string{
		"192.168.1.20":                      {"192.168.1.20", "554"},
		"192.168.1.20:8554":                 {"192.168.1.20", "8554"},
		" cam.local ":                       {"cam.local", "554"},
		"rtsp://admin:pw@10.0.0.5:554/live": {"10.0.0.5", "554"},
		"http://10.0.0.5/":                  {"10.0.0.5", "554"},
	}
	for in, want := range ok {
		h, p, err := ParseHost(in)
		if err != nil || h != want[0] || p != want[1] {
			t.Errorf("%q: got %q %q %v", in, h, p, err)
		}
	}
	for _, bad := range []string{"", "   ", "bad host", "a;b", "rtsp://"} {
		if _, _, err := ParseHost(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
