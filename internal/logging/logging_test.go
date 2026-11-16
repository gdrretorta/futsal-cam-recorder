package logging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// brokenWriter mimics stdout in a Windows service, which has no console.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("invalid handle") }

func TestLogsReachFileAndMemoryWhenStdoutFails(t *testing.T) {
	dir := t.TempDir()
	log, err := newLogger(dir, brokenWriter{})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello from the service")

	recent := log.Recent()
	if len(recent) != 1 || !strings.Contains(recent[0], "hello from the service") {
		t.Fatalf("Recent() = %q, want the logged line", recent)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "recorder.log"))
	if !strings.Contains(string(data), "hello from the service") {
		t.Fatalf("recorder.log = %q", data)
	}
}

func TestRecentIsNeverNil(t *testing.T) {
	// Encoded as JSON for /api/logs: must be [] rather than null.
	if got := Discard().Recent(); got == nil {
		t.Fatal("Recent() returned nil")
	}
}

func TestRedactURLs(t *testing.T) {
	in := "Error opening input file rtsp://admin:S3cret!@192.168.1.10:554/cam?channel=1."
	want := "Error opening input file rtsp://***@192.168.1.10:554/cam?channel=1."
	if got := RedactURLs(in); got != want {
		t.Fatalf("RedactURLs = %q, want %q", got, want)
	}
	if got := RedactURLs("rtsp://192.168.1.10/x a@b.c"); got != "rtsp://192.168.1.10/x a@b.c" {
		t.Fatalf("changed a URL without credentials: %q", got)
	}
}

func TestOldLogsAreScrubbed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "recorder.log.1"), []byte("ffmpeg: rtsp://admin:pw@cam/x failed\n"), 0o644)
	log, err := newLogger(dir, brokenWriter{})
	if err != nil {
		t.Fatal(err)
	}
	w := &LineWriter{Log: log.Logger, Prefix: "ffmpeg: "}
	w.Write([]byte("Error opening input file rtsp://admin:pw@cam/x.\n"))
	for _, name := range []string{"recorder.log", "recorder.log.1"} {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		if strings.Contains(string(data), "pw@") {
			t.Fatalf("%s still holds the password: %q", name, data)
		}
	}
	if strings.Contains(strings.Join(log.Recent(), "\n"), "pw@") || strings.Contains(w.LastLine(), "pw@") {
		t.Fatal("password reached the in-memory log or LastLine")
	}
}
