package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/config"
	"github.com/gdrretorta/futsal-cam-recorder/internal/logging"
)

func testConfig() config.Config {
	cfg := config.Default()
	cfg.Title = "Futsal"
	cfg.Notify = config.Notify{ResendAPIKey: "re_test", From: "Gravador <g@retorta.com>", SiteURL: "https://futsal.retorta.com", LongRecordingHours: 3}
	cfg.Users = []config.User{
		{ID: "1", Email: "admin@x.pt", Role: config.RoleAdmin},
		{ID: "2", Email: "mgr@x.pt", Role: config.RoleUserManager},
		{ID: "3", Email: "boss@x.pt", Role: config.RoleAdmin},
	}
	return cfg
}

func TestSendToResend(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		if got["from"] == "bad" {
			w.WriteHeader(403)
			w.Write([]byte(`{"statusCode":403,"message":"The retorta.com domain is not verified."}`))
			return
		}
		w.Write([]byte(`{"id":"abc"}`))
	}))
	defer srv.Close()
	old := ResendURL
	ResendURL = srv.URL
	defer func() { ResendURL = old }()

	cfg := testConfig()
	if err := Send(context.Background(), cfg.Notify, TestEmail(cfg)); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer re_test" || got["from"] != "Gravador <g@retorta.com>" {
		t.Errorf("auth %q, body %v", auth, got)
	}
	// Only admins receive the emails.
	if to, _ := json.Marshal(got["to"]); string(to) != `["admin@x.pt","boss@x.pt"]` {
		t.Errorf("to = %s", to)
	}
	if html, _ := got["html"].(string); !strings.Contains(html, `<a href="https://futsal.retorta.com">`) {
		t.Errorf("html = %s", html)
	}

	cfg.Notify.From = "bad"
	if err := Send(context.Background(), cfg.Notify, TestEmail(cfg)); err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Errorf("Resend error not reported: %v", err)
	}
	cfg.Notify.ResendAPIKey = ""
	if err := Send(context.Background(), cfg.Notify, TestEmail(cfg)); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("without key = %v", err)
	}
}

func TestWatcher(t *testing.T) {
	cfg := testConfig()
	start := time.Date(2026, 11, 1, 14, 5, 0, 0, time.Local)
	now := start
	var started *time.Time
	var sent []int
	fail := false
	w := &Watcher{
		Config:    func() config.Config { return cfg },
		StartedAt: func() *time.Time { return started },
		Log:       logging.Discard().Logger,
		Now:       func() time.Time { return now },
		Send: func(_ context.Context, _ config.Notify, e Email) error {
			if fail {
				return errors.New("offline")
			}
			var h int
			for _, f := range strings.Fields(e.Subject) {
				if n, err := json.Number(f).Int64(); err == nil {
					h = int(n)
				}
			}
			sent = append(sent, h)
			if !strings.Contains(e.Text, "01/11/2026 às 14:05") {
				t.Errorf("text = %s", e.Text)
			}
			return nil
		},
	}
	at := func(d time.Duration) { now = start.Add(d); w.Check(context.Background()) }

	at(5 * time.Hour) // not recording
	started = &start
	at(2*time.Hour + 59*time.Minute)
	at(3 * time.Hour)
	at(3*time.Hour + 30*time.Minute) // once per hour only
	at(4*time.Hour + time.Minute)
	if want := []int{3, 4}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}

	// A failure is retried 10 minutes later, not every minute.
	fail = true
	at(5 * time.Hour)
	fail = false
	at(5*time.Hour + 5*time.Minute)
	at(5*time.Hour + 11*time.Minute)
	if want := []int{3, 4, 5}; !slices.Equal(sent, want) {
		t.Fatalf("after retry sent %v, want %v", sent, want)
	}

	// A new recording starts counting again.
	start2 := start.Add(6 * time.Hour)
	started = &start2
	at(6*time.Hour + 2*time.Hour)
	if len(sent) != 3 {
		t.Fatalf("new recording alerted too early: %v", sent)
	}

	// Off when hours is 0 or emails are not configured.
	cfg.Notify.LongRecordingHours = 0
	at(6*time.Hour + 4*time.Hour)
	cfg.Notify.LongRecordingHours = 3
	cfg.Notify.ResendAPIKey = ""
	at(6*time.Hour + 5*time.Hour)
	if len(sent) != 3 {
		t.Fatalf("alert sent while off: %v", sent)
	}
}
