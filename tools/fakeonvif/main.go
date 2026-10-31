// Fake ONVIF camera for local testing: answers WS-Discovery and the SOAP calls Vigil makes.
// Login: admin / demo. Streams come from the demo RTSP server.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const rtsp = "rtsp://vigil-demo-mtx:8554/cam"

// The camera reports motion for 15 s of every minute, whatever the picture does.
var lastSent bool

func motionNow() bool { s := time.Now().Unix() % 60; return s >= 30 && s < 45 }

func main() {
	group, _ := net.ResolveUDPAddr("udp4", "239.255.255.250:3702")
	conn, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		buf := make([]byte, 8192)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil || !strings.Contains(string(buf[:n]), "Probe") {
				continue
			}
			log.Printf("probe from %s", from)
			reply := `<?xml version="1.0"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"><s:Body><d:ProbeMatches><d:ProbeMatch><a:EndpointReference><a:Address>urn:uuid:aaaaaaaa-bbbb-cccc-dddd-000000000001</a:Address></a:EndpointReference><d:Types>dn:NetworkVideoTransmitter</d:Types><d:Scopes>onvif://www.onvif.org/name/Garage%20Cam onvif://www.onvif.org/hardware/DemoCam-1</d:Scopes><d:XAddrs>http://127.0.0.1:8000/onvif/device_service</d:XAddrs></d:ProbeMatch></d:ProbeMatches></s:Body></s:Envelope>`
			time.Sleep(100 * time.Millisecond)
			conn.WriteToUDP([]byte(reply), from)
		}
	}()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		out := func(x string) {
			fmt.Fprintf(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>%s</s:Body></s:Envelope>`, x)
		}
		if strings.Contains(body, "GetSystemDateAndTime") {
			t := time.Now().UTC()
			out(fmt.Sprintf(`<GetSystemDateAndTimeResponse><SystemDateAndTime><UTCDateTime><Time><Hour>%d</Hour><Minute>%d</Minute><Second>%d</Second></Time><Date><Year>%d</Year><Month>%d</Month><Day>%d</Day></Date></UTCDateTime></SystemDateAndTime></GetSystemDateAndTimeResponse>`, t.Hour(), t.Minute(), t.Second(), t.Year(), t.Month(), t.Day()))
			return
		}
		g := func(re string) string {
			m := regexp.MustCompile(re).FindStringSubmatch(body)
			if m == nil {
				return ""
			}
			return m[1]
		}
		nonce, _ := base64.StdEncoding.DecodeString(g(`<Nonce[^>]*>([^<]+)</Nonce>`))
		h := sha1.New()
		h.Write(nonce)
		h.Write([]byte(g(`<Created[^>]*>([^<]+)</Created>`) + "demo"))
		if g(`<Username>([^<]*)</Username>`) != "admin" || base64.StdEncoding.EncodeToString(h.Sum(nil)) != g(`<Password[^>]*>([^<]+)</Password>`) {
			log.Printf("auth failed")
			w.WriteHeader(400)
			out(`<Fault><Code><Subcode><Value>ter:NotAuthorized</Value></Subcode></Code></Fault>`)
			return
		}
		switch {
		case strings.Contains(body, "GetCapabilities") && strings.Contains(body, "<Category>Events"):
			out(`<GetCapabilitiesResponse><Capabilities><Events><XAddr>http://127.0.0.1:8000/onvif/events</XAddr></Events></Capabilities></GetCapabilitiesResponse>`)
		case strings.Contains(body, "CreatePullPointSubscription"):
			out(`<CreatePullPointSubscriptionResponse><SubscriptionReference><Address>http://127.0.0.1:8000/onvif/pull</Address></SubscriptionReference></CreatePullPointSubscriptionResponse>`)
		case strings.Contains(body, "PullMessages"):
			// Wait up to 4 s for motion to start or stop, then report the change.
			for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
				if cur := motionNow(); cur != lastSent {
					lastSent = cur
					out(fmt.Sprintf(`<PullMessagesResponse><wsnt:NotificationMessage xmlns:wsnt="x"><wsnt:Topic>tns1:RuleEngine/CellMotionDetector/Motion</wsnt:Topic><wsnt:Message><tt:Message xmlns:tt="y"><tt:Data><tt:SimpleItem Name="IsMotion" Value="%v"/></tt:Data></tt:Message></wsnt:Message></wsnt:NotificationMessage></PullMessagesResponse>`, cur))
					return
				}
			}
			out(`<PullMessagesResponse/>`)
		case strings.Contains(body, "Renew") || strings.Contains(body, "Unsubscribe"):
			out(`<ok/>`)
		case strings.Contains(body, "GetCapabilities"):
			out(`<GetCapabilitiesResponse><Capabilities><Media><XAddr>http://127.0.0.1:8000/onvif/media</XAddr></Media></Capabilities></GetCapabilitiesResponse>`)
		case strings.Contains(body, "GetProfiles"):
			out(`<GetProfilesResponse><Profiles token="p1"><VideoEncoderConfiguration><Resolution><Width>1280</Width><Height>720</Height></Resolution></VideoEncoderConfiguration></Profiles></GetProfilesResponse>`)
		case strings.Contains(body, "GetStreamUri"):
			out(`<GetStreamUriResponse><MediaUri><Uri>` + rtsp + `</Uri></MediaUri></GetStreamUriResponse>`)
		}
	})
	log.Fatal(http.ListenAndServe(":8000", nil))
}
