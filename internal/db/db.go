package db

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct{ *sql.DB }

// Camera schedule is 168 chars of 0/1, index = weekday*24 + hour (Sunday = 0).
type Camera struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	URL               string `json:"url"`
	SubURL            string `json:"sub_url"`
	Mode              string `json:"mode"` // off | continuous | schedule | motion
	Schedule          string `json:"schedule"`
	Transcode         bool   `json:"transcode"`
	MaxHeight         int    `json:"max_height"`
	Audio             bool   `json:"audio"`
	RetentionDays     int    `json:"retention_days"`     // 0 = global default
	MotionSensitivity int    `json:"motion_sensitivity"` // 1 (low) .. 10 (high)
	MotionMask        string `json:"motion_mask"`        // 16x9 grid, row-major, 1 = ignore; "" = none
	MaxGB             int    `json:"max_gb"`             // size limit for this camera's recordings; 0 = none
	MotionSource      string `json:"motion_source"`      // vigil | camera | both
	OnvifURL          string `json:"onvif_url"`          // ONVIF device service, for camera motion events
	LowRes            bool   `json:"low_res"`            // also record the sub-stream continuously
}

const schema = `
CREATE TABLE IF NOT EXISTS cameras (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	url TEXT NOT NULL,
	sub_url TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL DEFAULT 'continuous',
	schedule TEXT NOT NULL DEFAULT '',
	transcode INTEGER NOT NULL DEFAULT 0,
	max_height INTEGER NOT NULL DEFAULT 720,
	audio INTEGER NOT NULL DEFAULT 0,
	retention_days INTEGER NOT NULL DEFAULT 0,
	motion_sensitivity INTEGER NOT NULL DEFAULT 5,
	motion_mask TEXT NOT NULL DEFAULT '',
	max_gb INTEGER NOT NULL DEFAULT 0,
	motion_source TEXT NOT NULL DEFAULT 'vigil',
	onvif_url TEXT NOT NULL DEFAULT '',
	low_res INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS protected_clips (
	camera_id INTEGER NOT NULL,
	name TEXT NOT NULL,
	PRIMARY KEY (camera_id, name)
);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	camera_id INTEGER NOT NULL,
	start_ts INTEGER NOT NULL,
	end_ts INTEGER NOT NULL,
	peak REAL NOT NULL DEFAULT 0,
	clip TEXT NOT NULL DEFAULT '',
	clip_dur REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS events_cam_start ON events(camera_id, start_ts);
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE COLLATE NOCASE,
	pass_hash TEXT NOT NULL,
	role TEXT NOT NULL, -- admin | viewer
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL,
	expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS health_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	camera_id INTEGER NOT NULL,
	ts INTEGER NOT NULL,
	kind TEXT NOT NULL, -- online | offline | stopped
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS health_cam_ts ON health_events(camera_id, ts);
CREATE TABLE IF NOT EXISTS discovered (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	hardware TEXT NOT NULL DEFAULT '',
	host TEXT NOT NULL,
	xaddr TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'new', -- new | dismissed | added
	first_seen INTEGER NOT NULL,
	camera_id INTEGER NOT NULL DEFAULT 0 -- camera created from this device
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

func Open(dir string) (*DB, error) {
	d, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "vigil.db")+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(4)
	if _, err := d.Exec(schema); err != nil {
		return nil, err
	}
	// databases created before motion detection lack this column
	d.Exec(`ALTER TABLE cameras ADD COLUMN motion_sensitivity INTEGER NOT NULL DEFAULT 5`)
	d.Exec(`ALTER TABLE cameras ADD COLUMN motion_mask TEXT NOT NULL DEFAULT ''`)
	d.Exec(`ALTER TABLE cameras ADD COLUMN max_gb INTEGER NOT NULL DEFAULT 0`)
	d.Exec(`ALTER TABLE cameras ADD COLUMN motion_source TEXT NOT NULL DEFAULT 'vigil'`)
	d.Exec(`ALTER TABLE cameras ADD COLUMN onvif_url TEXT NOT NULL DEFAULT ''`)
	d.Exec(`ALTER TABLE cameras ADD COLUMN low_res INTEGER NOT NULL DEFAULT 0`)
	d.Exec(`ALTER TABLE discovered ADD COLUMN camera_id INTEGER NOT NULL DEFAULT 0`)
	return &DB{d}, nil
}

const cols = `id,name,url,sub_url,mode,schedule,transcode,max_height,audio,retention_days,motion_sensitivity,motion_mask,max_gb,motion_source,onvif_url,low_res`

func scan(r interface{ Scan(...any) error }) (Camera, error) {
	var c Camera
	err := r.Scan(&c.ID, &c.Name, &c.URL, &c.SubURL, &c.Mode, &c.Schedule,
		&c.Transcode, &c.MaxHeight, &c.Audio, &c.RetentionDays, &c.MotionSensitivity, &c.MotionMask, &c.MaxGB, &c.MotionSource, &c.OnvifURL, &c.LowRes)
	return c, err
}

func (d *DB) Cameras() ([]Camera, error) {
	rows, err := d.Query(`SELECT ` + cols + ` FROM cameras ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Camera{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) Camera(id int64) (Camera, error) {
	return scan(d.QueryRow(`SELECT `+cols+` FROM cameras WHERE id=?`, id))
}

