// Package discover keeps the list of ONVIF cameras on the network up to date.
package discover

import (
	"context"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/onvif"
)

type Scanner struct {
	DB *db.DB
	mu sync.Mutex
}

// Scan probes the network once. Devices already used by a camera are stored as added.
func (s *Scanner) Scan(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	devs, err := onvif.Discover(ctx, 4*time.Second, onvif.MulticastAddr)
	if err != nil {
		return err
	}
	cams, _ := s.DB.Cameras()
	used := map[string]bool{}
	for _, c := range cams {
		for _, raw := range []string{c.URL, c.SubURL} {
			if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
				used[u.Hostname()] = true
			}
		}
	}
	for _, d := range devs {
		st := "new"
		if used[d.Host] {
			st = "added"
		}
		if err := s.DB.UpsertDiscovered(db.Discovered{ID: d.ID, Name: d.Name, Hardware: d.Hardware, Host: d.Host, XAddr: d.XAddr, Status: st}); err != nil {
			return err
		}
	}
	for h := range used {
		s.DB.MarkAdded(h)
	}
	return nil
}

func (s *Scanner) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if err := s.Scan(ctx); err != nil {
			log.Printf("discover: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
