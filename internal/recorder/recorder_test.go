package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/logging"
)

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	return path
}

// testSource creates a short H.264 file that the recorder reads at native
// speed, standing in for a camera.
func testSource(t *testing.T, ffmpeg string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "camera.mp4")
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25", "-t", "20",
		"-c:v", "libx264", "-g", "25", "-pix_fmt", "yuv420p", src).CombinedOutput()
	if err != nil {
		t.Fatalf("creating test source: %v\n%s", err, out)
	}
	return src
}

func newTestRecorder(t *testing.T, url string) (*Recorder, string) {
	t.Helper()
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()
	r := New(ffmpeg, func() (string, string) { return url, dir }, filepath.Join(dir, "state.json"), logging.Discard().Logger)
	r.connectTimeout = 5 * time.Second
	r.backoff = []time.Duration{100 * time.Millisecond}
	return r, dir
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRecordStopAndFinalize(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	r, dir := newTestRecorder(t, testSource(t, ffmpeg))

	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != ErrBusy {
		t.Fatalf("second Start = %v, want ErrBusy", err)
	}
	waitFor(t, "recording", func() bool { return r.Status().State == StateRecording })
	if r.Status().StartedAt == nil {
		t.Fatal("StartedAt not set while recording")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal("recording state not persisted")
	}
	if r.TryLock() {
		t.Fatal("TryLock succeeded during a recording")
	}
	if list, _ := r.List(); len(list) != 0 {
		t.Fatalf("current recording should not be listed: %+v", list)
	}

	// The preview comes from the recording itself, not a second connection.
	waitFor(t, "live preview frame", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.liveFrame != nil
	})
	snap, err := r.Snapshot(context.Background())
	if err != nil || !bytes.HasPrefix(snap, []byte{0xff, 0xd8}) {
		t.Fatalf("Snapshot while recording = %d bytes, %v; want a JPEG", len(snap), err)
	}

	time.Sleep(2 * time.Second)
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "idle and finalized", func() bool {
		s := r.Status()
		return s.State == StateIdle && len(s.Finalizing) == 0
	})

	list, err := r.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v; want 1 recording", list, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, list[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	moov, mdat := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat"))
	if moov < 0 || mdat < 0 || moov > mdat {
		t.Errorf("file not finalized with faststart (moov=%d mdat=%d)", moov, mdat)
	}
	if bytes.Contains(data, []byte("moof")) {
		t.Error("finalized file is still fragmented")
	}
	// Decoding only keyframes for the preview must not drop frames from the file.
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", filepath.Join(dir, list[0].Name),
		"-map", "0:v", "-c", "copy", "-f", "framecrc", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := parseFrameCRC(bytes.NewReader(out)); !ok || st.FPS < 24 || st.FPS > 26 || st.Gaps != 0 {
		t.Errorf("recorded video stats = %+v, want 25 fps without gaps", st)
	}
	r.mu.Lock()
	if r.liveFrame != nil {
		t.Error("live preview frame kept after the session ended")
	}
	r.mu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Error("state file should be removed after a user stop")
	}
	if !r.TryLock() {
		t.Fatal("TryLock failed while idle")
	}
	if err := r.Start(); err != ErrUpdating {
		t.Fatalf("Start while locked = %v, want ErrUpdating", err)
	}
	r.Unlock()
}

// TestRecordCameraQuirks covers what the real camera (DCS-2530L) sends: AAC
// with ADTS headers, and a stream joined mid-GOP with a keyframe interval
// longer than ffmpeg's default probe (10 s here).
func TestRecordCameraQuirks(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	full := filepath.Join(t.TempDir(), "full.ts")
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25", "-f", "lavfi", "-i", "sine", "-t", "30",
		"-c:v", "libx264", "-g", "250", "-x264opts", "no-scenecut", "-pix_fmt", "yuv420p",
		"-c:a", "aac", full).CombinedOutput()
	if err != nil {
		t.Fatalf("creating test source: %v\n%s", err, out)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	// Start 1 s in (aligned to a 188-byte TS packet), well before the next keyframe.
	src := filepath.Join(t.TempDir(), "camera.ts")
	if err := os.WriteFile(src, data[len(data)/30/188*188:], 0o644); err != nil {
		t.Fatal(err)
	}

	r, _ := newTestRecorder(t, src)
	r.connectTimeout = 20 * time.Second
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "recording", func() bool {
		s := r.Status()
		if s.State == StateIdle {
			t.Fatalf("recording failed: %s", s.Error)
		}
		return s.State == StateRecording
	})
	time.Sleep(time.Second)
	if s := r.Status(); s.State != StateRecording {
		t.Fatalf("state = %s (%s), want recording", s.State, s.Error)
	}
	r.Stop()
	waitFor(t, "idle", func() bool { return r.Status().State == StateIdle })
}

