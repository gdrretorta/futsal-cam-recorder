package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/config"
	"github.com/gdrretorta/futsal-cam-recorder/internal/logging"
)

func TestHealthWatcher(t *testing.T) {
	cfg := testConfig()
	cfg.Notify.CameraDownMinutes, cfg.Notify.LowDiskGB = 5, 20
	start := time.Date(2026, 11, 1, 21, 0, 0, 0, time.Local)
	now := start
	recording, down, reason := false, false, ""
	free := uint64(100) << 30
	var sent []string
	fail := false
	w := &HealthWatcher{
		Config: func() config.Config { return cfg },
		Camera: func() (bool, bool, string) { return recording, down, reason },
		Disk:   func() (uint64, uint64, error) { return free, 500 << 30, nil },
		Log:    logging.Discard().Logger,
		Now:    func() time.Time { return now },
		Send: func(_ context.Context, _ config.Notify, e Email) error {
			if fail {
				return errors.New("offline")
			}
			if len(e.To) != 2 {
				t.Errorf("to = %v", e.To)
			}
			sent = append(sent, strings.TrimPrefix(e.Subject, "Futsal: "))
			return nil
		},
	}
	at := func(d time.Duration) { now = start.Add(d); w.Check(context.Background()) }
	expect := func(want ...string) {
		t.Helper()
		if !slices.Equal(sent, want) {
			t.Fatalf("sent %q, want %q", sent, want)
		}
	}

	// Camera: not while idle, only after 5 minutes down while recording.
	down = true
	at(0)
	at(10 * time.Minute)
	recording, reason = true, "ligação à câmara perdida"
	at(11 * time.Minute)
	at(15 * time.Minute)
	expect()
	at(16 * time.Minute)
	at(30 * time.Minute) // once per outage
	expect("câmara sem imagem")
	down = false
	at(31 * time.Minute)
	at(32 * time.Minute)
	expect("câmara sem imagem", "a câmara voltou")

	// A short drop sends nothing; a failure is retried 10 minutes later.
	down = true
	at(40 * time.Minute)
	down = false
	at(43 * time.Minute)
	down, fail = true, true
	at(50 * time.Minute)
	at(56 * time.Minute)
	fail = false
	at(60 * time.Minute)
	expect("câmara sem imagem", "a câmara voltou")
	at(66 * time.Minute)
	expect("câmara sem imagem", "a câmara voltou", "câmara sem imagem")

	// Stopping the recording during an outage doesn't say the camera is back.
	recording = false
	at(70 * time.Minute)
	expect("câmara sem imagem", "a câmara voltou", "câmara sem imagem")
	down = false

	// Disk: alert below 20 GB, again after a day, re-armed only 1 GB above.
	sent = nil
	free = 19 << 30
	at(2 * time.Hour)
	at(3 * time.Hour)
	expect("pouco espaço em disco")
	free = 20<<30 + 1
	at(4 * time.Hour)
	free = 19 << 30
	at(5 * time.Hour)
	expect("pouco espaço em disco")
	at(26 * time.Hour)
	expect("pouco espaço em disco", "pouco espaço em disco")
	free = 22 << 30
	at(27 * time.Hour)
	free = 19 << 30
	at(28 * time.Hour)
	expect("pouco espaço em disco", "pouco espaço em disco", "pouco espaço em disco")

	// Off at 0 or without emails.
	free = 1 << 30
	cfg.Notify.LowDiskGB = 0
	at(60 * time.Hour)
	cfg.Notify.LowDiskGB = 20
	cfg.Notify.ResendAPIKey = ""
	at(61 * time.Hour)
	if len(sent) != 3 {
		t.Fatalf("alert sent while off: %q", sent)
	}
}

func TestHealthEmails(t *testing.T) {
	cfg := testConfig()
	since := time.Date(2026, 11, 1, 21, 3, 0, 0, time.Local)
	e := CameraDownEmail(cfg, since, since.Add(5*time.Minute), "timeout")
	if !strings.Contains(e.Text, "desde as 21:03 (há 5 minutos)") || !strings.Contains(e.Text, "Último erro: timeout") || !strings.Contains(e.Text, "https://futsal.retorta.com") {
		t.Errorf("down = %s", e.Text)
	}
	cfg.RetentionDays = 30
	e = LowDiskEmail(cfg, 15<<29, 500<<30)
	if !strings.Contains(e.Text, "7,5 GB livres (de 500,0 GB)") || !strings.Contains(e.Text, "mais de 30 dias") {
		t.Errorf("disk = %s", e.Text)
	}
}
