package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joaoopereira/simple-rtsp-recorder/internal/config"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/logging"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/recorder"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/updater"
	"github.com/joaoopereira/simple-rtsp-recorder/web"
)

func newTestServer(t *testing.T) (*httptest.Server, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	cfg.Camera = config.Camera{Host: "cam", Port: 554, Path: "live", User: "admin", Password: "secret"}
	if err := store.Update(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddUser(adminEmail, config.RoleAdmin, adminPassword, false); err != nil {
		t.Fatal(err)
	}
	log := logging.Discard()
	rec := recorder.New("ffmpeg", func() (string, string) {
		return store.Get().Camera.StreamURL(), store.RecordingsDir()
	}, filepath.Join(dir, "state"), log.Logger)
	upd := updater.New(updater.Options{Current: "dev", Log: log.Logger, Enabled: func() bool { return false }})
	s := &Server{Version: "test", Store: store, Recorder: rec, Updater: upd, Log: log, Static: web.FS}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, store
}

const (
	adminEmail    = "admin@example.com"
	adminPassword = "correct horse"
)

// client keeps cookies, like a browser.
func client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

// adminClient returns a client logged in as admin.
func adminClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	return loginClient(t, srv, adminEmail, adminPassword)
}

// loginClient returns a client logged in as the given user.
func loginClient(t *testing.T, srv *httptest.Server, email, password string) *http.Client {
	t.Helper()
	c := client()
	if resp, body := doWith(t, c, "POST", srv.URL+"/api/login", `{"email":"`+email+`","password":"`+password+`"}`); resp.StatusCode != 200 {
		t.Fatalf("login %s = %d %s", email, resp.StatusCode, body)
	}
	return c
}

func do(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	return doWith(t, http.DefaultClient, method, url, body)
}

func doWith(t *testing.T, c *http.Client, method, url, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, b.String()
}

func TestSettingsPasswordIsWriteOnly(t *testing.T) {
	srv, store := newTestServer(t)
	c := adminClient(t, srv)
	do := func(t *testing.T, method, url, body string) (*http.Response, string) {
		return doWith(t, c, method, url, body)
	}

	_, body := do(t, "GET", srv.URL+"/api/settings", "")
	if strings.Contains(body, "secret") || strings.Contains(body, "pbkdf2") || !strings.Contains(body, `"hasPassword":true`) {
		t.Fatalf("GET /api/settings leaked or hid password state: %s", body)
	}

	// Saving without a password field keeps the stored one.
	var dto map[string]any
	json.Unmarshal([]byte(body), &dto)
	dto["title"] = "Futsal"
	payload, _ := json.Marshal(dto)
	if resp, body := do(t, "PUT", srv.URL+"/api/settings", string(payload)); resp.StatusCode != 200 {
		t.Fatalf("PUT = %d %s", resp.StatusCode, body)
	}
	if got := store.Get(); got.Camera.Password != "secret" || got.Title != "Futsal" || len(got.Users) != 1 {
		t.Fatalf("after PUT: %+v", got)
	}

	// An explicit password replaces it.
	dto["camera"].(map[string]any)["password"] = "new"
	payload, _ = json.Marshal(dto)
	do(t, "PUT", srv.URL+"/api/settings", string(payload))
	if got := store.Get().Camera.Password; got != "new" {
		t.Fatalf("password = %q, want new", got)
	}

	// Invalid settings are rejected.
	dto["camera"].(map[string]any)["port"] = 70000
	payload, _ = json.Marshal(dto)
	if resp, _ := do(t, "PUT", srv.URL+"/api/settings", string(payload)); resp.StatusCode != 400 {
		t.Fatalf("invalid port accepted: %d", resp.StatusCode)
	}
}