func (d *DB) CreateCamera(c Camera) (int64, error) {
	res, err := d.Exec(`INSERT INTO cameras (name,url,sub_url,mode,schedule,transcode,max_height,audio,retention_days,motion_sensitivity,motion_mask,max_gb,motion_source,onvif_url,low_res)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.Name, c.URL, c.SubURL, c.Mode, c.Schedule, c.Transcode, c.MaxHeight, c.Audio, c.RetentionDays, c.MotionSensitivity, c.MotionMask, c.MaxGB, c.MotionSource, c.OnvifURL, c.LowRes)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UpdateCamera(c Camera) error {
	_, err := d.Exec(`UPDATE cameras SET name=?,url=?,sub_url=?,mode=?,schedule=?,transcode=?,max_height=?,audio=?,retention_days=?,motion_sensitivity=?,motion_mask=?,max_gb=?,motion_source=?,onvif_url=?,low_res=? WHERE id=?`,
		c.Name, c.URL, c.SubURL, c.Mode, c.Schedule, c.Transcode, c.MaxHeight, c.Audio, c.RetentionDays, c.MotionSensitivity, c.MotionMask, c.MaxGB, c.MotionSource, c.OnvifURL, c.LowRes, c.ID)
	return err
}

func (d *DB) DeleteCamera(id int64) error {
	if _, err := d.Exec(`DELETE FROM events WHERE camera_id=?`, id); err != nil {
		return err
	}
	d.Exec(`DELETE FROM health_events WHERE camera_id=?`, id)
	d.Exec(`DELETE FROM protected_clips WHERE camera_id=?`, id)
	_, err := d.Exec(`DELETE FROM cameras WHERE id=?`, id)
	return err
}

func (d *DB) Int(key string, def int) int {
	var v string
	if d.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v) != nil {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Event is one period of detected motion and the clip that was saved for it.
type Event struct {
	ID       int64     `json:"id"`
	CameraID int64     `json:"camera_id"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Peak     float64   `json:"peak"`     // largest % of the frame that changed
	Clip     string    `json:"clip"`     // file name inside the camera folder, "" if none was saved
	ClipDur  float64   `json:"duration"` // clip length in seconds
}

func (d *DB) AddEvent(e Event) error {
	_, err := d.Exec(`INSERT INTO events (camera_id,start_ts,end_ts,peak,clip,clip_dur) VALUES (?,?,?,?,?,?)`,
		e.CameraID, e.Start.Unix(), e.End.Unix(), e.Peak, e.Clip, e.ClipDur)
	return err
}

// Events returns a camera's events that start in [from, to), oldest first.
func (d *DB) Events(cam int64, from, to time.Time) ([]Event, error) {
	rows, err := d.Query(`SELECT id,camera_id,start_ts,end_ts,peak,clip,clip_dur FROM events
		WHERE camera_id=? AND start_ts>=? AND start_ts<? ORDER BY start_ts`, cam, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var s, en int64
		if err := rows.Scan(&e.ID, &e.CameraID, &s, &en, &e.Peak, &e.Clip, &e.ClipDur); err != nil {
			return nil, err
		}
		e.Start, e.End = time.Unix(s, 0), time.Unix(en, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) DeleteEventsBefore(cam int64, t time.Time) {
	d.Exec(`DELETE FROM events WHERE camera_id=? AND start_ts<?`, cam, t.Unix())
}

// Discovered is a camera found on the network. Status "new" cameras are offered
// on the home page; "dismissed" ones only on the add-camera page; "added" are hidden.
type Discovered struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hardware  string    `json:"hardware"`
	Host      string    `json:"host"`
	XAddr     string    `json:"-"`
	Status    string    `json:"status"`
	FirstSeen time.Time `json:"first_seen"`
}

// UpsertDiscovered inserts a device or refreshes its address; an existing status is kept.
func (d *DB) UpsertDiscovered(x Discovered) error {
	_, err := d.Exec(`INSERT INTO discovered (id,name,hardware,host,xaddr,status,first_seen) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, hardware=excluded.hardware, host=excluded.host, xaddr=excluded.xaddr`,
		x.ID, x.Name, x.Hardware, x.Host, x.XAddr, x.Status, time.Now().Unix())
	return err
}

func (d *DB) DiscoveredByID(id string) (Discovered, error) {
	var x Discovered
	var ts int64
	err := d.QueryRow(`SELECT id,name,hardware,host,xaddr,status,first_seen FROM discovered WHERE id=?`, id).
		Scan(&x.ID, &x.Name, &x.Hardware, &x.Host, &x.XAddr, &x.Status, &ts)
	x.FirstSeen = time.Unix(ts, 0)
	return x, err
}

// Discovered lists devices that are not yet added, newest first.
func (d *DB) Discovered() ([]Discovered, error) {
	rows, err := d.Query(`SELECT id,name,hardware,host,xaddr,status,first_seen FROM discovered WHERE status!='added' ORDER BY first_seen DESC, host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Discovered{}
	for rows.Next() {
		var x Discovered
		var ts int64
		if err := rows.Scan(&x.ID, &x.Name, &x.Hardware, &x.Host, &x.XAddr, &x.Status, &ts); err != nil {
			return nil, err
		}
		x.FirstSeen = time.Unix(ts, 0)
		out = append(out, x)
	}
	return out, rows.Err()
}

// DismissDiscovered hides a "new" device from the home page.
func (d *DB) DismissDiscovered(id string) error {
	_, err := d.Exec(`UPDATE discovered SET status='dismissed' WHERE id=? AND status='new'`, id)
	return err
}

// LinkDiscovered records that camera cam was created from device id.
func (d *DB) LinkDiscovered(id string, cam int64) {
	d.Exec(`UPDATE discovered SET status='added', camera_id=? WHERE id=?`, cam, id)
}

// MarkAdded flags every device at host as added (a camera now exists for it).
func (d *DB) MarkAdded(host string) {
	d.Exec(`UPDATE discovered SET status='added' WHERE host=? AND status!='added'`, host)
}

// ReleaseCamera makes the device a camera was created from offerable again.
func (d *DB) ReleaseCamera(cam int64) {
	d.Exec(`UPDATE discovered SET status='dismissed', camera_id=0 WHERE camera_id=?`, cam)
}

// ReleaseAdded makes an added device offerable again (on the add page) after its camera is deleted.
func (d *DB) ReleaseAdded(host string) {
	d.Exec(`UPDATE discovered SET status='dismissed' WHERE host=? AND status='added'`, host)
}
