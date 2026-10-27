package onvif

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDigest(t *testing.T) {
	// test vector from the WS-Security UsernameToken Profile 1.0 specification
	nonce, _ := base64.StdEncoding.DecodeString("LKqI6G/AikKCQrN0zqZFlg==")
	if got := digest(nonce, "2010-09-16T07:50:45Z", "userpassword"); got != "tuOSpGlFlIXsozq4HFNeeGeFLEI=" {
		t.Fatalf("digest = %s", got)
	}
}

const probeReply = `<?xml version="1.0"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"><s:Body><d:ProbeMatches><d:ProbeMatch>
<a:EndpointReference><a:Address>urn:uuid:11111111-2222-3333-4444-555555555555</a:Address></a:EndpointReference>
<d:Types>dn:NetworkVideoTransmitter</d:Types>
<d:Scopes>onvif://www.onvif.org/type/video_encoder onvif://www.onvif.org/name/Front%20Door onvif://www.onvif.org/hardware/IPC-1234</d:Scopes>
<d:XAddrs>http://10.9.9.9:8899/onvif/device_service</d:XAddrs></d:ProbeMatch></d:ProbeMatches></s:Body></s:Envelope>`

func TestDiscover(t *testing.T) {
	srv, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer srv.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			_, from, err := srv.ReadFrom(buf)
			if err != nil {
				return
			}
			srv.WriteTo([]byte(probeReply), from)
		}
	}()
	devs, err := Discover(context.Background(), 600*time.Millisecond, srv.LocalAddr().String())
	if err != nil || len(devs) != 1 {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	d := devs[0]
	// the advertised host 10.9.9.9 must be replaced by the address that answered
	if d.Name != "Front Door" || d.Hardware != "IPC-1234" || d.Host != "127.0.0.1" || d.XAddr != "http://127.0.0.1:8899/onvif/device_service" {
		t.Fatalf("unexpected device %+v", d)
	}
}

func TestResolve(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		body := string(b[:n])
		resp := func(inner string) {
			fmt.Fprintf(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>%s</s:Body></s:Envelope>`, inner)
		}
		if strings.Contains(body, "GetSystemDateAndTime") {
			resp(`<tds:GetSystemDateAndTimeResponse xmlns:tds="x"><tds:SystemDateAndTime><tt:UTCDateTime xmlns:tt="y"><tt:Time><tt:Hour>1</tt:Hour><tt:Minute>2</tt:Minute><tt:Second>3</tt:Second></tt:Time><tt:Date><tt:Year>2020</tt:Year><tt:Month>1</tt:Month><tt:Day>1</tt:Day></tt:Date></tt:UTCDateTime></tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`)
			return
		}
		// everything else needs a valid digest for password "demo"
		nonce := regexp.MustCompile(`<Nonce[^>]*>([^<]+)</Nonce>`).FindStringSubmatch(body)
		created := regexp.MustCompile(`<Created[^>]*>([^<]+)</Created>`).FindStringSubmatch(body)
		pw := regexp.MustCompile(`<Password[^>]*>([^<]+)</Password>`).FindStringSubmatch(body)
		ok := false
		if nonce != nil && created != nil && pw != nil {
			nb, _ := base64.StdEncoding.DecodeString(nonce[1])
			ok = digest(nb, created[1], "demo") == pw[1]
		}
		if !ok {
			w.WriteHeader(400)
			resp(`<s:Fault><s:Code><s:Subcode><s:Value>ter:NotAuthorized</s:Value></s:Subcode></s:Code></s:Fault>`)
			return
		}
		switch {
		case strings.Contains(body, "GetCapabilities"):
			resp(`<tds:GetCapabilitiesResponse xmlns:tds="x"><tds:Capabilities><tt:Media xmlns:tt="y"><tt:XAddr>http://192.0.2.1:1/media</tt:XAddr></tt:Media></tds:Capabilities></tds:GetCapabilitiesResponse>`)
		case strings.Contains(body, "GetProfiles"):
			resp(`<trt:GetProfilesResponse xmlns:trt="x" xmlns:tt="y">
<trt:Profiles token="hi"><tt:Name>main</tt:Name><tt:VideoEncoderConfiguration><tt:Resolution><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:Resolution></tt:VideoEncoderConfiguration></trt:Profiles>
<trt:Profiles token="lo"><tt:Name>sub</tt:Name><tt:VideoEncoderConfiguration><tt:Resolution><tt:Width>640</tt:Width><tt:Height>360</tt:Height></tt:Resolution></tt:VideoEncoderConfiguration></trt:Profiles></trt:GetProfilesResponse>`)
		case strings.Contains(body, "GetStreamUri"):
			tok := "hi"
			if strings.Contains(body, ">lo<") {
				tok = "lo"
			}
			resp(`<trt:GetStreamUriResponse xmlns:trt="x"><trt:MediaUri><tt:Uri xmlns:tt="y">rtsp://0.0.0.0:554/` + tok + `</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`)
		}
	}))
	defer srv.Close()

	m, s, err := Resolve(context.Background(), srv.URL, "admin", "demo")
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]
	if m != "rtsp://admin:demo@"+host+":554/hi" || s != "rtsp://admin:demo@"+host+":554/lo" {
		t.Fatalf("main=%s sub=%s", m, s)
	}
	if _, _, err := Resolve(context.Background(), srv.URL, "admin", "wrong"); !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
}

func TestFindDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<s:Envelope xmlns:s="x"><s:Body><GetSystemDateAndTimeResponse/></s:Body></s:Envelope>`)
	}))
	defer srv.Close()
	port := 0
	fmt.Sscanf(srv.URL[strings.LastIndex(srv.URL, ":")+1:], "%d", &port)
	got := FindDevice(context.Background(), "127.0.0.1", []int{1, port})
	if len(got) != 1 || !strings.Contains(got[0], fmt.Sprint(port)) {
		t.Fatalf("got %v", got)
	}
}