func TestRecordingsEndpoints(t *testing.T) {
	srv, store := newTestServer(t)
	dir := store.RecordingsDir()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "2026-10-29_150000.mp4"), []byte("video"), 0o644)
	os.WriteFile(filepath.Join(store.BaseDir(), "secret.mp4"), []byte("outside"), 0o644)
	c := adminClient(t, srv)
	do := func(t *testing.T, method, url, body string) (*http.Response, string) {
		return doWith(t, c, method, url, body)
	}

	if resp, _ := doWith(t, client(), "GET", srv.URL+"/api/recordings", ""); resp.StatusCode != 401 {
		t.Fatalf("list without login = %d, want 401", resp.StatusCode)
	}
	_, body := do(t, "GET", srv.URL+"/api/recordings", "")
	if !strings.Contains(body, "2026-10-29_150000.mp4") {
		t.Fatalf("list = %s", body)
	}

	// The shared link works without logging in.
	if resp, body := doWith(t, client(), "GET", srv.URL+"/recordings/2026-10-29_150000.mp4", ""); resp.StatusCode != 200 || body != "video" {
		t.Fatalf("public link = %d %q", resp.StatusCode, body)
	}
	resp, body := do(t, "GET", srv.URL+"/recordings/2026-10-29_150000.mp4?download=1", "")
	if resp.StatusCode != 200 || body != "video" || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download = %d %q %v", resp.StatusCode, body, resp.Header)
	}

	for _, path := range []string{"/recordings/..%2Fsecret.mp4", "/api/recordings/..%2Fsecret.mp4"} {
		method := "GET"
		if strings.HasPrefix(path, "/api") {
			method = "DELETE"
		}
		if resp, _ := do(t, method, srv.URL+path, ""); resp.StatusCode != 400 {
			t.Errorf("%s %s = %d, want 400", method, path, resp.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(store.BaseDir(), "secret.mp4")); err != nil {
		t.Fatal("file outside recordings dir was touched")
	}

	if resp, _ := do(t, "DELETE", srv.URL+"/api/recordings/2026-10-29_150000.mp4", ""); resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if resp, _ := do(t, "DELETE", srv.URL+"/api/recordings/2026-10-29_150000.mp4", ""); resp.StatusCode != 404 {
		t.Fatalf("second delete = %d, want 404", resp.StatusCode)
	}
}

func TestStatusAndStatic(t *testing.T) {
	srv, _ := newTestServer(t)
	// Without login, only what a page needs to reload after an update.
	resp, body := do(t, "GET", srv.URL+"/api/status", "")
	if resp.StatusCode != 200 || body != `{"loginRequired":true,"title":"Simple RTSP Recorder","version":"test"}`+"\n" {
		t.Fatalf("anonymous status = %d %s", resp.StatusCode, body)
	}
	c := adminClient(t, srv)
	resp, body = doWith(t, c, "GET", srv.URL+"/api/status", "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"state":"idle"`) || !strings.Contains(body, `"configured":true`) {
		t.Fatalf("status = %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, "GET", srv.URL+"/", "")
	if resp.StatusCode != 200 || !strings.Contains(body, "<title>") {
		t.Fatalf("index = %d", resp.StatusCode)
	}
	if resp, _ := doWith(t, c, "POST", srv.URL+"/api/recording/stop", ""); resp.StatusCode != 409 {
		t.Fatalf("stop while idle = %d, want 409", resp.StatusCode)
	}
}

// addUser creates a user directly in the store and logs it in.
func addUser(t *testing.T, srv *httptest.Server, store *config.Store, email string, role config.Role) (config.User, *http.Client) {
	t.Helper()
	u, err := store.AddUser(email, role, "password "+email, false)
	if err != nil {
		t.Fatal(err)
	}
	return u, loginClient(t, srv, email, "password "+email)
}

func TestAccessByRole(t *testing.T) {
	srv, store := newTestServer(t)
	_, manager := addUser(t, srv, store, "manager@example.com", config.RoleUserManager)
	_, user := addUser(t, srv, store, "user@example.com", config.RoleUser)
	clients := map[string]*http.Client{"anon": client(), "user": user, "manager": manager, "admin": adminClient(t, srv)}

	// Expected status per client: anon, user, manager, admin. 0 = "allowed"
	// (anything but 401/403: the handler ran).
	const ok = 0
	for _, c := range []struct {
		method, path string
		want         [4]int
	}{
		{"GET", "/api/recordings", [4]int{401, ok, ok, ok}},
		{"POST", "/api/recording/stop", [4]int{401, ok, ok, ok}},
		{"DELETE", "/api/recordings/2026-01-01_000000.mp4", [4]int{401, ok, ok, ok}},
		{"GET", "/api/snapshot", [4]int{401, ok, ok, ok}},
		{"GET", "/api/users", [4]int{401, 403, ok, ok}},
		{"POST", "/api/users", [4]int{401, 403, ok, ok}},
		{"PUT", "/api/users/x", [4]int{401, 403, ok, ok}},
		{"DELETE", "/api/users/x", [4]int{401, 403, ok, ok}},
		{"GET", "/api/settings", [4]int{401, 403, 403, ok}},
		{"PUT", "/api/settings", [4]int{401, 403, 403, ok}},
		{"POST", "/api/settings/test", [4]int{401, 403, 403, ok}},
		{"POST", "/api/update/check", [4]int{401, 403, 403, ok}},
		{"GET", "/api/logs", [4]int{401, 403, 403, ok}},
		{"PUT", "/api/me/password", [4]int{401, ok, ok, ok}},
	} {
		for i, who := range []string{"anon", "user", "manager", "admin"} {
			resp, body := doWith(t, clients[who], c.method, srv.URL+c.path, `{}`)
			got := resp.StatusCode
			if c.want[i] == ok && got != 401 && got != 403 {
				continue
			}
			if got != c.want[i] {
				t.Errorf("%s %s as %s = %d %s", c.method, c.path, who, got, body)
			}
		}
	}

	// A forged cookie is rejected.
	req, _ := http.NewRequest("GET", srv.URL+"/api/recordings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "x.99999999999.deadbeef"})
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Errorf("forged cookie = %d, want 401", resp.StatusCode)
	}

	if _, body := doWith(t, clients["anon"], "GET", srv.URL+"/api/me", ""); body != `{"authRequired":true,"user":null}`+"\n" {
		t.Errorf("me (anon) = %s", body)
	}
	_, body := doWith(t, manager, "GET", srv.URL+"/api/me", "")
	if !strings.Contains(body, `"email":"manager@example.com","role":"userManager"`) {
		t.Errorf("me (manager) = %s", body)
	}
}

func TestUserManagement(t *testing.T) {
	srv, store := newTestServer(t)
	admin := adminClient(t, srv)
	adminUser := store.Get().Users[0]
	mgrUser, manager := addUser(t, srv, store, "manager@example.com", config.RoleUserManager)
	plain, plainClient := addUser(t, srv, store, "user@example.com", config.RoleUser)

	// The list never contains password hashes.
	_, body := doWith(t, manager, "GET", srv.URL+"/api/users", "")
	if strings.Contains(body, "pbkdf2") || strings.Contains(body, "asswordHash") || strings.Count(body, `"id"`) != 3 {
		t.Fatalf("list = %s", body)
	}

	// A user manager can't create, promote to, edit or delete admins or user
	// managers: only plain users.
	otherMgrUser, _ := addUser(t, srv, store, "other@example.com", config.RoleUserManager)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/users", `{"email":"x@example.com","role":"admin","password":"password1"}`},
		{"POST", "/api/users", `{"email":"x@example.com","role":"userManager","password":"password1"}`},
		{"PUT", "/api/users/" + plain.ID, `{"role":"admin"}`},
		{"PUT", "/api/users/" + plain.ID, `{"role":"userManager"}`},
		{"PUT", "/api/users/" + mgrUser.ID, `{"role":"admin"}`},
		{"PUT", "/api/users/" + otherMgrUser.ID, `{"password":"hijacked!"}`},
		{"PUT", "/api/users/" + otherMgrUser.ID, `{"role":"user"}`},
		{"DELETE", "/api/users/" + otherMgrUser.ID, ``},
		{"PUT", "/api/users/" + adminUser.ID, `{"password":"hijacked!"}`},
		{"DELETE", "/api/users/" + adminUser.ID, ``},
	} {
		if resp, body := doWith(t, manager, c.method, srv.URL+c.path, c.body); resp.StatusCode != 403 {
			t.Errorf("manager %s %s %s = %d %s, want 403", c.method, c.path, c.body, resp.StatusCode, body)
		}
	}
	if _, err := store.Authenticate(adminEmail, adminPassword); err != nil {
		t.Fatal("admin password changed by a user manager")
	}
	if _, err := store.Authenticate("x@example.com", "password1"); err == nil {
		t.Fatal("user manager created a privileged user")
	}

	// But can manage plain users.
	resp, body := doWith(t, manager, "POST", srv.URL+"/api/users", `{"email":"New@Example.com","role":"user","password":"password1"}`)
	if resp.StatusCode != 201 || !strings.Contains(body, `"email":"new@example.com"`) || !strings.Contains(body, `"mustChangePassword":true`) {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var created userDTO
	json.Unmarshal([]byte(body), &created)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"email":"new@example.com","role":"user","password":"password1"}`, 409},
		{`{"email":"bad","role":"user","password":"password1"}`, 400},
		{`{"email":"b@example.com","role":"user","password":"short"}`, 400},
		{`{"email":"b@example.com","role":"boss","password":"password1"}`, 400},
	} {
		if resp, body := doWith(t, manager, "POST", srv.URL+"/api/users", c.body); resp.StatusCode != c.want {
			t.Errorf("create %s = %d %s, want %d", c.body, resp.StatusCode, body, c.want)
		}
	}
	if resp, body := doWith(t, manager, "PUT", srv.URL+"/api/users/"+created.ID, `{"email":"renamed@example.com","password":""}`); resp.StatusCode != 200 || !strings.Contains(body, `"email":"renamed@example.com"`) {
		t.Fatalf("edit = %d %s", resp.StatusCode, body)
	}
	if _, err := store.Authenticate("renamed@example.com", "password1"); err != nil {
		t.Fatal("empty password in update must keep the old one")
	}
	// The admin can promote to user manager.
	if resp, body := doWith(t, admin, "PUT", srv.URL+"/api/users/"+created.ID, `{"role":"userManager"}`); resp.StatusCode != 200 || !strings.Contains(body, `"role":"userManager"`) {
		t.Fatalf("admin promote = %d %s", resp.StatusCode, body)
	}

	// Nobody can delete themselves.
	if resp, _ := doWith(t, manager, "DELETE", srv.URL+"/api/users/"+mgrUser.ID, ""); resp.StatusCode != 403 {
		t.Errorf("delete self = %d, want 403", resp.StatusCode)
	}
	// The last admin can't be demoted, even by itself.
	if resp, _ := doWith(t, admin, "PUT", srv.URL+"/api/users/"+adminUser.ID, `{"role":"user"}`); resp.StatusCode != 409 {
		t.Errorf("demote last admin = %d, want 409", resp.StatusCode)
	}

	// A role change applies at once to existing sessions.
	if resp, _ := doWith(t, plainClient, "GET", srv.URL+"/api/users", ""); resp.StatusCode != 403 {
		t.Fatal("user reached the users page")
	}
	doWith(t, admin, "PUT", srv.URL+"/api/users/"+plain.ID, `{"role":"userManager"}`)
	if resp, _ := doWith(t, plainClient, "GET", srv.URL+"/api/users", ""); resp.StatusCode != 200 {
		t.Errorf("after promotion = %d, want 200", resp.StatusCode)
	}
	// A password reset by someone else logs the user out and makes them
	// change it at the next login; a deleted user is logged out too.
	if _, body := doWith(t, admin, "PUT", srv.URL+"/api/users/"+plain.ID, `{"password":"another password"}`); !strings.Contains(body, `"mustChangePassword":true`) {
		t.Errorf("reset by admin = %s, want mustChangePassword", body)
	}
	if resp, _ := doWith(t, plainClient, "GET", srv.URL+"/api/recordings", ""); resp.StatusCode != 401 {
		t.Errorf("after password reset = %d, want 401", resp.StatusCode)
	}
	// Setting your own password from the users page doesn't.
	if _, body := doWith(t, admin, "PUT", srv.URL+"/api/users/"+adminUser.ID, `{"password":"`+adminPassword+`!"}`); !strings.Contains(body, `"mustChangePassword":false`) {
		t.Errorf("own reset = %s, want no mustChangePassword", body)
	}
	if resp, _ := doWith(t, admin, "GET", srv.URL+"/api/settings", ""); resp.StatusCode != 200 {
		t.Errorf("own session after own reset = %d, want 200", resp.StatusCode)
	}
	if resp, _ := doWith(t, admin, "DELETE", srv.URL+"/api/users/"+mgrUser.ID, ""); resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if resp, _ := doWith(t, manager, "GET", srv.URL+"/api/recordings", ""); resp.StatusCode != 401 {
		t.Errorf("deleted user's session = %d, want 401", resp.StatusCode)
	}
	if resp, _ := doWith(t, admin, "DELETE", srv.URL+"/api/users/"+mgrUser.ID, ""); resp.StatusCode != 404 {
		t.Errorf("delete twice = %d, want 404", resp.StatusCode)
	}
}

// A user whose password was chosen by someone else can do nothing but change
// it, and must pick a different one.
func TestMustChangePassword(t *testing.T) {
	srv, _ := newTestServer(t)
	admin := adminClient(t, srv)
	if resp, body := doWith(t, admin, "POST", srv.URL+"/api/users", `{"email":"new@example.com","role":"user","password":"temporary1"}`); resp.StatusCode != 201 {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	c := loginClient(t, srv, "new@example.com", "temporary1")
	if _, body := doWith(t, c, "GET", srv.URL+"/api/me", ""); !strings.Contains(body, `"mustChangePassword":true`) {
		t.Fatalf("me = %s", body)
	}
	if _, body := doWith(t, c, "GET", srv.URL+"/api/status", ""); !strings.Contains(body, `"loginRequired":true`) {
		t.Errorf("status before change = %s, want the public one", body)
	}
	for _, r := range []struct{ method, path string }{{"GET", "/api/recordings"}, {"POST", "/api/recording/start"}, {"GET", "/api/snapshot"}} {
		if resp, body := doWith(t, c, r.method, srv.URL+r.path, ""); resp.StatusCode != 403 || !strings.Contains(body, "mudar a palavra-passe") {
			t.Errorf("%s %s before change = %d %s, want 403", r.method, r.path, resp.StatusCode, body)
		}
	}
	if resp, _ := doWith(t, c, "PUT", srv.URL+"/api/me/password", `{"currentPassword":"temporary1","password":"temporary1"}`); resp.StatusCode != 400 {
		t.Errorf("same password = %d, want 400", resp.StatusCode)
	}
	if resp, body := doWith(t, c, "PUT", srv.URL+"/api/me/password", `{"currentPassword":"temporary1","password":"my own pass"}`); resp.StatusCode != 204 {
		t.Fatalf("change = %d %s", resp.StatusCode, body)
	}
	if resp, _ := doWith(t, c, "GET", srv.URL+"/api/recordings", ""); resp.StatusCode != 200 {
		t.Errorf("after change = %d, want 200", resp.StatusCode)
	}
	if _, body := doWith(t, c, "GET", srv.URL+"/api/me", ""); !strings.Contains(body, `"mustChangePassword":false`) {
		t.Errorf("me after change = %s", body)
	}
}

func TestLoginLockoutAndPasswordChange(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client()
	for i := 0; i < maxFailures; i++ {
		if resp, _ := doWith(t, c, "POST", srv.URL+"/api/login", `{"email":"`+adminEmail+`","password":"wrong"}`); resp.StatusCode != 401 {
			t.Fatalf("wrong password = %d, want 401", resp.StatusCode)
		}
	}
	if resp, _ := doWith(t, c, "POST", srv.URL+"/api/login", `{"email":"`+adminEmail+`","password":"`+adminPassword+`"}`); resp.StatusCode != 429 {
		t.Fatalf("login after lockout = %d, want 429", resp.StatusCode)
	}

	// Changing the own password needs the current one, logs out other
	// sessions but keeps this one.
	srv2, _ := newTestServer(t)
	a, b := adminClient(t, srv2), adminClient(t, srv2)
	if resp, body := doWith(t, a, "PUT", srv2.URL+"/api/me/password", `{"currentPassword":"`+adminPassword+`","password":"short"}`); resp.StatusCode != 400 {
		t.Fatalf("weak password = %d %s, want 400", resp.StatusCode, body)
	}
	if resp, _ := doWith(t, a, "PUT", srv2.URL+"/api/me/password", `{"currentPassword":"wrong","password":"a new password"}`); resp.StatusCode != 400 {
		t.Fatalf("wrong current password = %d, want 400", resp.StatusCode)
	}
	if resp, _ := doWith(t, a, "PUT", srv2.URL+"/api/me/password", `{"currentPassword":"`+adminPassword+`","password":"a new password"}`); resp.StatusCode != 204 {
		t.Fatalf("change password = %d", resp.StatusCode)
	}
	if resp, _ := doWith(t, a, "GET", srv2.URL+"/api/settings", ""); resp.StatusCode != 200 {
		t.Errorf("own session after change = %d, want 200", resp.StatusCode)
	}
	if resp, _ := doWith(t, b, "GET", srv2.URL+"/api/settings", ""); resp.StatusCode != 401 {
		t.Errorf("other session after change = %d, want 401", resp.StatusCode)
	}

	// Logout ends the session.
	doWith(t, a, "POST", srv2.URL+"/api/logout", "")
	if resp, _ := doWith(t, a, "GET", srv2.URL+"/api/settings", ""); resp.StatusCode != 401 {
		t.Errorf("after logout = %d, want 401", resp.StatusCode)
	}
}

func TestSessionSurvivesRestart(t *testing.T) {
	srv, store := newTestServer(t)
	c := adminClient(t, srv)
	resp, _ := doWith(t, c, "GET", srv.URL+"/api/me", "")
	var renewed *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			renewed = ck
		}
	}
	if renewed == nil || renewed.MaxAge != int(sessionDuration.Seconds()) {
		t.Errorf("session not renewed for 7 days: %+v", renewed)
	}

	// A restart (or update) builds a new handler; the key is read back from disk.
	restarted := &Server{Version: "test", Store: store, Log: logging.Discard(), Static: web.FS}
	srv2 := httptest.NewServer(restarted.Handler())
	t.Cleanup(srv2.Close)
	if resp, body := doWith(t, c, "GET", srv2.URL+"/api/logs", ""); resp.StatusCode != 200 {
		t.Fatalf("logs after restart = %d %s, want 200", resp.StatusCode, body)
	}
	if info, err := os.Stat(filepath.Join(store.BaseDir(), sessionKeyFile)); err != nil || info.Size() == 0 {
		t.Fatalf("session key not saved: %v", err)
	}
}

// Without any user (fresh install), the recorder stays open as before 2.7 so
// an automatic update never locks anyone out; settings stay closed.
func TestOpenModeWithoutUsers(t *testing.T) {
	srv, store := newTestServer(t)
	cfg := store.Get()
	cfg.Users = nil
	store.Update(cfg)
	if resp, body := do(t, "GET", srv.URL+"/api/status", ""); resp.StatusCode != 200 || !strings.Contains(body, `"recorder"`) {
		t.Fatalf("status = %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, "GET", srv.URL+"/api/recordings", ""); resp.StatusCode != 200 {
		t.Fatalf("recordings = %d, want 200", resp.StatusCode)
	}
	if _, body := do(t, "GET", srv.URL+"/api/me", ""); body != `{"authRequired":false,"user":null}`+"\n" {
		t.Fatalf("me = %s", body)
	}
	if resp, body := do(t, "POST", srv.URL+"/api/login", `{"email":"a@b.pt","password":"x"}`); resp.StatusCode != 403 || !strings.Contains(body, "add-user") {
		t.Fatalf("login with no users = %d %s", resp.StatusCode, body)
	}
	for _, path := range []string{"/api/settings", "/api/users", "/api/logs"} {
		if resp, _ := do(t, "GET", srv.URL+path, ""); resp.StatusCode != 401 {
			t.Errorf("%s with no users = %d, want 401", path, resp.StatusCode)
		}
	}
	if resp, _ := do(t, "PUT", srv.URL+"/api/me/password", `{"password":"password1"}`); resp.StatusCode != 401 {
		t.Errorf("me/password with no users = %d, want 401", resp.StatusCode)
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cf-Connecting-Ip", "1.2.3.4")
	r.RemoteAddr = "127.0.0.1:5000"
	if got := clientIP(r); got != "1.2.3.4" {
		t.Errorf("via tunnel = %q", got)
	}
	r.RemoteAddr = "192.168.1.50:5000"
	if got := clientIP(r); got != "192.168.1.50" {
		t.Errorf("LAN client with forged header = %q", got)
	}
}

// The page must reference every asset by a content hash so that no cache
// (Cloudflare kept serving the 1.x style.css) can serve another version.
func TestStaticAssetsAreVersioned(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, page := do(t, "GET", srv.URL+"/", "")
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("index Cache-Control = %q, want no-store", cc)
	}
	for _, name := range assets {
		data, err := fs.ReadFile(web.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		url := name + "?v=" + hex.EncodeToString(sum[:6])
		if !strings.Contains(page, `"`+url+`"`) {
			t.Errorf("index.html does not reference %s", url)
		}
		if strings.Contains(page, `"`+name+`"`) {
			t.Errorf("index.html still has an unversioned reference to %s", name)
		}
		resp, body := do(t, "GET", srv.URL+"/"+url, "")
		if resp.StatusCode != 200 || body != string(data) {
			t.Errorf("GET %s = %d", url, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s Cache-Control = %q, want immutable", url, cc)
		}
		if resp, _ := do(t, "GET", srv.URL+"/"+name+"?v=stale", ""); resp.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s with a stale hash must not be cached long", name)
		}
	}
	if resp, _ := do(t, "GET", srv.URL+"/style.css", ""); !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		t.Errorf("style.css Content-Type = %q", resp.Header.Get("Content-Type"))
	}
}
