package db

import "time"

// HealthEvent is a change in a camera's state: online, offline or stopped
// (stopped on purpose: schedule ended, mode changed, Vigil shutting down).
type HealthEvent struct {
	ID       int64     `json:"id"`
	CameraID int64     `json:"camera_id"`
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Detail   string    `json:"detail"`
}

func (d *DB) AddHealth(cam int64, kind, detail string) {
	d.Exec(`INSERT INTO health_events (camera_id,ts,kind,detail) VALUES (?,?,?,?)`, cam, time.Now().Unix(), kind, detail)
}

// HealthSince returns events from all cameras since t, oldest first.
func (d *DB) HealthSince(t time.Time) ([]HealthEvent, error) {
	rows, err := d.Query(`SELECT id,camera_id,ts,kind,detail FROM health_events WHERE ts>=? ORDER BY ts,id`, t.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HealthEvent{}
	for rows.Next() {
		var e HealthEvent
		var ts int64
		if err := rows.Scan(&e.ID, &e.CameraID, &ts, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.Time = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// HealthKindBefore is the camera's last event kind before t ("" if none).
func (d *DB) HealthKindBefore(cam int64, t time.Time) string {
	var k string
	d.QueryRow(`SELECT kind FROM health_events WHERE camera_id=? AND ts<? ORDER BY ts DESC,id DESC LIMIT 1`, cam, t.Unix()).Scan(&k)
	return k
}

func (d *DB) PruneHealth(before time.Time) {
	d.Exec(`DELETE FROM health_events WHERE ts<?`, before.Unix())
}

// UptimePct is the share of monitored time a camera spent online in [from, to].
// startKind is its last event before from. It returns -1 if it wasn't monitored at all.
func UptimePct(startKind string, evs []HealthEvent, from, to time.Time) float64 {
	var up, down time.Duration
	state, cursor := startKind, from
	add := func(until time.Time) {
		switch state {
		case "online":
			up += until.Sub(cursor)
		case "offline":
			down += until.Sub(cursor)
		}
		cursor = until
	}
	for _, e := range evs {
		if e.Time.Before(from) || e.Time.After(to) {
			continue
		}
		add(e.Time)
		state = e.Kind
	}
	add(to)
	if up+down <= 0 {
		return -1
	}
	return float64(up) * 100 / float64(up+down)
}
