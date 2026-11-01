package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joaoopereira/simple-rtsp-recorder/internal/config"
)

const (
	sessionCookie   = "srr_session"
	sessionDuration = 7 * 24 * time.Hour
	sessionKeyFile  = "session.key"
	maxFailures     = 5
	lockoutDuration = 15 * time.Minute
)

// auth issues and checks user sessions. A session is a cookie
// "<user id>.<expiry>.<hmac>" signed with a random key kept in session.key, so
// sessions survive restarts and updates. The signature also covers the user's
// password hash: changing a password logs that user out everywhere else.
type auth struct {
	key []byte

	mu       sync.Mutex
	failures map[string]*failure
}

type failure struct {
	count int
	first time.Time
}

func newAuth(dir string) (*auth, error) {
	key, err := loadSessionKey(filepath.Join(dir, sessionKeyFile))
	return &auth{key: key, failures: map[string]*failure{}}, err
}

// loadSessionKey reads the signing key, creating it on first use. If it can't
// be read or saved, a random key is still returned: sessions then only last
// until the next restart.
func loadSessionKey(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		if key, err := hex.DecodeString(strings.TrimSpace(string(data))); err == nil && len(key) >= 32 {
			return key, nil
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key, os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600)
}

func (a *auth) sign(userID string, expiry int64, passwordHash string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(userID + "|" + strconv.FormatInt(expiry, 10) + "|" + passwordHash))
	return hex.EncodeToString(m.Sum(nil))
}

// user returns the logged-in user, looked up again on every request so that a
// deleted user or a changed role takes effect at once.
func (a *auth) user(r *http.Request, store *config.Store) (config.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return config.User{}, false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return config.User{}, false
	}
	expiry, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return config.User{}, false
	}
	u, ok := store.UserByID(parts[0])
	if !ok || u.PasswordHash == "" || !hmac.Equal([]byte(parts[2]), []byte(a.sign(u.ID, expiry, u.PasswordHash))) {
		return config.User{}, false
	}
	return u, true
}

func (a *auth) setSession(w http.ResponseWriter, r *http.Request, u config.User) {
	expiry := time.Now().Add(sessionDuration).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    u.ID + "." + strconv.FormatInt(expiry, 10) + "." + a.sign(u.ID, expiry, u.PasswordHash),
		Path:     "/",
		MaxAge:   int(sessionDuration.Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode})
}

// isHTTPS is true behind the Cloudflare Tunnel, which talks plain HTTP to us.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// clientIP identifies the client for the login lockout. Behind the tunnel
// every request comes from localhost (cloudflared), so Cloudflare's header is
// used then — and only then, so LAN clients can't fake it.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := r.Header.Get("Cf-Connecting-Ip"); cf != "" {
			return cf
		}
	}
	return host
}

// locked reports whether an IP has too many recent failed logins.
func (a *auth) locked(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[ip]
	if f == nil {
		return false
	}
	if time.Since(f.first) > lockoutDuration {
		delete(a.failures, ip)
		return false
	}
	return f.count >= maxFailures
}

func (a *auth) fail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[ip]
	if f == nil || time.Since(f.first) > lockoutDuration {
		f = &failure{first: time.Now()}
		a.failures[ip] = f
	}
	f.count++
}

func (a *auth) succeed(ip string) {
	a.mu.Lock()
	delete(a.failures, ip)
	a.mu.Unlock()
}

var (
	errLoginRequired      = errors.New("é preciso iniciar sessão")
	errForbidden          = errors.New("não tem permissão para isto")
	errMustChangePassword = errors.New("tem de mudar a palavra-passe antes de continuar")
)

// access is what an endpoint needs.
type access int

const (
	// accessRecorder: any logged-in user. While there are no users at all
	// (fresh install, nothing migrated) it is open to everyone, as before
	// 2.7, so an automatic update never locks the owner out.
	accessRecorder access = iota
	// accessUsers: admins and user managers.
	accessUsers
	// accessAdmin: admins only.
	accessAdmin
	// accessAccount: any logged-in user, even one who must still change the
	// password someone else chose (see User.MustChangePassword). Only for
	// changing that password.
	accessAccount
)

func allowed(role config.Role, level access) bool {
	switch level {
	case accessRecorder, accessAccount:
		return true
	case accessUsers:
		return role.CanManageUsers()
	default:
		return role == config.RoleAdmin
	}
}

type userKey struct{}

// require wraps a handler with an access check. The handler finds the user,
// if any, with userFrom.
func (s *Server) require(level access, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		switch {
		case !ok && level == accessRecorder && !s.Store.HasUsers():
			h(w, r)
		case !ok:
			writeError(w, http.StatusUnauthorized, errLoginRequired)
		case u.MustChangePassword && level != accessAccount:
			writeError(w, http.StatusForbidden, errMustChangePassword)
		case !allowed(u.Role, level):
			writeError(w, http.StatusForbidden, errForbidden)
		default:
			h(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
		}
	}
}

