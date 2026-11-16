package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/config"
)

const (
	sessionCookie   = "srr_session"
	sessionDuration = 7 * 24 * time.Hour
	sessionKeyFile  = "session.key"
	revokedFile     = "sessions.revoked"
	keyedSinceFile  = "links.keyed-since"
	maxFailures     = 5 // per IP (an IPv6 /64)
	// maxAccountFailures locks an account whatever the IP, against guesses
	// spread over many addresses. Not on the PC itself, so the owner can
	// always log in there.
	maxAccountFailures = 10
	lockoutDuration    = 15 * time.Minute
)

// auth issues and checks user sessions. A session is a cookie
// "<user id>.<expiry>.<hmac>" signed with a random key kept in session.key, so
// sessions survive restarts and updates. The signature also covers the user's
// password hash: changing a password logs that user out everywhere else.
type auth struct {
	key         []byte
	revokedPath string
	// keyedSince is when this app first required share keys. Recordings
	// started before keep working by their bare name, because links to them
	// were shared that way (the owner asked for this).
	keyedSince time.Time

	mu       sync.Mutex
	failures map[string]*failure
	revoked  map[string]int64 // signature of a logged-out session → its expiry
}

type failure struct {
	count int
	first time.Time
}

func newAuth(dir string) (*auth, error) {
	key, err := loadSessionKey(filepath.Join(dir, sessionKeyFile))
	a := &auth{key: key, revokedPath: filepath.Join(dir, revokedFile), failures: map[string]*failure{}, revoked: map[string]int64{}}
	a.keyedSince = loadKeyedSince(filepath.Join(dir, keyedSinceFile))
	if data, rerr := os.ReadFile(a.revokedPath); rerr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			sig, exp, _ := strings.Cut(line, " ")
			if e, perr := strconv.ParseInt(exp, 10, 64); perr == nil {
				a.revoked[sig] = e
			}
		}
	}
	return a, err
}

// revoke ends a session for good (logout): the cookie may have been copied.
// Revoked sessions are kept in sessions.revoked until they would expire.
func (a *auth) revoke(sig string, expiry int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().Unix()
	a.revoked[sig] = expiry
	var b strings.Builder
	for s, e := range a.revoked {
		if e < now {
			delete(a.revoked, s)
			continue
		}
		fmt.Fprintf(&b, "%s %d\n", s, e)
	}
	os.WriteFile(a.revokedPath, []byte(b.String()), 0o600)
}

func (a *auth) isRevoked(sig string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.revoked[sig]
	return ok
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
	if !ok || u.PasswordHash == "" || !hmac.Equal([]byte(parts[2]), []byte(a.sign(u.ID, expiry, u.PasswordHash))) || a.isRevoked(parts[2]) {
		return config.User{}, false
	}
	return u, true
}

// loadKeyedSince reads the time share keys started, saving now on first use.
func loadKeyedSince(path string) time.Time {
	if data, err := os.ReadFile(path); err == nil {
		if sec, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			return time.Unix(sec, 0)
		}
	}
	now := time.Now()
	os.WriteFile(path, []byte(strconv.FormatInt(now.Unix(), 10)), 0o600)
	return now
}

// openByName reports whether a recording opens without a key: one started
// before share keys existed (its name is its start time).
func (a *auth) openByName(name string) bool {
	if len(name) < 17 {
		return false
	}
	t, err := time.ParseInLocation("2006-01-02_150405", name[:17], time.Local)
	return err == nil && t.Before(a.keyedSince)
}

// shareKey signs a recording's share link. Deleting session.key also ends
// every shared link.
func (a *auth) shareKey(name string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte("recording|" + name))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

