package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joaoopereira/simple-rtsp-recorder/internal/config"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/logging"
)

// fakeService stands for the cloudflared service: the "exe" is a text file
// holding its version, and the tunnel works while a good version runs.
type fakeService struct {
	mu      sync.Mutex
	exe     string
	running bool
	stops   int
}

func (f *fakeService) ExePath() (string, error) { return f.exe, nil }
func (f *fakeService) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	f.stops++
	return nil
}
func (f *fakeService) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	return nil
}

// tunnelUp: the public site answers when the service runs a version that
// isn't "broken".
func (f *fakeService) tunnelUp() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, _ := os.ReadFile(f.exe)
	return f.running && !strings.Contains(string(data), "broken")
}

func fileVersion(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.Fields(string(data))[0], nil
}

type env struct {
	u        *Updater
	svc      *fakeService
	site     *httptest.Server
	siteDown bool
	asset    string // content of the latest release's exe
	sum      string // its published sha256 ("" = the right one)
	locked   int    // TryLock failures left (a recording running)
	notes    []string
}

func newEnv(t *testing.T, installed, latest string) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{svc: &fakeService{exe: filepath.Join(dir, "cloudflared.exe"), running: true}, asset: latest + " new build"}
	os.WriteFile(e.svc.exe, []byte(installed+" old build"), 0o755)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asset" {
			w.Write([]byte(e.asset))
			return
		}
		sum := e.sum
		if sum == "" {
			h := sha256.Sum256([]byte(e.asset))
			sum = hex.EncodeToString(h[:])
		}
		fmt.Fprintf(w, `{"tag_name":"%s","body":"","assets":[{"name":"%s","browser_download_url":"http://%s/asset","digest":"sha256:%s"}]}`,
			latest, AssetName, r.Host, sum)
	}))
	t.Cleanup(gh.Close)
	old := LatestURL
	LatestURL = gh.URL + "/latest"
	t.Cleanup(func() { LatestURL = old })

	e.site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.siteDown || !e.svc.tunnelUp() {
			w.WriteHeader(530) // Cloudflare: tunnel not connected
			return
		}
		w.Write([]byte(`{"loginRequired":true,"title":"t","version":"v9.9.9"}`))
	}))
	t.Cleanup(e.site.Close)

	cfg := config.Default()
	cfg.Notify.SiteURL = e.site.URL
	e.u = &Updater{
		Service:    e.svc,
		Config:     func() config.Config { return cfg },
		AppVersion: "v9.9.9",
		TryLock: func() bool {
			if e.locked > 0 {
				e.locked--
				return false
			}
			return true
		},
		Unlock:        func() {},
		Notify:        func(subject, text string) { e.notes = append(e.notes, subject+": "+text) },
		Log:           logging.Discard().Logger,
		ExeVersion:    fileVersion,
		WaitEvery:     5 * time.Millisecond,
		CheckEvery:    5 * time.Millisecond,
		VerifyTimeout: 100 * time.Millisecond,
	}
	e.u.defaults()
	return e
}

func (e *env) run(t *testing.T) Status {
	t.Helper()
	e.u.running = true
	e.u.update(context.Background(), "test")
	return e.u.Status()
}

func (e *env) exe() string {
	data, _ := os.ReadFile(e.svc.exe)
	return string(data)
}

func TestUpdateSuccess(t *testing.T) {
	e := newEnv(t, "2025.1.0", "2025.8.1")
	e.locked = 3 // a recording is running: wait for it to end
	st := e.run(t)
	if st.Failed || !strings.Contains(st.Message, "2025.1.0 para a 2025.8.1") || st.Installed != "2025.8.1" || st.State != "idle" {
		t.Fatalf("status = %+v", st)
	}
	if e.exe() != "2025.8.1 new build" {
		t.Fatalf("exe = %q", e.exe())
	}
	if old, _ := os.ReadFile(e.svc.exe + ".old"); string(old) != "2025.1.0 old build" {
		t.Fatalf(".old = %q", old)
	}
	if _, err := os.Stat(e.svc.exe + ".new"); err == nil {
		t.Fatal(".new left behind")
	}
	if e.locked != 0 {
		t.Fatal("did not wait for the recording to end")
	}
	if len(e.notes) != 1 || !strings.Contains(e.notes[0], "atualizado") {
		t.Fatalf("notes = %v", e.notes)
	}
}

