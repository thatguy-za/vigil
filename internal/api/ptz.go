package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/onvif"
)

// A held button must not leave the camera spinning if the phone drops off the network.
const ptzAutoStop = 4 * time.Second

type ptzEntry struct {
	key    string // camera address and login this was opened with
	p      *onvif.PTZ
	err    error
	tried  time.Time
	stopAt *time.Timer
}

var ptzs = struct {
	sync.Mutex
	m map[int64]*ptzEntry
}{m: map[int64]*ptzEntry{}}

// ptzFor returns the camera's PTZ connection, opening it on first use. A camera with no PTZ
// (or one that can't be reached) is remembered for a couple of minutes instead of being asked again.
func ptzFor(ctx context.Context, c db.Camera) (*onvif.PTZ, error) {
	key := c.OnvifURL + "|" + c.URL
	ptzs.Lock()
	e := ptzs.m[c.ID]
	if e != nil && e.key == key && (e.p != nil || time.Since(e.tried) < 2*time.Minute) {
		ptzs.Unlock()
		return e.p, e.err
	}
	ptzs.Unlock()

	u, _ := url.Parse(c.URL)
	user, pass := "", ""
	if u != nil && u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	xaddr := c.OnvifURL
	if xaddr == "" && u != nil {
		if found := onvif.FindDevice(ctx, u.Hostname(), onvif.DefaultPorts); len(found) > 0 {
			xaddr = found[0]
		}
	}
	var p *onvif.PTZ
	var err error
	if xaddr == "" {
		err = onvif.ErrNoPTZ
	} else {
		p, err = onvif.OpenPTZ(ctx, xaddr, user, pass)
	}
	ptzs.Lock()
	defer ptzs.Unlock()
	if old := ptzs.m[c.ID]; old != nil && old.stopAt != nil {
		old.stopAt.Stop()
	}
	ptzs.m[c.ID] = &ptzEntry{key: key, p: p, err: err, tried: time.Now()}
	return p, err
}

func (s *Server) ptzInfo(w http.ResponseWriter, r *http.Request) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	p, err := ptzFor(r.Context(), c)
	if err != nil {
		reason := ""
		if !errors.Is(err, onvif.ErrNoPTZ) {
			reason = err.Error()
		}
		jsonOut(w, 200, map[string]any{"supported": false, "reason": reason})
		return
	}
	presets, _ := p.Presets(r.Context())
	if presets == nil {
		presets = []onvif.Preset{}
	}
	jsonOut(w, 200, map[string]any{"supported": true, "presets": presets})
}

// ptzDo runs one PTZ command for the camera in the URL.
func (s *Server) ptzDo(w http.ResponseWriter, r *http.Request, fn func(db.Camera, *onvif.PTZ) error) {
	c, ok := s.camera(w, r)
	if !ok {
		return
	}
	p, err := ptzFor(r.Context(), c)
	if err != nil {
		fail(w, 409, "this camera can't be moved: "+err.Error())
		return
	}
	if err := fn(c, p); err != nil {
		fail(w, 502, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) ptzMove(w http.ResponseWriter, r *http.Request) {
	var b struct{ X, Y, Zoom float64 }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&b) != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	s.ptzDo(w, r, func(c db.Camera, p *onvif.PTZ) error {
		if err := p.Move(r.Context(), b.X, b.Y, b.Zoom); err != nil {
			return err
		}
		ptzs.Lock()
		defer ptzs.Unlock()
		if e := ptzs.m[c.ID]; e != nil {
			if e.stopAt != nil {
				e.stopAt.Stop()
			}
			e.stopAt = time.AfterFunc(ptzAutoStop, func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				p.Stop(ctx)
			})
		}
		return nil
	})
}

func (s *Server) ptzStop(w http.ResponseWriter, r *http.Request) {
	s.ptzDo(w, r, func(_ db.Camera, p *onvif.PTZ) error { return p.Stop(r.Context()) })
}

func (s *Server) ptzGoto(w http.ResponseWriter, r *http.Request) {
	var b struct{ Token string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&b) != nil || b.Token == "" {
		fail(w, 400, "choose a preset")
		return
	}
	s.ptzDo(w, r, func(_ db.Camera, p *onvif.PTZ) error { return p.GotoPreset(r.Context(), b.Token) })
}

func (s *Server) ptzSave(w http.ResponseWriter, r *http.Request) {
	var b struct{ Name string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&b) != nil || strings.TrimSpace(b.Name) == "" || len(b.Name) > 60 {
		fail(w, 400, "give the preset a name of up to 60 characters")
		return
	}
	s.ptzDo(w, r, func(_ db.Camera, p *onvif.PTZ) error { return p.SavePreset(r.Context(), strings.TrimSpace(b.Name)) })
}