func (a *auth) setSession(w http.ResponseWriter, r *http.Request, u config.User) {
	expiry := time.Now().Add(sessionDuration).Unix()
	// Logging out and in again within a second would give the revoked
	// signature back.
	for a.isRevoked(a.sign(u.ID, expiry, u.PasswordHash)) {
		expiry++
	}
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

// lockKey is what the lockout counts failures by: the IP, but a whole IPv6
// /64 (one home or phone gets a whole /64, so one IP each is easy to rotate).
func lockKey(ip string) string {
	if p := net.ParseIP(ip); p != nil && p.To4() == nil {
		return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip
}

// accountKey is the lockout key of an account.
func accountKey(email string) string { return "account:" + strings.ToLower(strings.TrimSpace(email)) }

// locked reports whether an IP (or with max = maxAccountFailures, an
// account key) has too many recent failures.
func (a *auth) locked(key string, max int) bool {
	key = lockKey(key)
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[key]
	if f == nil {
		return false
	}
	if time.Since(f.first) > lockoutDuration {
		delete(a.failures, key)
		return false
	}
	return f.count >= max
}

func (a *auth) fail(keys ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.failures) > 10000 { // many random emails: drop what has expired
		for k, f := range a.failures {
			if time.Since(f.first) > lockoutDuration {
				delete(a.failures, k)
			}
		}
	}
	for _, key := range keys {
		key = lockKey(key)
		f := a.failures[key]
		if f == nil || time.Since(f.first) > lockoutDuration {
			f = &failure{first: time.Now()}
			a.failures[key] = f
		}
		f.count++
	}
}

func (a *auth) succeed(keys ...string) {
	a.mu.Lock()
	for _, key := range keys {
		delete(a.failures, lockKey(key))
	}
	a.mu.Unlock()
}

var (
	errLoginRequired      = errors.New("é preciso iniciar sessão")
	errForbidden          = errors.New("não tem permissão para isto")
	errMustChangePassword = errors.New("tem de mudar a palavra-passe antes de continuar")
	errTooManyFailures    = errors.New("demasiadas tentativas falhadas; tente daqui a 15 minutos")
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

// localUser is whoever uses the browser on the PC itself without logging in:
// a plain user, so the recorder works without a login but the settings and
// the users stay behind an admin login (the owner asked for this).
var localUser = config.User{ID: "local", Email: "este PC", Role: config.RoleUser}

// currentUser is the user of the session or, without one, the local user on
// the PC itself. A session wins, so an admin can log in on the PC.
func (s *Server) currentUser(r *http.Request) (config.User, bool) {
	if u, ok := s.auth.user(r, s.Store); ok {
		return u, true
	}
	if s.TrustLocal && isLocal(r) {
		return localUser, true
	}
	return config.User{}, false
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
	// InvitePending: invited by email, hasn't chosen a password yet.
	InvitePending bool `json:"invitePending"`
}

func toUserDTO(u config.User) userDTO {
	return userDTO{u.ID, u.Email, u.Role, u.MustChangePassword, u.InvitePending()}
}

type meDTO struct {
	// AuthRequired is false while there are no users: the recorder is open.
	AuthRequired bool     `json:"authRequired"`
	User         *userDTO `json:"user"`
	// Local is true on the PC itself without a session (see isLocal): a
	// plain user without a login, who can still log in.
	Local bool `json:"local,omitempty"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	resp := meDTO{AuthRequired: s.Store.HasUsers()}
	if u, ok := s.auth.user(r, s.Store); ok {
		// The page asks on every load: renewing here keeps someone who uses
		// the site at least once a week logged in.
		s.auth.setSession(w, r, u)
		d := toUserDTO(u)
		resp.User = &d
	} else if s.TrustLocal && isLocal(r) {
		d := toUserDTO(localUser)
		resp.User, resp.Local = &d, true
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.auth.locked(ip, maxFailures) {
		writeError(w, http.StatusTooManyRequests, errTooManyFailures)
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	account := accountKey(body.Email)
	if s.auth.locked(account, maxAccountFailures) && !(s.TrustLocal && isLocal(r)) {
		writeError(w, http.StatusTooManyRequests, errTooManyFailures)
		return
	}
	if !s.Store.HasUsers() {
		writeError(w, http.StatusForbidden, errors.New("ainda não existe nenhum utilizador; crie um administrador no PC com: futsal-cam-recorder.exe add-user <email> admin"))
		return
	}
	u, err := s.Store.Authenticate(body.Email, body.Password)
	if err != nil {
		s.auth.fail(ip, account)
		s.Log.Warn("login failed", "email", body.Email, "ip", ip)
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	s.auth.succeed(ip, account)
	s.auth.setSession(w, r, u)
	s.Log.Info("logged in", "email", u.Email, "role", u.Role, "ip", ip)
	d := toUserDTO(u)
	writeJSON(w, http.StatusOK, meDTO{AuthRequired: true, User: &d})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth.user(r, s.Store); ok {
		c, _ := r.Cookie(sessionCookie)
		parts := strings.Split(c.Value, ".")
		expiry, _ := strconv.ParseInt(parts[1], 10, 64)
		s.auth.revoke(parts[2], expiry)
	}
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
		writeError(w, http.StatusBadRequest, errors.New("sem sessão iniciada não há palavra-passe para mudar: inicie sessão primeiro"))
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
