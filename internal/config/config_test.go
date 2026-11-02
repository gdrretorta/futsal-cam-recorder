package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamURL(t *testing.T) {
	cases := []struct {
		cam  Camera
		want string
	}{
		{Camera{Host: "192.168.1.10", Port: 554, Path: "live1.sdp"}, "rtsp://192.168.1.10:554/live1.sdp"},
		{Camera{Host: "cam", Port: 8554, Path: "/a/b", User: "admin", Password: "p@ss:w"}, "rtsp://admin:p%40ss%3Aw@cam:8554/a/b"},
		{Camera{Host: "192.168.1.20", Port: 554, Path: "cam/realmonitor?channel=17&subtype=0", User: "futsal", Password: "pw"},
			"rtsp://futsal:pw@192.168.1.20:554/cam/realmonitor?channel=17&subtype=0"},
		{Camera{URL: "rtsp://x/y", Host: "ignored"}, "rtsp://x/y"},
		{Camera{}, ""},
	}
	for _, c := range cases {
		if got := c.cam.StreamURL(); got != c.want {
			t.Errorf("StreamURL(%+v) = %q, want %q", c.cam, got, c.want)
		}
	}
}

func TestRedact(t *testing.T) {
	got := Redact("rtsp://admin:secret@cam:554/live")
	if got != "rtsp://%2A%2A%2A@cam:554/live" && got != "rtsp://***@cam:554/live" {
		t.Fatalf("Redact leaked or mangled URL: %q", got)
	}
}

func TestOpenMigratesLegacyEnv(t *testing.T) {
	dir := t.TempDir()
	env := "\ufeff# comment\nRTSP_USER=admin\nRTSP_PASSWORD=\"secret\"\nRTSP_IP=10.0.0.5\nRTSP_PORT=8554\nRTSP_SDP=live1.sdp\nOUTPUT_DIR=D:\\videos\nLABEL_TITLE=Futsal\n"
	if err := os.WriteFile(filepath.Join(dir, "prod.env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.Get()
	want := Camera{Host: "10.0.0.5", Port: 8554, Path: "live1.sdp", User: "admin", Password: "secret"}
	if cfg.Camera != want {
		t.Errorf("camera = %+v, want %+v", cfg.Camera, want)
	}
	if cfg.Title != "Futsal" || cfg.RecordingsDir != `D:\videos` || cfg.Port != 8080 || !cfg.AutoUpdate {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Errorf("config.json not written: %v", err)
	}
}

func TestOpenDefaultsAndUpdate(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.RecordingsDir() != filepath.Join(dir, "recordings") {
		t.Errorf("RecordingsDir = %q", s.RecordingsDir())
	}
	cfg := s.Get()
	cfg.Camera.Host = "cam.local"
	cfg.RetentionDays = 30
	if err := s.Update(cfg); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get(); got.Camera.Host != "cam.local" || got.RetentionDays != 30 {
		t.Errorf("settings not persisted: %+v", got)
	}

	bad := cfg
	bad.Camera.Host = "admin@cam"
	if err := s.Update(bad); err == nil {
		t.Error("expected validation error for host with credentials")
	}
}

func TestPasswordHash(t *testing.T) {
	if _, err := HashPassword("short"); err != ErrWeakPassword {
		t.Fatalf("weak password err = %v", err)
	}
	h, err := HashPassword("palavra-passe çã")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "palavra-passe çã") || CheckPassword(h, "palavra-passe ca") || CheckPassword("garbage", "x") {
		t.Fatal("CheckPassword gave a wrong answer")
	}
	h2, _ := HashPassword("palavra-passe çã")
	if h == h2 {
		t.Fatal("hashes must be salted")
	}
}

func TestStorePicksUpExternalChanges(t *testing.T) {
	dir := t.TempDir()
	running, err := Open(dir) // e.g. the service
	if err != nil {
		t.Fatal(err)
	}
	// Make sure the next write gets a different mtime on coarse filesystems.
	time.Sleep(20 * time.Millisecond)
	cli, _ := Open(dir) // e.g. add-user from the command line
	if _, err := cli.SetUser("a@b.pt", RoleAdmin, "from the cli!"); err != nil {
		t.Fatal(err)
	}
	if _, err := running.Authenticate("a@b.pt", "from the cli!"); err != nil {
		t.Fatalf("running store did not reload: %v", err)
	}
	// A later save by the running store must keep the new user.
	cfg := running.Get()
	cfg.Title = "x"
	running.Update(cfg)
	if _, err := cli.Authenticate("a@b.pt", "from the cli!"); err != nil {
		t.Fatalf("user lost after update: %v", err)
	}
}

func TestValidateRenamesLegacyTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Simple RTSP Recorder": "Futsal Cam Recorder",
		"":                     "Futsal Cam Recorder",
		"Futsal Retorta":       "Futsal Retorta",
	} {
		c := Default()
		c.Title = in
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if c.Title != want {
			t.Errorf("title %q: got %q, want %q", in, c.Title, want)
		}
	}
}
