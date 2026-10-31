package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thatguy-za/vigil/internal/auth"
	"github.com/thatguy-za/vigil/internal/db"
)

const (
	cookieName = "vigil_session"
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{3,32}$`)

type ctxKey struct{}

func userFrom(r *http.Request) db.User {
	u, _ := r.Context().Value(ctxKey{}).(db.User)
	return u
}

func publicUser(u db.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "role": u.Role}
}

func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && (u.Host == r.Host || u.Host == r.Header.Get("X-Forwarded-Host"))
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// sessionUser returns the signed-in user and the hash of their session token.
func (s *Server) sessionUser(r *http.Request) (db.User, string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return db.User{}, "", false
	}
	h := auth.HashToken(c.Value)
	u, err := s.DB.SessionUser(h)
	return u, h, err == nil
}

// require wraps a handler so only signed-in users (or only admins) can call it.
func (s *Server) require(admin bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, _, ok := s.sessionUser(r)
		switch {
		case !ok:
			fail(w, 401, "Sign in required")
		case r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r):
			fail(w, 403, "Cross-site request blocked")
		case admin && u.Role != "admin":
			fail(w, 403, "Only admins can do this")
		default:
			h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
		}
	}
}

// open wraps public auth endpoints (login, setup) with the cross-site check.
func (s *Server) open(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			fail(w, 403, "Cross-site request blocked")
			return
		}
		h(w, r)
	}
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u db.User) error {
	token, hash := auth.NewToken()
	exp := time.Now().Add(time.Duration(s.DB.Int("session_days", 30)) * 24 * time.Hour)
	if err := s.DB.CreateSession(hash, u.ID, exp); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", Expires: exp, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(v) != nil {
		fail(w, 400, "invalid JSON")
		return false
	}
	return true
}

func validCreds(w http.ResponseWriter, name, pass string) bool {
	switch {
	case !usernameRe.MatchString(name):
		fail(w, 400, "Username must be 3-32 letters, numbers, dots, dashes or underscores")
	case len(pass) < 8 || len(pass) > 200:
		fail(w, 400, "Password must be at least 8 characters")
	default:
		return true
	}
	return false
}

// status tells the UI whether to show setup, login, or the app.
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"setup_required": s.DB.UserCount() == 0}
	if u, _, ok := s.sessionUser(r); ok {
		out["user"], out["version"] = publicUser(u), s.Version
	}
	jsonOut(w, 200, out)
}

// setup creates the first admin. It only works while there are no users.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var b struct{ Username, Password string }
	if !decode(w, r, &b) || !validCreds(w, b.Username, b.Password) {
		return
	}
	u, err := s.DB.CreateFirstAdmin(b.Username, auth.Hash(b.Password))
	if errors.Is(err, db.ErrExists) {
		fail(w, 409, "Setup has already been completed")
		return
	}
	if err != nil || s.startSession(w, r, u) != nil {
		fail(w, 500, "could not create the account")
		return
	}
	jsonOut(w, 200, publicUser(u))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var b struct{ Username, Password string }
	if !decode(w, r, &b) {
		return
	}
	ipKey, userKey := "ip:"+clientIP(r), "user:"+strings.ToLower(b.Username)
	if !s.lim.Allow(ipKey) || !s.lim.Allow(userKey) {
		fail(w, 429, "Too many failed attempts. Try again in a few minutes.")
		return
	}
	u, err := s.DB.UserByName(b.Username)
	hash := auth.DummyHash // same work whether or not the user exists
	if err == nil {
		hash = u.Hash
	}
	if !auth.Verify(hash, b.Password) || err != nil {
		s.lim.Fail(ipKey)
		s.lim.Fail(userKey)
		fail(w, 401, "Wrong username or password")
		return
	}
	s.lim.Reset(userKey)
	if s.startSession(w, r, u) != nil {
		fail(w, 500, "could not sign in")
		return
	}
	jsonOut(w, 200, publicUser(u))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, h, ok := s.sessionUser(r); ok {
		s.DB.DeleteSession(h)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	w.WriteHeader(204)
}

// changePassword lets a user change their own password and signs out their other devices.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var b struct{ Current, New string }
	if !decode(w, r, &b) {
		return
	}
	u := userFrom(r)
	if len(b.New) < 8 || len(b.New) > 200 {
		fail(w, 400, "Password must be at least 8 characters")
		return
	}
	if !s.lim.Allow("pw:"+u.Username) || !auth.Verify(u.Hash, b.Current) {
		s.lim.Fail("pw:" + u.Username)
		fail(w, 401, "Current password is wrong")
		return
	}
	if err := s.DB.SetPassword(u.ID, auth.Hash(b.New)); err != nil {
		fail(w, 500, err.Error())
		return
	}
	_, keep, _ := s.sessionUser(r)
	s.DB.DeleteUserSessions(u.ID, keep)
	w.WriteHeader(204)
}

// ---------- admin: manage users ----------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	us, err := s.DB.Users()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	jsonOut(w, 200, us)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var b struct{ Username, Password, Role string }
	if !decode(w, r, &b) || !validCreds(w, b.Username, b.Password) {
		return
	}
	if b.Role != "admin" && b.Role != "viewer" {
		fail(w, 400, "Role must be admin or viewer")
		return
	}
	u, err := s.DB.CreateUser(b.Username, auth.Hash(b.Password), b.Role)
	if errors.Is(err, db.ErrExists) {
		fail(w, 409, "That username is taken")
		return
	}
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	jsonOut(w, 200, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var b struct{ Role, Password string }
	if !decode(w, r, &b) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.userMu.Lock()
	defer s.userMu.Unlock()
	t, err := s.DB.UserByID(id)
	if err != nil {
		fail(w, 404, "user not found")
		return
	}
	me := userFrom(r)
	if b.Role != "" && b.Role != t.Role {
		if b.Role != "admin" && b.Role != "viewer" {
			fail(w, 400, "Role must be admin or viewer")
			return
		}
		if t.Role == "admin" && s.DB.AdminCount() <= 1 {
			fail(w, 400, "There must be at least one admin")
			return
		}
		if err := s.DB.SetRole(id, b.Role); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if b.Password != "" {
		if len(b.Password) < 8 || len(b.Password) > 200 {
			fail(w, 400, "Password must be at least 8 characters")
			return
		}
		if err := s.DB.SetPassword(id, auth.Hash(b.Password)); err != nil {
			fail(w, 500, err.Error())
			return
		}
		keep := ""
		if id == me.ID {
			_, keep, _ = s.sessionUser(r)
		}
		s.DB.DeleteUserSessions(id, keep)
	}
	t, _ = s.DB.UserByID(id)
	jsonOut(w, 200, t)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.userMu.Lock()
	defer s.userMu.Unlock()
	t, err := s.DB.UserByID(id)
	switch {
	case err != nil:
		fail(w, 404, "user not found")
	case id == userFrom(r).ID:
		fail(w, 400, "You can't delete your own account")
	case t.Role == "admin" && s.DB.AdminCount() <= 1:
		fail(w, 400, "There must be at least one admin")
	default:
		if err := s.DB.DeleteUser(id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		w.WriteHeader(204)
	}
}

// cameraFor builds the camera JSON for the caller. Viewers never see stream
// URLs, which contain the camera's login.
func (s *Server) cameraFor(r *http.Request, c db.Camera) cameraView {
	v := cameraView{c, s.Rec.Status(c)}
	if userFrom(r).Role != "admin" {
		v.URL, v.SubURL = "", ""
	}
	return v
}
