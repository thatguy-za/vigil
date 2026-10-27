package db

import (
	"errors"
	"strings"
	"time"
)

var ErrExists = errors.New("already exists")

type User struct {
	ID       int64     `json:"id"`
	Username string    `json:"username"`
	Role     string    `json:"role"` // admin | viewer
	Created  time.Time `json:"created"`
	Hash     string    `json:"-"`
}

const userCols = `id,username,role,created,pass_hash`

func scanUser(r interface{ Scan(...any) error }) (User, error) {
	var u User
	var ts int64
	err := r.Scan(&u.ID, &u.Username, &u.Role, &ts, &u.Hash)
	u.Created = time.Unix(ts, 0)
	return u, err
}

func (d *DB) UserCount() int {
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

func (d *DB) AdminCount() int {
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n)
	return n
}

// CreateFirstAdmin creates the initial admin, and only if there are no users yet.
func (d *DB) CreateFirstAdmin(name, hash string) (User, error) {
	res, err := d.Exec(`INSERT INTO users (username,pass_hash,role,created)
		SELECT ?,?,'admin',? WHERE NOT EXISTS (SELECT 1 FROM users)`, name, hash, time.Now().Unix())
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrExists
	}
	id, _ := res.LastInsertId()
	return d.UserByID(id)
}

func (d *DB) CreateUser(name, hash, role string) (User, error) {
	res, err := d.Exec(`INSERT INTO users (username,pass_hash,role,created) VALUES (?,?,?,?)`, name, hash, role, time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, ErrExists
		}
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return d.UserByID(id)
}

func (d *DB) UserByID(id int64) (User, error) {
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (d *DB) UserByName(name string) (User, error) {
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE username=?`, name))
}

func (d *DB) Users() ([]User, error) {
	rows, err := d.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (d *DB) SetRole(id int64, role string) error {
	_, err := d.Exec(`UPDATE users SET role=? WHERE id=?`, role, id)
	return err
}

func (d *DB) SetPassword(id int64, hash string) error {
	_, err := d.Exec(`UPDATE users SET pass_hash=? WHERE id=?`, hash, id)
	return err
}

func (d *DB) DeleteUser(id int64) error {
	d.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	_, err := d.Exec(`DELETE FROM users WHERE id=?`, id)
	return err
}

func (d *DB) CreateSession(tokenHash string, uid int64, expires time.Time) error {
	d.Exec(`DELETE FROM sessions WHERE expires<?`, time.Now().Unix())
	_, err := d.Exec(`INSERT INTO sessions (token_hash,user_id,expires) VALUES (?,?,?)`, tokenHash, uid, expires.Unix())
	return err
}

// SessionUser returns the user for an unexpired session token hash.
func (d *DB) SessionUser(tokenHash string) (User, error) {
	return scanUser(d.QueryRow(`SELECT u.id,u.username,u.role,u.created,u.pass_hash FROM sessions s
		JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires>?`, tokenHash, time.Now().Unix()))
}

func (d *DB) DeleteSession(tokenHash string) {
	d.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash)
}

// DeleteUserSessions signs a user out everywhere except the session keep (may be "").
func (d *DB) DeleteUserSessions(uid int64, keep string) {
	d.Exec(`DELETE FROM sessions WHERE user_id=? AND token_hash!=?`, uid, keep)
}
