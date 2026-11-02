package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gdrretorta/futsal-cam-recorder/internal/logging"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v2.0.1", "v2.0.0", true},
		{"v2.1.0", "v2.0.9", true},
		{"v10.0.0", "v9.9.9", true},
		{"v2.0.0", "v2.0.0", false},
		{"v1.9.0", "v2.0.0", false},
		{"v2.0.0", "dev", false},
		{"garbage", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

type fakeRelease struct {
	srv     *httptest.Server
	binary  []byte
	sumLine string
}

func newFakeRelease(t *testing.T, tag string, binary []byte) *fakeRelease {
	sum := sha256.Sum256(binary)
	f := &fakeRelease{binary: binary, sumLine: hex.EncodeToString(sum[:]) + "  " + AssetName + "\n"}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":"%s/exe"},{"name":"checksums.txt","browser_download_url":"%s/sums"}]}`,
			tag, AssetName, f.srv.URL, f.srv.URL)
	})
	mux.HandleFunc("/exe", func(w http.ResponseWriter, r *http.Request) { w.Write(f.binary) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, f.sumLine) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func setup(t *testing.T, current string, rel *fakeRelease, idle *bool) (*Updater, string, *bool) {
	exe := filepath.Join(t.TempDir(), AssetName)
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	restarted := new(bool)
	u := New(Options{
		Repo: "o/r", Current: current, ExePath: exe, Log: logging.Discard().Logger,
		Enabled: func() bool { return true },
		TryLock: func() bool { return *idle },
		Unlock:  func() {},
		Restart: func() { *restarted = true },
		APIBase: rel.srv.URL,
	})
	return u, exe, restarted
}

func TestUpdateWaitsForIdleThenSwaps(t *testing.T) {
	rel := newFakeRelease(t, "v2.1.0", []byte("new binary"))
	idle := false
	u, exe, restarted := setup(t, "v2.0.0", rel, &idle)

	u.checkAndStage(context.Background())
	if s := u.Status(); s.State != "waiting" || s.Latest != "v2.1.0" {
		t.Fatalf("status = %+v, want waiting for v2.1.0", s)
	}
	u.tryApply() // still recording
	if *restarted {
		t.Fatal("applied update while recording")
	}
	idle = true
	u.tryApply()
	if !*restarted {
		t.Fatal("update not applied when idle")
	}
	if got, _ := os.ReadFile(exe); string(got) != "new binary" {
		t.Errorf("exe = %q", got)
	}
	if got, _ := os.ReadFile(exe + ".old"); string(got) != "old binary" {
		t.Errorf("exe.old = %q", got)
	}
	u.CleanupOld()
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Error("exe.old not cleaned up")
	}
}

func TestNoUpdateWhenCurrent(t *testing.T) {
	rel := newFakeRelease(t, "v2.0.0", []byte("same"))
	idle := true
	u, _, restarted := setup(t, "v2.0.0", rel, &idle)
	u.checkAndStage(context.Background())
	u.tryApply()
	if *restarted || u.Status().State != "idle" {
		t.Fatalf("unexpected update: %+v", u.Status())
	}
}

func TestChecksumMismatchRejected(t *testing.T) {
	rel := newFakeRelease(t, "v2.1.0", []byte("new binary"))
	rel.sumLine = "0000  " + AssetName + "\n"
	idle := true
	u, exe, restarted := setup(t, "v2.0.0", rel, &idle)
	u.checkAndStage(context.Background())
	u.tryApply()
	if *restarted || u.Status().State != "error" {
		t.Fatalf("bad checksum accepted: %+v", u.Status())
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Errorf("exe replaced: %q", got)
	}
}
