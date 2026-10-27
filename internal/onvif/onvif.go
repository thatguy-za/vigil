// Package onvif implements the small part of ONVIF that Vigil needs: WS-Discovery
// to find cameras, and a SOAP client that logs in and returns RTSP stream URLs.
package onvif

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const MulticastAddr = "239.255.255.250:3702"

var ErrAuth = errors.New("wrong username or password")

type Device struct {
	ID       string // stable short id derived from the device's endpoint address
	Name     string
	Hardware string
	Host     string // IP the device answered from
	XAddr    string // device service URL (host forced to the answering IP)
}

// ---------- WS-Discovery ----------

func probeMsg() []byte {
	id := make([]byte, 16)
	rand.Read(id)
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope" xmlns:w="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl">
<e:Header><w:MessageID>uuid:` + hex.EncodeToString(id) + `</w:MessageID><w:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</w:To><w:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</w:Action></e:Header>
<e:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types></d:Probe></e:Body></e:Envelope>`)
}

// Discover multicasts a probe to target and collects replies for `wait`.
func Discover(ctx context.Context, wait time.Duration, target string) ([]Device, error) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	dst, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		return nil, err
	}
	send := func() { conn.WriteTo(probeMsg(), dst) }
	send()
	t := time.AfterFunc(wait/3, send) // UDP is lossy: probe twice
	defer t.Stop()
	conn.SetReadDeadline(time.Now().Add(wait))

	found := map[string]Device{}
	buf := make([]byte, 64<<10)
	for ctx.Err() == nil {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		if d, ok := parseMatch(buf[:n], from); ok {
			found[d.ID] = d
		}
	}
	out := make([]Device, 0, len(found))
	for _, d := range found {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

type probeResp struct {
	Matches []struct {
		Address string `xml:"EndpointReference>Address"`
		Scopes  string `xml:"Scopes"`
		XAddrs  string `xml:"XAddrs"`
	} `xml:"Body>ProbeMatches>ProbeMatch"`
}

func parseMatch(b []byte, from net.Addr) (Device, bool) {
	var r probeResp
	ua, ok := from.(*net.UDPAddr)
	if !ok || xml.Unmarshal(b, &r) != nil || len(r.Matches) == 0 {
		return Device{}, false
	}
	m, ip := r.Matches[0], ua.IP.String()
	d := Device{Host: ip}
	for _, x := range strings.Fields(m.XAddrs) {
		u, err := url.Parse(x)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		// Only talk to the address that actually answered, never one it names.
		if p := u.Port(); p != "" {
			u.Host = net.JoinHostPort(ip, p)
		} else {
			u.Host = ip
		}
		d.XAddr = u.String()
		break
	}
	if d.XAddr == "" {
		return Device{}, false
	}
	for _, s := range strings.Fields(m.Scopes) {
		if v, ok := strings.CutPrefix(s, "onvif://www.onvif.org/name/"); ok {
			d.Name, _ = url.PathUnescape(v)
		} else if v, ok := strings.CutPrefix(s, "onvif://www.onvif.org/hardware/"); ok {
			d.Hardware, _ = url.PathUnescape(v)
		}
	}
	if d.Name == "" {
		d.Name = d.Hardware
	}
	if d.Name == "" {
		d.Name = "Camera at " + ip
	}
	key := m.Address
	if key == "" {
		key = ip
	}
	sum := sha1.Sum([]byte(key))
	d.ID = hex.EncodeToString(sum[:6])
	return d, true
}

// ---------- SOAP client ----------

func digest(nonce []byte, created, password string) string {
	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(password))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

type client struct {
	http       *http.Client
	user, pass string
	skew       time.Duration
}

func (c *client) call(ctx context.Context, endpoint, body string, auth bool) ([]byte, error) {
	hdr := ""
	if auth {
		nonce := make([]byte, 16)
		rand.Read(nonce)
		created := time.Now().Add(c.skew).UTC().Format("2006-01-02T15:04:05Z")
		hdr = `<s:Header><Security s:mustUnderstand="1" xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"><UsernameToken><Username>` +
			html.EscapeString(c.user) + `</Username><Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` +
			digest(nonce, created, c.pass) + `</Password><Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` +
			base64.StdEncoding.EncodeToString(nonce) + `</Nonce><Created xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` +
			created + `</Created></UsernameToken></Security></s:Header>`
	}
	env := `<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">` + hdr + `<s:Body>` + body + `</s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(env))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the camera: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 || bytes.Contains(b, []byte("NotAuthorized")) || bytes.Contains(b, []byte("Sender not Authorized")) {
		return nil, ErrAuth
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("camera answered HTTP %d", resp.StatusCode)
	}
	return b, nil
}

// Resolve logs in to an ONVIF device and returns its main (highest resolution)
// and sub (lowest resolution) RTSP URLs with the credentials embedded.
// Streams are the RTSP URLs a camera offers, with credentials embedded.
type Streams struct {
	Main, Sub       string
	MainRes, SubRes string // "1920x1080", "" if the camera didn't say
}

// Resolve is ResolveInfo without the resolutions.
func Resolve(ctx context.Context, xaddr, user, pass string) (mainURL, subURL string, err error) {
	r, err := ResolveInfo(ctx, xaddr, user, pass)
	return r.Main, r.Sub, err
}

func ResolveInfo(ctx context.Context, xaddr, user, pass string) (Streams, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := &client{http: &http.Client{Timeout: 8 * time.Second}, user: user, pass: pass}

	// Cameras reject tokens whose timestamp is far from their own clock.
	var dt struct {
		Y, Mo, D, H, Mi, S int
	}
	if b, err := c.call(ctx, xaddr, `<GetSystemDateAndTime xmlns="http://www.onvif.org/ver10/device/wsdl"/>`, false); err == nil {
		var r struct {
			Y  int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Date>Year"`
			Mo int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Date>Month"`
			D  int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Date>Day"`
			H  int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Time>Hour"`
			Mi int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Time>Minute"`
			S  int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Time>Second"`
		}
		if xml.Unmarshal(b, &r) == nil && r.Y > 2000 {
			dt = struct{ Y, Mo, D, H, Mi, S int }(r)
			c.skew = time.Date(dt.Y, time.Month(dt.Mo), dt.D, dt.H, dt.Mi, dt.S, 0, time.UTC).Sub(time.Now())
		}
	}

	media := xaddr // most cameras answer media calls on the device service too
	if b, err := c.call(ctx, xaddr, `<GetCapabilities xmlns="http://www.onvif.org/ver10/device/wsdl"><Category>Media</Category></GetCapabilities>`, true); err != nil {
		if errors.Is(err, ErrAuth) {
			return Streams{}, err
		}
	} else {
		var r struct {
			XAddr string `xml:"Body>GetCapabilitiesResponse>Capabilities>Media>XAddr"`
		}
		if xml.Unmarshal(b, &r) == nil && r.XAddr != "" {
			if u, err := url.Parse(r.XAddr); err == nil {
				// keep talking to the host we already trust
				if base, err := url.Parse(xaddr); err == nil {
					u.Host = base.Host
					media = u.String()
				}
			}
		}
	}

	b, err := c.call(ctx, media, `<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>`, true)
	if err != nil {
		return Streams{}, err
	}
	var pr struct {
		Profiles []struct {
			Token string `xml:"token,attr"`
			W     int    `xml:"VideoEncoderConfiguration>Resolution>Width"`
			H     int    `xml:"VideoEncoderConfiguration>Resolution>Height"`
		} `xml:"Body>GetProfilesResponse>Profiles"`
	}
	if err := xml.Unmarshal(b, &pr); err != nil || len(pr.Profiles) == 0 {
		return Streams{}, errors.New("the camera has no video profiles")
	}
	type stream struct {
		uri    string
		pixels int
		w, h   int
	}
	var streams []stream
	for _, p := range pr.Profiles {
		b, err := c.call(ctx, media, `<GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl"><StreamSetup><Stream xmlns="http://www.onvif.org/ver10/schema">RTP-Unicast</Stream><Transport xmlns="http://www.onvif.org/ver10/schema"><Protocol>RTSP</Protocol></Transport></StreamSetup><ProfileToken>`+html.EscapeString(p.Token)+`</ProfileToken></GetStreamUri>`, true)
		if err != nil {
			continue
		}
		var ur struct {
			URI string `xml:"Body>GetStreamUriResponse>MediaUri>Uri"`
		}
		if xml.Unmarshal(b, &ur) != nil || ur.URI == "" {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(ur.URI))
		if err != nil || (u.Scheme != "rtsp" && u.Scheme != "rtsps") {
			continue
		}
		if base, err := url.Parse(xaddr); err == nil && (u.Hostname() == "0.0.0.0" || u.Hostname() == "") {
			u.Host = net.JoinHostPort(base.Hostname(), u.Port())
		}
		u.User = url.UserPassword(user, pass)
		streams = append(streams, stream{u.String(), p.W * p.H, p.W, p.H})
	}
	if len(streams) == 0 {
		return Streams{}, errors.New("the camera did not return a stream address")
	}
	hi, lo := streams[0], streams[0]
	for _, st := range streams {
		if st.pixels > hi.pixels {
			hi = st
		}
		if st.pixels < lo.pixels {
			lo = st
		}
	}
	res := func(st stream) string {
		if st.w == 0 || st.h == 0 {
			return ""
		}
		return fmt.Sprintf("%dx%d", st.w, st.h)
	}
	out := Streams{Main: hi.uri, MainRes: res(hi)}
	if lo.uri != hi.uri && lo.pixels < hi.pixels {
		out.Sub, out.SubRes = lo.uri, res(lo)
	}
	return out, nil
}

// DefaultPorts are where cameras commonly serve ONVIF.
var DefaultPorts = []int{80, 8080, 8000, 8899, 2020, 5000, 8081, 8001}

// FindDevice returns the ONVIF device-service URLs that answer on host.
// A device that demands a login even for this call still counts as present.
func FindDevice(ctx context.Context, host string, ports []int) []string {
	c := &client{http: &http.Client{Timeout: 2 * time.Second}}
	found := make([]string, len(ports))
	var wg sync.WaitGroup
	for i, port := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ep := "http://" + net.JoinHostPort(host, fmt.Sprint(port)) + "/onvif/device_service"
			_, err := c.call(ctx, ep, `<GetSystemDateAndTime xmlns="http://www.onvif.org/ver10/device/wsdl"/>`, false)
			if err == nil || errors.Is(err, ErrAuth) {
				found[i] = ep
			}
		}()
	}
	wg.Wait()
	var out []string
	for _, f := range found {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
