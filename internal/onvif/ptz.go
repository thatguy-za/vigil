package onvif

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
)

// ErrNoPTZ means the camera has no pan, tilt or zoom service.
var ErrNoPTZ = errors.New("this camera has no pan/tilt/zoom")

const (
	ptzNS    = `xmlns="http://www.onvif.org/ver20/ptz/wsdl"`
	schemaNS = `xmlns="http://www.onvif.org/ver10/schema"`
)

// Preset is a saved camera position.
type Preset struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

// PTZ drives one camera's pan/tilt/zoom over ONVIF.
type PTZ struct {
	c       *client
	service string // PTZ service address
	profile string // media profile that has a PTZ configuration
}

// OpenPTZ finds the camera's PTZ service and a profile to control it with.
func OpenPTZ(ctx context.Context, xaddr, user, pass string) (*PTZ, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := &client{http: &http.Client{Timeout: 8 * time.Second}, user: user, pass: pass}
	c.syncClock(ctx, xaddr)

	caps := func(category string, out any) error {
		b, err := c.call(ctx, xaddr, `<GetCapabilities xmlns="http://www.onvif.org/ver10/device/wsdl"><Category>`+category+`</Category></GetCapabilities>`, true)
		if err != nil {
			return err
		}
		return xml.Unmarshal(b, out)
	}
	var pc struct {
		XAddr string `xml:"Body>GetCapabilitiesResponse>Capabilities>PTZ>XAddr"`
	}
	if err := caps("PTZ", &pc); err != nil {
		return nil, err
	}
	if strings.TrimSpace(pc.XAddr) == "" {
		return nil, ErrNoPTZ
	}
	media := xaddr
	var mc struct {
		XAddr string `xml:"Body>GetCapabilitiesResponse>Capabilities>Media>XAddr"`
	}
	if caps("Media", &mc) == nil && mc.XAddr != "" {
		media = trustHost(strings.TrimSpace(mc.XAddr), xaddr)
	}
	b, err := c.call(ctx, media, `<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>`, true)
	if err != nil {
		return nil, err
	}
	var pr struct {
		Profiles []struct {
			Token string `xml:"token,attr"`
			PTZ   *struct {
				Token string `xml:"token,attr"`
			} `xml:"PTZConfiguration"`
		} `xml:"Body>GetProfilesResponse>Profiles"`
	}
	if err := xml.Unmarshal(b, &pr); err != nil {
		return nil, err
	}
	for _, p := range pr.Profiles {
		if p.PTZ != nil {
			return &PTZ{c: c, service: trustHost(strings.TrimSpace(pc.XAddr), xaddr), profile: p.Token}, nil
		}
	}
	return nil, ErrNoPTZ
}

func (p *PTZ) do(ctx context.Context, body string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return p.c.call(ctx, p.service, body, true)
}

func clamp(v float64) float64 { return max(-1, min(1, v)) }

// Move starts moving at the given speeds (-1 to 1) until Stop is called.
func (p *PTZ) Move(ctx context.Context, pan, tilt, zoom float64) error {
	_, err := p.do(ctx, fmt.Sprintf(`<ContinuousMove %s><ProfileToken>%s</ProfileToken><Velocity><PanTilt x="%.2f" y="%.2f" %s/><Zoom x="%.2f" %s/></Velocity></ContinuousMove>`,
		ptzNS, html.EscapeString(p.profile), clamp(pan), clamp(tilt), schemaNS, clamp(zoom), schemaNS))
	return err
}

func (p *PTZ) Stop(ctx context.Context) error {
	_, err := p.do(ctx, fmt.Sprintf(`<Stop %s><ProfileToken>%s</ProfileToken><PanTilt>true</PanTilt><Zoom>true</Zoom></Stop>`, ptzNS, html.EscapeString(p.profile)))
	return err
}

func (p *PTZ) Presets(ctx context.Context) ([]Preset, error) {
	b, err := p.do(ctx, fmt.Sprintf(`<GetPresets %s><ProfileToken>%s</ProfileToken></GetPresets>`, ptzNS, html.EscapeString(p.profile)))
	if err != nil {
		return nil, err
	}
	var r struct {
		P []struct {
			Token string `xml:"token,attr"`
			Name  string `xml:"Name"`
		} `xml:"Body>GetPresetsResponse>Preset"`
	}
	if err := xml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	out := []Preset{}
	for _, x := range r.P {
		name := strings.TrimSpace(x.Name)
		if name == "" {
			name = "Preset " + x.Token
		}
		out = append(out, Preset{x.Token, name})
	}
	return out, nil
}

func (p *PTZ) GotoPreset(ctx context.Context, token string) error {
	_, err := p.do(ctx, fmt.Sprintf(`<GotoPreset %s><ProfileToken>%s</ProfileToken><PresetToken>%s</PresetToken></GotoPreset>`,
		ptzNS, html.EscapeString(p.profile), html.EscapeString(token)))
	return err
}

// SavePreset stores the camera's current position under name.
func (p *PTZ) SavePreset(ctx context.Context, name string) error {
	_, err := p.do(ctx, fmt.Sprintf(`<SetPreset %s><ProfileToken>%s</ProfileToken><PresetName>%s</PresetName></SetPreset>`,
		ptzNS, html.EscapeString(p.profile), html.EscapeString(name)))
	return err
}
