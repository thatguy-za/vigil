package db

import (
	"database/sql"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

type DB struct{ *sql.DB }

// Camera schedule is 168 chars of 0/1, index = weekday*24 + hour (Sunday = 0).
type Camera struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	SubURL        string `json:"sub_url"`
	Mode          string `json:"mode"` // off | continuous | schedule | motion
	Schedule      string `json:"schedule"`
	Transcode     bool   `json:"transcode"`
	MaxHeight     int    `json:"max_height"`
	Audio         bool   `json:"audio"`
	RetentionDays int    `json:"retention_days"` // 0 = global default
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
	retention_days INTEGER NOT NULL DEFAULT 0
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
	return &DB{d}, nil
}

const cols = `id,name,url,sub_url,mode,schedule,transcode,max_height,audio,retention_days`

func scan(r interface{ Scan(...any) error }) (Camera, error) {
	var c Camera
	err := r.Scan(&c.ID, &c.Name, &c.URL, &c.SubURL, &c.Mode, &c.Schedule,
		&c.Transcode, &c.MaxHeight, &c.Audio, &c.RetentionDays)
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
	res, err := d.Exec(`INSERT INTO cameras (name,url,sub_url,mode,schedule,transcode,max_height,audio,retention_days)
		VALUES (?,?,?,?,?,?,?,?,?)`, c.Name, c.URL, c.SubURL, c.Mode, c.Schedule, c.Transcode, c.MaxHeight, c.Audio, c.RetentionDays)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UpdateCamera(c Camera) error {
	_, err := d.Exec(`UPDATE cameras SET name=?,url=?,sub_url=?,mode=?,schedule=?,transcode=?,max_height=?,audio=?,retention_days=? WHERE id=?`,
		c.Name, c.URL, c.SubURL, c.Mode, c.Schedule, c.Transcode, c.MaxHeight, c.Audio, c.RetentionDays, c.ID)
	return err
}

func (d *DB) DeleteCamera(id int64) error {
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
