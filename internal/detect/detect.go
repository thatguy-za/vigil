// Package detect works out a camera's main and sub-stream URLs from just its
// address and login: first through ONVIF, then by trying the RTSP paths that
// common camera brands use.
package detect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/thatguy-za/vigil/internal/onvif"
)

var (
	ErrAuth        = onvif.ErrAuth
	ErrUnreachable = errors.New("could not reach the camera. Check the address and that it is powered on and on the same network")
	ErrNotFound    = errors.New("the camera answered but none of the usual stream addresses worked. Enter the stream URL manually")
)

type Result struct {
	URL     string `json:"url"`
	SubURL  string `json:"sub_url"`
	Method  string `json:"method"` // onvif | pattern
	Info    string `json:"main_info"`
	SubInfo string `json:"sub_info"`
}

type Request struct {
	Host, User, Pass string
	XAddr            string // known ONVIF address (from discovery), tried first
}

type pattern struct{ name, main, sub string }

// Common RTSP paths by brand. Order is the order they are tried in.
var patterns = []pattern{
	{"Hikvision", "/Streaming/Channels/101", "/Streaming/Channels/102"},
	{"Dahua / Amcrest / Lorex", "/cam/realmonitor?channel=1&subtype=0", "/cam/realmonitor?channel=1&subtype=1"},
	{"Reolink", "/h264Preview_01_main", "/h264Preview_01_sub"},
	{"TP-Link Tapo / Vigi", "/stream1", "/stream2"},
	{"Uniview", "/media/video1", "/media/video2"},
	{"Foscam", "/videoMain", "/videoSub"},
	{"Hikvision (older)", "/h264/ch1/main/av_stream", "/h264/ch1/sub/av_stream"},
	{"Ubiquiti", "/s0", "/s1"},
	{"Generic", "/live/ch00_0", "/live/ch00_1"},
	{"Generic", "/11", "/12"},
	{"Generic", "/ch0_0.h264", "/ch0_1.h264"},
	{"Axis", "/axis-media/media.amp", ""},
	{"Generic", "/h264", ""},
	{"Generic", "/live", ""},
	{"Generic", "/", ""},
}

// ParseHost accepts "192.168.1.20", "192.168.1.20:554" or a pasted rtsp:// URL
// and returns the host and RTSP port.
func ParseHost(in string) (host, port string, err error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", "", errors.New("enter the camera's IP address")
	}
	if !strings.Contains(in, "://") {
		in = "rtsp://" + in
	}
	u, err := url.Parse(in)
	if err != nil || u.Hostname() == "" {
		return "", "", errors.New("that doesn't look like an IP address or hostname")
	}
	host, port = u.Hostname(), u.Port()
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == ':') {
			return "", "", errors.New("that doesn't look like an IP address or hostname")
		}
	}
	if port == "" {
		port = "554"
	}
	return host, port, nil
}

// Detect finds the streams. progress, if not nil, is called with a plain-language
// description of each step as it starts.
func Detect(ctx context.Context, req Request, progress func(string)) (Result, error) {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	host, port, err := ParseHost(req.Host)
	if err != nil {
		return Result{}, err
	}
	sawAuth := false

	// 1. ONVIF gives both streams and their resolutions in one go.
	say("Looking for the camera at " + host + "…")
	xaddrs := onvif.FindDevice(ctx, host, onvif.DefaultPorts)
	if req.XAddr != "" {
		xaddrs = append([]string{req.XAddr}, xaddrs...)
	}
	seen := map[string]bool{}
	for _, x := range xaddrs {
		if seen[x] {
			continue
		}
		seen[x] = true
		say("Camera found. Signing in with ONVIF…")
		st, err := onvif.ResolveInfo(ctx, x, req.User, req.Pass)
		if err == nil {
			return Result{URL: st.Main, SubURL: st.Sub, Method: "onvif", Info: st.MainRes, SubInfo: st.SubRes}, nil
		}
		if errors.Is(err, ErrAuth) {
			sawAuth = true
			say("The camera rejected that username or password over ONVIF. Trying other ways in…")
		}
	}
	if len(xaddrs) == 0 {
		say("No ONVIF service found. Trying the stream addresses cameras commonly use…")
	}

	// 2. Brand-specific RTSP paths.
	say("Connecting to the camera's video port (" + port + ")…")
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		if sawAuth {
			return Result{}, ErrAuth
		}
		return Result{}, ErrUnreachable
	}
	c.Close()

	base := url.URL{Scheme: "rtsp", Host: net.JoinHostPort(host, port)}
	if req.User != "" || req.Pass != "" {
		base.User = url.UserPassword(req.User, req.Pass)
	}
	mk := func(path string) string { return base.String() + path }

	type probeRes struct {
		info string
		auth bool
		ok   bool
	}
	results := make([]probeRes, len(patterns))
	const batch = 4 // keep a small Pi responsive
	for start := 0; start < len(patterns) && ctx.Err() == nil; start += batch {
		var names []string
		for i := start; i < start+batch && i < len(patterns); i++ {
			if n := patterns[i].name; n != "Generic" && !strings.Contains(strings.Join(names, ","), n) {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			names = []string{"generic"}
		}
		say("Trying " + strings.Join(names, ", ") + " addresses…")
		var wg sync.WaitGroup
		for i := start; i < start+batch && i < len(patterns); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				info, auth, ok := probeRTSP(ctx, mk(patterns[i].main))
				results[i] = probeRes{info, auth, ok}
			}()
		}
		wg.Wait()
		for i := start; i < start+batch && i < len(patterns); i++ {
			if results[i].ok {
				say("Stream found. Checking for a lower-resolution sub-stream…")
				r := Result{URL: mk(patterns[i].main), Method: "pattern", Info: results[i].info}
				if p := patterns[i].sub; p != "" {
					if info, _, ok := probeRTSP(ctx, mk(p)); ok {
						r.SubURL, r.SubInfo = mk(p), info
					}
				}
				return r, nil
			}
			sawAuth = sawAuth || results[i].auth
		}
	}
	if sawAuth {
		return Result{}, ErrAuth
	}
	return Result{}, ErrNotFound
}

// probeRTSP asks ffprobe for the video stream at url. It reports the stream
// ("1920x1080 h264"), whether the camera rejected the login, and success.
func probeRTSP(ctx context.Context, u string) (info string, auth, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-rtsp_transport", "tcp",
		"-select_streams", "v:0", "-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", "-i", u).CombinedOutput()
	text := string(out)
	if err != nil {
		low := strings.ToLower(text)
		return "", strings.Contains(low, "401") || strings.Contains(low, "unauthorized"), false
	}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) == 3 && f[1] != "" && f[2] != "" {
			return fmt.Sprintf("%sx%s %s", f[1], f[2], f[0]), false, true
		}
	}
	return "", false, false
}
