package db

import "strconv"

// ProtectedClips returns the names of a camera's clips that retention must not delete.
func (d *DB) ProtectedClips(cam int64) map[string]bool {
	out := map[string]bool{}
	rows, err := d.Query(`SELECT name FROM protected_clips WHERE camera_id=?`, cam)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out[n] = true
		}
	}
	return out
}

func (d *DB) SetProtected(cam int64, name string, on bool) error {
	var err error
	if on {
		_, err = d.Exec(`INSERT OR IGNORE INTO protected_clips (camera_id,name) VALUES (?,?)`, cam, name)
	} else {
		_, err = d.Exec(`DELETE FROM protected_clips WHERE camera_id=? AND name=?`, cam, name)
	}
	return err
}

// ForgetClip removes what the database knows about a deleted clip: its
// protection and the clip name on any motion events.
func (d *DB) ForgetClip(cam int64, name string) {
	d.Exec(`DELETE FROM protected_clips WHERE camera_id=? AND name=?`, cam, name)
	d.Exec(`UPDATE events SET clip='' WHERE camera_id=? AND clip=?`, cam, name)
}

// SetInt stores a setting.
func (d *DB) SetInt(key string, v int) error {
	_, err := d.Exec(`INSERT INTO settings (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, strconv.Itoa(v))
	return err
}