func TestUpdateRollsBackWhenTunnelDoesNotReturn(t *testing.T) {
	e := newEnv(t, "2025.1.0", "2025.8.1")
	e.asset = "2025.8.1 broken build"
	st := e.run(t)
	if !st.Failed || !strings.Contains(st.Message, "reposta e o túnel está a funcionar") {
		t.Fatalf("status = %+v", st)
	}
	if e.exe() != "2025.1.0 old build" || !e.svc.tunnelUp() {
		t.Fatalf("old version not restored: %q", e.exe())
	}
	if len(e.notes) != 1 || !strings.Contains(e.notes[0], "revertida") {
		t.Fatalf("notes = %v", e.notes)
	}
}

func TestUpdateDoesNothing(t *testing.T) {
	cases := []struct {
		name, installed, latest, want string
		setup                         func(*env)
		failed                        bool
	}{
		{"up to date", "2025.8.1", "2025.8.1", "já está na versão mais recente", nil, false},
		{"older release", "2025.9.0", "2025.8.1", "já está na versão mais recente", nil, false},
		{"tunnel already down", "2025.1.0", "2025.8.1", "nada foi alterado", func(e *env) { e.siteDown = true }, true},
		{"bad checksum", "2025.1.0", "2025.8.1", "checksum", func(e *env) { e.sum = strings.Repeat("0", 64) }, true},
		{"no checksum", "2025.1.0", "2025.8.1", "não publica o checksum", func(e *env) { e.sum = "x" }, true},
		{"exe reports another version", "2025.1.0", "2025.8.1", "não arranca como esperado", func(e *env) { e.asset = "2024.1.1 build" }, true},
		{"no site address", "2025.1.0", "2025.8.1", "endereço público", func(e *env) {
			cfg := config.Default()
			e.u.Config = func() config.Config { return cfg }
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, c.installed, c.latest)
			if c.setup != nil {
				c.setup(e)
			}
			st := e.run(t)
			if st.Failed != c.failed || !strings.Contains(st.Message, c.want) {
				t.Fatalf("status = %+v", st)
			}
			if e.exe() != c.installed+" old build" || e.svc.stops != 0 {
				t.Fatalf("cloudflared touched: exe %q, stops %d", e.exe(), e.svc.stops)
			}
			if _, err := os.Stat(e.svc.exe + ".new"); err == nil {
				t.Fatal(".new left behind")
			}
		})
	}
}

func TestWeeklySchedule(t *testing.T) {
	cfg := config.Default()
	now := time.Date(2026, 11, 2, 4, 30, 0, 0, time.Local) // a Monday
	u := &Updater{Config: func() config.Config { return cfg }, Now: func() time.Time { return now }}
	if !u.dueWeekly() || u.dueWeekly() {
		t.Fatal("Monday 04:30 must run exactly once")
	}
	for _, at := range []time.Time{
		time.Date(2026, 11, 2, 5, 10, 0, 0, time.Local), // Monday 05:10
		time.Date(2026, 11, 8, 4, 30, 0, 0, time.Local), // Sunday
		time.Date(2026, 11, 3, 4, 30, 0, 0, time.Local), // Tuesday
	} {
		now = at
		if u.dueWeekly() {
			t.Errorf("ran at %v", at)
		}
	}
	now = time.Date(2026, 11, 9, 4, 0, 0, 0, time.Local) // next Monday
	cfg.Tunnel.AutoUpdate = false
	if u.dueWeekly() {
		t.Error("ran with auto update off")
	}
	cfg.Tunnel.AutoUpdate = true
	if !u.dueWeekly() {
		t.Error("did not run the next Monday")
	}
}

func TestExeFromCommandLine(t *testing.T) {
	for in, want := range map[string]string{
		`"C:\Program Files (x86)\cloudflared\cloudflared.exe" tunnel run --token eyJh`: `C:\Program Files (x86)\cloudflared\cloudflared.exe`,
		`C:\Program Files (x86)\cloudflared\cloudflared.exe tunnel run --token eyJh`:   `C:\Program Files (x86)\cloudflared\cloudflared.exe`,
		`C:\cloudflared\cloudflared.EXE --config c.yml`:                                `C:\cloudflared\cloudflared.EXE`,
	} {
		if got := ExeFromCommandLine(in); got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
}

func TestUpdateNowBusy(t *testing.T) {
	e := newEnv(t, "2025.1.0", "2025.8.1")
	e.u.running = true
	if err := e.u.UpdateNow(); err != ErrBusy {
		t.Fatalf("UpdateNow while running = %v", err)
	}
}
