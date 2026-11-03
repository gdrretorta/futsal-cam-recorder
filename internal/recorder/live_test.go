package recorder

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var segRef = regexp.MustCompile(`(?m)^#EXTINF:([0-9.]+),\n/api/live/seg/(\d+)$`)
var mapRef = regexp.MustCompile(`#EXT-X-MAP:URI="/api/live/init/(\d+)"`)

// checkLive fetches the playlist, its init section and segments, and checks
// that together they are a valid MP4 that ffmpeg decodes.
func checkLive(t *testing.T, r *Recorder, ffmpeg string) {
	t.Helper()
	pl, err := r.LivePlaylist(context.Background(), "viewer-1")
	if err != nil {
		t.Fatalf("LivePlaylist: %v", err)
	}
	text := string(pl)
	m := mapRef.FindStringSubmatch(text)
	segs := segRef.FindAllStringSubmatch(text, -1)
	if m == nil || len(segs) == 0 || !strings.HasPrefix(text, "#EXTM3U\n") {
		t.Fatalf("bad playlist:\n%s", text)
	}
	gen, _ := strconv.Atoi(m[1])
	init, err := r.LiveInit(gen)
	if err != nil || !bytes.Contains(init[:16], []byte("ftyp")) || !bytes.Contains(init, []byte("moov")) {
		t.Fatalf("LiveInit = %d bytes, %v", len(init), err)
	}
	all := append([]byte(nil), init...)
	for _, s := range segs {
		dur, _ := strconv.ParseFloat(s[1], 64)
		if dur < 0.9 || dur > 1.1 {
			t.Errorf("segment duration %v, want 1 s (one GOP)", dur)
		}
		seq, _ := strconv.Atoi(s[2])
		data, err := r.LiveSegment(seq)
		if err != nil || string(data[4:8]) != "moof" {
			t.Fatalf("LiveSegment(%d) = %d bytes, %v", seq, len(data), err)
		}
		all = append(all, data...)
	}
	if _, err := r.LiveSegment(1 << 30); err != ErrLiveGone {
		t.Errorf("unknown segment: %v, want ErrLiveGone", err)
	}
	file := filepath.Join(t.TempDir(), "live.mp4")
	os.WriteFile(file, all, 0o644)
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", file, "-f", "null", "-").CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("live segments don't decode: %v\n%s", err, out)
	}
}

// liveTestSource is like testSource, at a camera-like bitrate (about
// 7 Mbps): ffmpeg writes the MP4 through a 32 KB buffer, so a tiny test
// stream would take seconds to show up in the file.
func liveTestSource(t *testing.T, ffmpeg string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "camera.mp4")
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25", "-vf", "noise=alls=30:allf=t", "-t", "20",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-pix_fmt", "yuv420p", src).CombinedOutput()
	if err != nil {
		t.Fatalf("creating test source: %v\n%s", err, out)
	}
	return src
}

func liveRunning(r *Recorder) bool {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	return r.liveP != nil
}

func TestLiveWhileRecordingReadsTheRecording(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	r, dir := newTestRecorder(t, liveTestSource(t, ffmpeg))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "recording", func() bool { return r.Status().State == StateRecording })
	time.Sleep(3 * time.Second)
	checkLive(t, r, ffmpeg)
	if liveRunning(r) {
		t.Fatal("a live ffmpeg was started during a recording")
	}
	r.Stop()
	waitFor(t, "idle and finalized", func() bool {
		s := r.Status()
		return s.State == StateIdle && len(s.Finalizing) == 0
	})
	list, _ := r.List()
	if len(list) != 1 {
		t.Fatalf("List = %+v, want 1 recording", list)
	}
	data, _ := os.ReadFile(filepath.Join(dir, list[0].Name))
	if bytes.Contains(data, []byte("moof")) {
		t.Error("recording not finalized")
	}
}

func TestLiveWhileIdleStopsBeforeRecording(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	r, dir := newTestRecorder(t, liveTestSource(t, ffmpeg))
	checkLive(t, r, ffmpeg)
	if !liveRunning(r) {
		t.Fatal("no live ffmpeg while idle")
	}
	// The live ffmpeg also feeds the snapshot.
	waitFor(t, "live preview frame", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.liveFrame != nil
	})

	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "recording", func() bool { return r.Status().State == StateRecording })
	if liveRunning(r) {
		t.Fatal("live ffmpeg still running during the recording")
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "live", "*.mp4")); len(files) != 0 {
		t.Errorf("live files left: %v", files)
	}
	time.Sleep(3 * time.Second)
	checkLive(t, r, ffmpeg) // now from the recording, as a new timeline
	if !strings.Contains(string(must(r.LivePlaylist(context.Background(), "viewer-1"))), "#EXT-X-DISCONTINUITY-SEQUENCE:1") {
		t.Error("switching to the recording should start a new discontinuity sequence")
	}
	r.Stop()
	waitFor(t, "idle", func() bool { return r.Status().State == StateIdle })
	if list, _ := r.List(); len(list) != 1 {
		t.Fatalf("List = %+v, want only the recording (the live folder isn't one)", list)
	}
}

func TestLiveStopsWithoutViewers(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	r, dir := newTestRecorder(t, liveTestSource(t, ffmpeg))
	r.liveIdle = time.Second
	if _, err := r.LivePlaylist(context.Background(), "v"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "live ffmpeg to stop", func() bool { return !liveRunning(r) })
	if files, _ := filepath.Glob(filepath.Join(dir, "live", "*.mp4")); len(files) != 0 {
		t.Errorf("live file not removed: %v", files)
	}
}

func TestLiveCameraDownIsReportedAndNotRetriedAtOnce(t *testing.T) {
	r, _ := newTestRecorder(t, "rtsp://127.0.0.1:1/none")
	if _, err := r.LivePlaylist(context.Background(), "v"); err == nil || err == ErrLiveUnavailable {
		t.Fatalf("LivePlaylist with the camera down = %v, want the connection error", err)
	}
	r.liveMu.Lock()
	failed := r.liveErr != nil
	r.liveMu.Unlock()
	if !failed {
		t.Fatal("failure not remembered")
	}
	start := time.Now()
	if _, err := r.LivePlaylist(context.Background(), "v"); err == nil || time.Since(start) > time.Second {
		t.Fatalf("retry right away = %v after %s, want the remembered error at once", err, time.Since(start))
	}
}

func TestLiveViewerLimit(t *testing.T) {
	var x liveIndex
	for i := range LiveMaxViewers {
		if err := x.admit(strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.admit("extra"); err != ErrLiveFull {
		t.Fatalf("admit beyond the limit = %v, want ErrLiveFull", err)
	}
	if err := x.admit("0"); err != nil {
		t.Fatalf("a viewer already watching was refused: %v", err)
	}
	x.viewers["1"] = time.Now().Add(-liveViewerTimeout - time.Second)
	if err := x.admit("extra"); err != nil {
		t.Fatalf("a gone viewer's place wasn't freed: %v", err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