func TestConnectionFailureReportsError(t *testing.T) {
	r, dir := newTestRecorder(t, "rtsp://127.0.0.1:1/nothing")
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failure", func() bool { return r.Status().State == StateIdle })
	if r.Status().Error == "" {
		t.Error("expected an error message")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.mp4"))
	if len(matches) != 0 {
		t.Errorf("empty files left behind: %v", matches)
	}
}

func TestStopWithoutRecording(t *testing.T) {
	r, _ := newTestRecorder(t, "rtsp://127.0.0.1:1/x")
	if err := r.Stop(); err != ErrNotRecording {
		t.Fatalf("Stop = %v, want ErrNotRecording", err)
	}
}

func TestParseFrameCRC(t *testing.T) {
	// 15 fps (6000 ticks of 1/90000 s), a keyframe every 15 frames, and one
	// 1 s hiccup after frame 30.
	var b strings.Builder
	b.WriteString("#tb 0: 1/90000\n#media_type 0: video\n")
	dts := int64(0)
	for i := 0; i < 60; i++ {
		flags := ", F=0x0"
		if i%15 == 0 {
			flags = ""
		}
		fmt.Fprintf(&b, "0, %d, %d, 6000, 1000, 0x00000000%s\n", dts, dts, flags)
		dts += 6000
		if i == 30 {
			dts += 90000
		}
	}
	st, ok := parseFrameCRC(strings.NewReader(b.String()))
	if !ok {
		t.Fatal("no stats")
	}
	if st.Frames != 60 || st.Gaps != 1 || st.MaxGap.Round(time.Millisecond) != 1067*time.Millisecond ||
		st.Lost.Round(time.Millisecond) != time.Second || st.KeyframeEvery.Round(time.Millisecond) != 1217*time.Millisecond {
		t.Errorf("stats = %+v", st)
	}
	if len(st.GapList) != 1 || st.GapList[0].At.Round(time.Millisecond) != 2*time.Second {
		t.Errorf("gap list = %+v, want one gap at 2s", st.GapList)
	}
	if got := formatGaps([]Gap{{At: 192 * time.Second, Length: 1030 * time.Millisecond}, {At: 3723 * time.Second, Length: 360 * time.Millisecond}}); got != "03:12(1.03s) 1:02:03(0.36s)" {
		t.Errorf("formatGaps = %q", got)
	}
	if _, ok := parseFrameCRC(strings.NewReader("#tb 0: 1/90000\n")); ok {
		t.Error("stats from an empty listing")
	}
}

func TestValidName(t *testing.T) {
	ok := []string{"2026-10-29_153000.mp4", "2026-10-29_153000_2.mp4", "jogo final.mp4"}
	bad := []string{"../x.mp4", "..\\x.mp4", "a/b.mp4", ".hidden.mp4", "x.mp4.tmp", "config.json", "a..mp4", ""}
	for _, n := range ok {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false", n)
		}
	}
	for _, n := range bad {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true", n)
		}
	}
}

// A session resumed after a restart keeps its original start time, so the
// timer and the long-recording alert count from when it really began.
func TestResumeKeepsStartTime(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	r, dir := newTestRecorder(t, testSource(t, ffmpeg))
	stateFile := filepath.Join(dir, "state.json")

	// A fresh recording stores the moment the camera connected.
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "recording", func() bool { return r.Status().State == StateRecording })
	started := *r.Status().StartedAt
	var st struct {
		Since time.Time `json:"since"`
	}
	data, _ := os.ReadFile(stateFile)
	if json.Unmarshal(data, &st); !st.Since.Equal(started) {
		t.Fatalf("state file since = %v, want the connection time %v", st.Since, started)
	}
	r.Shutdown(context.Background()) // like a service restart: the state file stays

	since := time.Now().Add(-2 * time.Hour).Round(time.Second)
	os.WriteFile(stateFile, []byte(`{"recording":true,"since":"`+since.Format(time.RFC3339)+`"}`), 0o644)
	r2, _ := newTestRecorder(t, testSource(t, ffmpeg))
	r2.stateFile = stateFile
	r2.Resume()
	waitFor(t, "recording", func() bool { return r2.Status().State == StateRecording })
	if got := r2.Status().StartedAt; got == nil || !got.Equal(since) {
		t.Fatalf("resumed StartedAt = %v, want %v", got, since)
	}
	r2.Stop()
	waitFor(t, "idle", func() bool { return r2.Status().State == StateIdle })
	if _, err := os.Stat(stateFile); err == nil {
		t.Fatal("state file left after stop")
	}
}
