package onvif

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// syncClock measures the camera's clock offset so WS-Security tokens are accepted.
func (c *client) syncClock(ctx context.Context, xaddr string) {
	b, err := c.call(ctx, xaddr, `<GetSystemDateAndTime xmlns="http://www.onvif.org/ver10/device/wsdl"/>`, false)
	if err != nil {
		return
	}
	var r struct {
		Y  int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Date>Year"`
		Mo int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Date>Month"`
		D  int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Date>Day"`
		H  int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Time>Hour"`
		Mi int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Time>Minute"`
		S  int `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime>Time>Second"`
	}
	if xml.Unmarshal(b, &r) == nil && r.Y > 2000 {
		c.skew = time.Date(r.Y, time.Month(r.Mo), r.D, r.H, r.Mi, r.S, 0, time.UTC).Sub(time.Now())
	}
}

// trustHost keeps addr's path and port but forces the host to the one we already
// reached the device on, so a camera can't point us at some other machine.
func trustHost(addr, base string) string {
	u, err := url.Parse(addr)
	b, err2 := url.Parse(base)
	if err != nil || err2 != nil || u.Host == "" {
		return base
	}
	if p := u.Port(); p != "" {
		u.Host = net.JoinHostPort(b.Hostname(), p)
	} else {
		u.Host = b.Hostname()
	}
	return u.String()
}

type pullResp struct {
	Msgs []struct {
		Topic string `xml:"Topic"`
		Items []struct {
			Name  string `xml:"Name,attr"`
			Value string `xml:"Value,attr"`
		} `xml:"Message>Message>Data>SimpleItem"`
	} `xml:"Body>PullMessagesResponse>NotificationMessage"`
}

// motionState reads a motion on/off from a notification, if it is one.
func motionState(topic, name, value string) (state, ok bool) {
	t, n := strings.ToLower(topic), strings.ToLower(name)
	if !strings.Contains(t, "motion") || strings.Contains(t, "tamper") {
		return false, false
	}
	if n != "ismotion" && n != "state" && n != "motion" {
		return false, false
	}
	switch strings.ToLower(value) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	}
	return false, false
}

// WatchMotion subscribes to a camera's ONVIF events and calls onMotion(true or
// false) when the camera reports motion starting or stopping. onReady is called
// once the subscription exists. It returns when ctx ends (nil) or the
// subscription fails (the caller should wait and retry).
func WatchMotion(ctx context.Context, xaddr, user, pass string, onReady func(), onMotion func(bool)) error {
	c := &client{http: &http.Client{Timeout: 25 * time.Second}, user: user, pass: pass}
	c.syncClock(ctx, xaddr)

	events := xaddr
	if b, err := c.call(ctx, xaddr, `<GetCapabilities xmlns="http://www.onvif.org/ver10/device/wsdl"><Category>Events</Category></GetCapabilities>`, true); err != nil {
		if errors.Is(err, ErrAuth) {
			return err
		}
	} else {
		var r struct {
			XAddr string `xml:"Body>GetCapabilitiesResponse>Capabilities>Events>XAddr"`
		}
		if xml.Unmarshal(b, &r) == nil && r.XAddr != "" {
			events = trustHost(r.XAddr, xaddr)
		}
	}
	b, err := c.call(ctx, events, `<CreatePullPointSubscription xmlns="http://www.onvif.org/ver10/events/wsdl"><InitialTerminationTime>PT60S</InitialTerminationTime></CreatePullPointSubscription>`, true)
	if err != nil {
		return fmt.Errorf("subscribing to camera events: %w", err)
	}
	var sr struct {
		Addr string `xml:"Body>CreatePullPointSubscriptionResponse>SubscriptionReference>Address"`
	}
	if xml.Unmarshal(b, &sr) != nil || strings.TrimSpace(sr.Addr) == "" {
		return errors.New("the camera did not return an event subscription")
	}
	pull := trustHost(strings.TrimSpace(sr.Addr), events)
	defer func() { // best effort; the subscription expires anyway
		bg, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c.call(bg, pull, `<Unsubscribe xmlns="http://docs.oasis-open.org/wsn/b-2"/>`, true)
	}()
	if onReady != nil {
		onReady()
	}

	renewAt := time.Now().Add(40 * time.Second)
	for ctx.Err() == nil {
		began := time.Now()
		b, err := c.call(ctx, pull, `<PullMessages xmlns="http://www.onvif.org/ver10/events/wsdl"><Timeout>PT10S</Timeout><MessageLimit>20</MessageLimit></PullMessages>`, true)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("reading camera events: %w", err)
		}
		var pr pullResp
		if xml.Unmarshal(b, &pr) == nil {
			for _, m := range pr.Msgs {
				for _, it := range m.Items {
					if st, ok := motionState(m.Topic, it.Name, it.Value); ok {
						onMotion(st)
					}
				}
			}
		}
		if time.Now().After(renewAt) {
			if _, err := c.call(ctx, pull, `<Renew xmlns="http://docs.oasis-open.org/wsn/b-2"><TerminationTime>PT60S</TerminationTime></Renew>`, true); err != nil && ctx.Err() == nil {
				return fmt.Errorf("renewing the event subscription: %w", err)
			}
			renewAt = time.Now().Add(40 * time.Second)
		}
		if time.Since(began) < time.Second { // some cameras ignore the pull timeout
			select {
			case <-ctx.Done():
			case <-time.After(700 * time.Millisecond):
			}
		}
	}
	return nil
}