// localUser is whoever uses the browser on the PC itself: an admin without a
// login (the owner asked for this; whoever sits at the PC owns it anyway).
var localUser = config.User{ID: "local", Email: "este PC", Role: config.RoleAdmin}

// currentUser is the local admin on the PC itself, or the user of the session.
func (s *Server) currentUser(r *http.Request) (config.User, bool) {
	if s.TrustLocal && isLocal(r) {
		return localUser, true
	}
	return s.auth.user(r, s.Store)
}

// isLocal reports whether a request comes from a browser on the PC itself.
// cloudflared runs on the same PC, so every request from the internet also
// arrives from loopback: the Cloudflare headers and the Host tell them apart
// (Cloudflare always adds Cf-Connecting-Ip, and the Host is the public name).
// A web page open on the PC could also send requests to localhost (CSRF), so
// a change needs the page's own Origin.
func isLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return false
	}
	for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "Cdn-Loop", "X-Forwarded-For", "X-Forwarded-Host"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	name := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		name = h
	}
	if ip := net.ParseIP(strings.Trim(name, "[]")); name != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false // also stops DNS rebinding
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			return false
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			return false
		}
	}
	return true
}

func userFrom(r *http.Request) (config.User, bool) {
	u, ok := r.Context().Value(userKey{}).(config.User)
	return u, ok
}

// userDTO is a user as the API shows it: never the password hash.
type userDTO struct {
	ID                 string      `json:"id"`
	Email              string      `json:"email"`
	Role               config.Role `json:"role"`
	MustChangePassword bool        `json:"mustChangePassword"`
}

func toUserDTO(u config.User) userDTO { return userDTO{u.ID, u.Email, u.Role, u.MustChangePassword} }

type meDTO struct {
	// AuthRequired is false while there are no users: the recorder is open.
	AuthRequired bool     `json:"authRequired"`
	User         *userDTO `json:"user"`
	// Local is true on the PC itself (see isLocal): admin without a login.
	Local bool `json:"local,omitempty"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	resp := meDTO{AuthRequired: s.Store.HasUsers()}
	if s.TrustLocal && isLocal(r) {
		d := toUserDTO(localUser)
		resp.User, resp.Local = &d, true
	} else if u, ok := s.auth.user(r, s.Store); ok {
		// The page asks on every load: renewing here keeps someone who uses
		// the site at least once a week logged in.
		s.auth.setSession(w, r, u)
		d := toUserDTO(u)
		resp.User = &d
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.auth.locked(ip) {
		writeError(w, http.StatusTooManyRequests, errors.New("demasiadas tentativas falhadas; tente daqui a 15 minutos"))
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !s.Store.HasUsers() {
		writeError(w, http.StatusForbidden, errors.New("ainda não existe nenhum utilizador; crie um administrador no PC com: simple-rtsp-recorder.exe add-user <email> admin"))
		return
	}
	u, err := s.Store.Authenticate(body.Email, body.Password)
	if err != nil {
		s.auth.fail(ip)
		s.Log.Warn("login failed", "email", body.Email, "ip", ip)
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	s.auth.succeed(ip)
	s.auth.setSession(w, r, u)
	s.Log.Info("logged in", "email", u.Email, "role", u.Role, "ip", ip)
	d := toUserDTO(u)
	writeJSON(w, http.StatusOK, meDTO{AuthRequired: true, User: &d})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// changeOwnPassword asks for the current password, so that someone using a
// session left open can't take over the account.
func (s *Server) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, errLoginRequired)
		return
	}
	if u.ID == localUser.ID {
		writeError(w, http.StatusBadRequest, errors.New("no próprio PC não há conta: a palavra-passe muda-se na página Utilizadores"))
		return
	}
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		Password        string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !config.CheckPassword(u.PasswordHash, body.CurrentPassword) {
		writeError(w, http.StatusBadRequest, errors.New("a palavra-passe atual está errada"))
		return
	}
	if body.Password == body.CurrentPassword {
		writeError(w, http.StatusBadRequest, errors.New("a nova palavra-passe tem de ser diferente da atual"))
		return
	}
	updated, err := s.Store.UpdateUser(u.ID, config.UserChanges{Password: &body.Password}, nil)
	if err != nil {
		writeUserError(w, err)
		return
	}
	// Other sessions of this user are now invalid; keep this one.
	s.auth.setSession(w, r, updated)
	s.Log.Info("password changed", "email", u.Email, "ip", clientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("pedido inválido"))
		return false
	}
	return true
}
