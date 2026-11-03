// Package recorder drives ffmpeg to record an RTSP stream to MP4 files.
//
// Lifecycle of a recording session:
//
//	idle → connecting → recording ⇄ reconnecting → stopping → idle
//
// Files are written as fragmented MP4 so they stay playable if the process or
// PC dies mid-recording. When a file is closed it is "finalized": remuxed
// (stream copy, no re-encode) into a regular MP4 with the index at the start,
// which browsers seek in much better.
package recorder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/config"
	"github.com/gdrretorta/futsal-cam-recorder/internal/logging"
)

var (
	ErrBusy          = errors.New("já existe uma gravação em curso")
	ErrNotRecording  = errors.New("não há nenhuma gravação em curso")
	ErrUpdating      = errors.New("a aplicação está a ser atualizada, tente daqui a pouco")
	ErrNotConfigured = errors.New("a câmara ainda não está configurada")
)

// State of the recorder.
type State string

const (
	StateIdle         State = "idle"
	StateConnecting   State = "connecting"
	StateRecording    State = "recording"
	StateReconnecting State = "reconnecting"
	StateStopping     State = "stopping"
)

// Status is a snapshot of the recorder, as served to the UI.
type Status struct {
	State      State      `json:"state"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	File       string     `json:"file,omitempty"`
	Error      string     `json:"error,omitempty"`
	Finalizing []string   `json:"finalizing,omitempty"`
}

// Recording is a file in the recordings directory.
type Recording struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"modTime"`
	Processing bool      `json:"processing,omitempty"`
	Protected  bool      `json:"protected,omitempty"` // "não apagar" (protect.go)
}

// Source returns the stream URL and the recordings directory. It is called at
// the start of every file so settings changes apply to the next file.
type Source func() (url, dir string)

// Recorder records one stream at a time. Safe for concurrent use.
type Recorder struct {
	ffmpeg    string
	source    Source
	log       *slog.Logger
	stateFile string

	// Tunables, overridden in tests.
	connectTimeout time.Duration
	stallTimeout   time.Duration
	backoff        []time.Duration

	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup

	mu         sync.Mutex
	state      State
	startedAt  time.Time
	file       string
	lastErr    string
	finalizing map[string]bool
	locked     bool
	stop       chan struct{}
	done       chan struct{}
	// Latest preview frame produced by the recording ffmpeg, so the preview
	// doesn't open a second stream to the camera while recording.
	liveFrame   []byte
	liveFrameAt time.Time

	// Live view (live.go). liveMu is never taken while holding mu.
	liveMu     sync.Mutex
	liveP      *liveProc
	liveErr    error
	liveFailAt time.Time
	liveIdle   time.Duration
	index      liveIndex

	keepMu sync.Mutex // protected.json (protect.go); never taken while holding mu
}

// New creates a recorder. stateFile remembers whether a recording was in
// progress so it can resume after a restart (power cut, Windows update…).
func New(ffmpeg string, source Source, stateFile string, log *slog.Logger) *Recorder {
	ctx, cancel := context.WithCancel(context.Background())
	return &Recorder{
		ffmpeg:         ffmpeg,
		source:         source,
		log:            log,
		stateFile:      stateFile,
		connectTimeout: 30 * time.Second,
		stallTimeout:   30 * time.Second,
		backoff:        []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second},
		liveIdle:       30 * time.Second,
		bgCtx:          ctx,
		bgCancel:       cancel,
		state:          StateIdle,
		finalizing:     map[string]bool{},
	}
}

// Status returns the current state.
func (r *Recorder) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Status{State: r.state, File: r.file, Error: r.lastErr}
	if !r.startedAt.IsZero() {
		t := r.startedAt
		s.StartedAt = &t
	}
	for name := range r.finalizing {
		s.Finalizing = append(s.Finalizing, name)
	}
	sort.Strings(s.Finalizing)
	return s
}

// Start begins a recording session. It returns once ffmpeg is launched; the
// connection result shows up in Status.
func (r *Recorder) Start() error { return r.start(false, time.Time{}) }

// Resume restarts a session that was interrupted by a process restart. The
// session keeps its original start time, so the timer (and the long-recording
// alert) count from when it really began.
func (r *Recorder) Resume() {
	data, err := os.ReadFile(r.stateFile)
	if err != nil {
		return
	}
	var st struct {
		Since time.Time `json:"since"`
	}
	json.Unmarshal(data, &st) // a zero time just restarts the count
	r.log.Info("resuming recording interrupted by restart", "since", st.Since)
	if err := r.start(true, st.Since); err != nil {
		r.log.Error("resume failed", "err", err)
	}
}

func (r *Recorder) start(resume bool, since time.Time) error {
	url, dir := r.source()
	if url == "" {
		return ErrNotConfigured
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.locked:
		return ErrUpdating
	case r.state != StateIdle:
		return ErrBusy
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("não foi possível criar a pasta de gravações: %w", err)
	}
	r.state, r.startedAt, r.file, r.lastErr = StateConnecting, since, "", ""
	r.stop, r.done = make(chan struct{}), make(chan struct{})
	r.persist(true)
	go r.session(r.stop, r.done, resume)
	return nil
}

// Stop ends the session. It returns immediately; the state goes to stopping
// and then idle once ffmpeg has closed the file.
func (r *Recorder) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case StateIdle:
		return ErrNotRecording
	case StateStopping:
		return nil
	}
	r.state = StateStopping
	close(r.stop)
	r.persist(false)
	return nil
}

// Shutdown stops any session without forgetting it (so it resumes on next
// start), waiting at most until ctx is done.
func (r *Recorder) Shutdown(ctx context.Context) {
	r.stopLive()
	r.mu.Lock()
	done := r.done
	if r.state != StateIdle && r.state != StateStopping {
		r.state = StateStopping
		close(r.stop)
	}
	r.mu.Unlock()
	defer r.bgCancel()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	// Let finalizations finish while there is time; files that don't make
	// it stay fragmented, which is still playable.
	finished := make(chan struct{})
	go func() { r.bg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		r.bgCancel()
		<-finished
	}
}

// TryLock prevents new recordings while nothing is running. Used by the
// updater so it never replaces the binary mid-recording.
func (r *Recorder) TryLock() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locked || r.state != StateIdle || len(r.finalizing) > 0 {
		return false
	}
	r.locked = true
	return true
}

// Unlock releases TryLock.
func (r *Recorder) Unlock() {
	r.mu.Lock()
	r.locked = false
	r.mu.Unlock()
}

// IsActive reports whether a file is being written or finalized.
func (r *Recorder) IsActive(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return (r.state != StateIdle && name == r.file) || r.finalizing[name]
}

var validName = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_\-. ]*\.mp4$`)

// ValidName reports whether name is a plain recording file name (no paths).
func ValidName(name string) bool {
	return validName.MatchString(name) && !strings.Contains(name, "..")
}

// List returns the recordings, newest first. The file currently being
// recorded is left out.
func (r *Recorder) List() ([]Recording, error) {
	_, dir := r.source()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Recording{}, nil
	}
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	current := ""
	if r.state != StateIdle {
		current = r.file
	}
	finalizing := make(map[string]bool, len(r.finalizing))
	for k := range r.finalizing {
		finalizing[k] = true
	}
	r.mu.Unlock()
	r.keepMu.Lock()
	keep := r.protected(dir)
	r.keepMu.Unlock()

	list := []Recording{}
	for _, e := range entries {
		if e.IsDir() || !ValidName(e.Name()) || e.Name() == current {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, Recording{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime(), Processing: finalizing[e.Name()], Protected: keep[e.Name()]})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name > list[j].Name })
	return list, nil
}

// Prune deletes recordings older than the given number of days, except the
// protected ones.
func (r *Recorder) Prune(days int) {
	if days <= 0 {
		return
	}
	list, err := r.List()
	if err != nil {
		r.log.Error("retention: list failed", "err", err)
		return
	}
	_, dir := r.source()
	cutoff := time.Now().AddDate(0, 0, -days)
	for _, rec := range list {
		if rec.ModTime.Before(cutoff) && !rec.Protected && !r.IsActive(rec.Name) {
			if err := os.Remove(filepath.Join(dir, rec.Name)); err != nil {
				r.log.Error("retention: delete failed", "file", rec.Name, "err", err)
				continue
			}
			r.log.Info("retention: deleted old recording", "file", rec.Name)
		}
	}
}

// CleanupTemp removes leftovers of interrupted finalizations.
func (r *Recorder) CleanupTemp() {
	_, dir := r.source()
	matches, _ := filepath.Glob(filepath.Join(dir, "*.mp4.tmp"))
	for _, m := range matches {
		os.Remove(m)
	}
}

// session runs until stopped, reconnecting whenever ffmpeg exits.
func (r *Recorder) session(stop, done chan struct{}, resume bool) {
	defer close(done)
	// The live view's own ffmpeg must have closed its connection before the
	// recording opens one: the camera never sends two streams at once.
	r.stopLive()
	attempt := 0
loop:
	for {
		url, dir := r.source()
		path := uniquePath(dir, time.Now())
		r.log.Info("recording to file", "file", filepath.Base(path))
		connected, stopped, err := r.recordOnce(url, path, stop)
		r.closeFile(path)
		if stopped {
			break
		}
		msg := "ligação à câmara perdida"
		if err != nil {
			msg = err.Error()
		}
		r.log.Warn("ffmpeg exited", "connected", connected, "err", msg)

		r.mu.Lock()
		if r.state == StateStopping {
			r.mu.Unlock()
			break
		}
		if !connected && r.startedAt.IsZero() && !resume {
			// The very first connection failed: report it instead of retrying forever.
			r.lastErr = "não foi possível ligar à câmara: " + msg
			r.persist(false)
			r.mu.Unlock()
			break
		}
		r.state, r.lastErr, r.file = StateReconnecting, msg, ""
		r.mu.Unlock()

		if connected {
			attempt = 0
		}
		wait := r.backoff[min(attempt, len(r.backoff)-1)]
		attempt++
		select {
		case <-stop:
			break loop
		case <-time.After(wait):
		}
	}
	r.mu.Lock()
	r.state, r.file, r.startedAt, r.stop = StateIdle, "", time.Time{}, nil
	r.liveFrame, r.liveFrameAt = nil, time.Time{}
	r.mu.Unlock()
	r.log.Info("recording session ended")
}

// recordOnce runs a single ffmpeg process writing to path.
func (r *Recorder) recordOnce(url, path string, stop chan struct{}) (connected, stopped bool, err error) {
	cmd := exec.Command(r.ffmpeg, recordArgs(url, path)...)
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false, false, err
	}
	frames, err := cmd.StdoutPipe()
	if err != nil {
		return false, false, err
	}
	stderr := &logging.LineWriter{Log: r.log, Prefix: "ffmpeg: "}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return false, false, fmt.Errorf("não foi possível iniciar o ffmpeg (%s): %w", r.ffmpeg, err)
	}
	r.mu.Lock()
	r.file = filepath.Base(path)
	r.mu.Unlock()

	// cmd.Wait closes the pipe, so read all of it before waiting.
	readDone := make(chan struct{})
	go func() { r.readFrames(frames); close(readDone) }()
	exited := make(chan error, 1)
	go func() { <-readDone; exited <- cmd.Wait() }()
	exitErr := func(err error) error {
		if line := stderr.LastLine(); line != "" {
			return errors.New(line)
		}
		if err != nil {
			return fmt.Errorf("ffmpeg terminou: %w", err)
		}
		return errors.New("ffmpeg terminou inesperadamente")
	}

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(r.connectTimeout)
	defer timeout.Stop()
	var lastSize int64
	lastGrowth := time.Now()
	for {
		select {
		case err := <-exited:
			return connected, false, exitErr(err)
		case <-stop:
			quit(cmd, stdin, exited)
			return connected, true, nil
		case <-timeout.C:
			if !connected {
				cmd.Process.Kill()
				<-exited
				return false, false, fmt.Errorf("a câmara não respondeu em %s", r.connectTimeout)
			}
		case <-tick.C:
			size := fileSize(path)
			if size > lastSize {
				lastSize, lastGrowth = size, time.Now()
			}
			if !connected && size > 0 {
				connected = true
				r.mu.Lock()
				if r.state != StateStopping {
					r.state, r.lastErr = StateRecording, ""
				}
				if r.startedAt.IsZero() {
					r.startedAt = time.Now()
					// Remember the real start for a resume, unless a stop
					// already removed the state file.
					if r.state != StateStopping {
						r.persist(true)
					}
				}
				r.mu.Unlock()
				r.log.Info("camera connected, recording", "url", config.Redact(url))
			}
			if connected && time.Since(lastGrowth) > r.stallTimeout {
				cmd.Process.Kill()
				<-exited
				return true, false, errors.New("o vídeo da câmara parou de chegar")
			}
		}
	}
}

// readFrames keeps the latest JPEG of the multipart stream that ffmpeg writes
// to stdout. It always reads to EOF: if nobody read the pipe, ffmpeg would
// block and stop recording.
func (r *Recorder) readFrames(stdout io.Reader) {
	defer io.Copy(io.Discard, stdout)
	mr := multipart.NewReader(stdout, "ffmpeg")
	for {
		part, err := mr.NextPart()
		if err != nil {
			return
		}
		data, err := io.ReadAll(io.LimitReader(part, 8<<20))
		if err != nil {
			return
		}
		if len(data) == 0 {
			continue
		}
		r.mu.Lock()
		r.liveFrame, r.liveFrameAt = data, time.Now()
		r.mu.Unlock()
	}
}

// quit asks ffmpeg to finish cleanly ("q" on stdin), killing it if it hangs.
func quit(cmd *exec.Cmd, stdin io.WriteCloser, exited chan error) {
	io.WriteString(stdin, "q")
	stdin.Close()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
}

// closeFile deletes empty files and finalizes the rest in the background.
func (r *Recorder) closeFile(path string) {
	if fileSize(path) <= 0 {
		os.Remove(path)
		return
	}
	name := filepath.Base(path)
	r.mu.Lock()
	r.finalizing[name] = true
	r.mu.Unlock()
	r.bg.Add(1)
	go func() {
		defer r.bg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.finalizing, name)
			r.mu.Unlock()
		}()
		if err := r.finalize(path); err != nil {
			r.log.Warn("finalize failed, keeping fragmented file", "file", name, "err", err)
			return
		}
		r.log.Info("recording finalized", "file", name)
	}()
}

// finalize remuxes path with the index at the start. The same pass lists the
// video packets (framecrc on stdout) to log how smooth the recording is.
func (r *Recorder) finalize(path string) error {
	tmp := path + ".tmp"
	cmd := exec.CommandContext(r.bgCtx, r.ffmpeg, "-hide_banner", "-nostats", "-loglevel", "error",
		"-i", path, "-map", "0", "-c", "copy", "-movflags", "+faststart", "-f", "mp4", "-y", tmp,
		"-map", "0:v:0", "-c", "copy", "-f", "framecrc", "pipe:1")
	configureProcess(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// Retried: on Windows, a reader that has the file open for an instant
	// (the live view) can make the rename fail.
	var err error
	for try := 0; try < 5; try++ {
		if err = os.Rename(tmp, path); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if st, ok := parseFrameCRC(&stdout); ok {
		r.log.Info("recording stats", "file", filepath.Base(path), "duration", st.Duration.Round(time.Second).String(),
			"fps", fmt.Sprintf("%.1f", st.FPS), "kbps", st.Kbps, "keyframeEvery", st.KeyframeEvery.Round(100*time.Millisecond).String(),
			"gaps", st.Gaps, "maxGap", st.MaxGap.Round(10*time.Millisecond).String(), "lost", st.Lost.Round(100*time.Millisecond).String(),
			"gapsAt", formatGaps(st.GapList))
	}
	return nil
}

// Stats describes the video of a recording. A gap is a pause between two
// frames much longer than usual: the camera skipped frames, which plays as
// a hiccup. Lost is the total time missing because of gaps.
type Stats struct {
	Frames        int
	Duration      time.Duration
	FPS           float64
	Kbps          int64
	KeyframeEvery time.Duration
	Gaps          int
	MaxGap        time.Duration
	Lost          time.Duration
	GapList       []Gap // the first maxGapList gaps
}

// Gap is a pause of Length that starts At into the recording.
type Gap struct {
	At, Length time.Duration
}

// maxGapList limits how many gaps are listed in the log line.
const maxGapList = 30

// formatGaps lists gaps as "03:12(1.03s) 11:40(0.36s)", with times from the
// start of the file so they can be found in the video.
func formatGaps(gaps []Gap) string {
	parts := make([]string, 0, len(gaps))
	for _, g := range gaps {
		sec := int(g.At / time.Second)
		at := fmt.Sprintf("%02d:%02d", sec/60, sec%60)
		if sec >= 3600 {
			at = fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
		}
		parts = append(parts, fmt.Sprintf("%s(%.2fs)", at, g.Length.Seconds()))
	}
	return strings.Join(parts, " ")
}

// minGap is the shortest pause counted as a gap, whatever the frame rate.
const minGap = 250 * time.Millisecond

// parseFrameCRC reads ffmpeg's framecrc output for one stream: "#tb 0: num/den"
// then lines "stream, dts, pts, duration, size, crc[, F=flags]". Keyframes
// have no F= field.
func parseFrameCRC(r io.Reader) (Stats, bool) {
	var num, den int64 = 1, 1
	var dts []time.Duration
	var size int64
	keyframes := 0
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#tb 0:") {
			fmt.Sscanf(strings.TrimPrefix(line, "#tb 0:"), "%d/%d", &num, &den)
			continue
		}
		if strings.HasPrefix(line, "#") || den == 0 {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 6 {
			continue
		}
		d, err1 := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		n, err2 := strconv.ParseInt(strings.TrimSpace(f[4]), 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		dts = append(dts, time.Duration(float64(d)*float64(num)/float64(den)*float64(time.Second)))
		size += n
		if len(f) == 6 {
			keyframes++
		}
	}
	if len(dts) < 3 {
		return Stats{}, false
	}
	// The first interval is skipped: over RTSP, ffmpeg's timestamps often
	// jump between the first two frames, before the RTCP sync arrives.
	deltas := make([]time.Duration, 0, len(dts)-1)
	for i := 2; i < len(dts); i++ {
		deltas = append(deltas, dts[i]-dts[i-1])
	}
	sorted := append([]time.Duration(nil), deltas...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	usual := sorted[len(sorted)/2]
	limit := max(3*usual, minGap)

	st := Stats{Frames: len(dts), Duration: dts[len(dts)-1] - dts[1]}
	for i, d := range deltas {
		if d > limit {
			if len(st.GapList) < maxGapList {
				st.GapList = append(st.GapList, Gap{At: dts[i+1] - dts[0], Length: d})
			}
			st.Gaps++
			st.Lost += d - usual
			st.MaxGap = max(st.MaxGap, d)
		}
	}
	if st.Duration > 0 {
		st.FPS = float64(len(deltas)) / st.Duration.Seconds()
		st.Kbps = int64(float64(size*8) / st.Duration.Seconds() / 1000)
	}
	if keyframes > 0 {
		st.KeyframeEvery = st.Duration / time.Duration(keyframes)
	}
	return st, true
}

// liveFrameMaxAge is how old the recording's preview frame may be before
// Snapshot falls back to opening the stream itself. Frames arrive once per
// keyframe, which some cameras send only every few seconds at night.
const liveFrameMaxAge = 30 * time.Second

// Snapshot grabs a single JPEG frame from the stream. While recording it
// returns the latest frame of the recording itself, because a second stream
// over the camera's Wi-Fi makes the recording drop frames.
func (r *Recorder) Snapshot(ctx context.Context) ([]byte, error) {
	url, _ := r.source()
	if url == "" {
		return nil, ErrNotConfigured
	}
	r.mu.Lock()
	frame, at := r.liveFrame, r.liveFrameAt
	r.mu.Unlock()
	if frame != nil && time.Since(at) < liveFrameMaxAge {
		return frame, nil
	}
	return SnapshotURL(ctx, r.ffmpeg, url)
}

// SnapshotURL grabs a single JPEG frame from any stream URL (used to test
// settings before saving them).
func SnapshotURL(ctx context.Context, ffmpeg, url string) ([]byte, error) {
	args := []string{"-hide_banner", "-nostats", "-loglevel", "error"}
	args = append(args, inputArgs(url)...)
	args = append(args, "-i", url, "-frames:v", "1")
	args = append(args, jpegArgs...)
	args = append(args, "-f", "image2", "pipe:1")
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	configureProcess(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			lines := strings.Split(msg, "\n")
			return nil, errors.New(strings.TrimSpace(lines[len(lines)-1]))
		}
		if ctx.Err() != nil {
			return nil, errors.New("a câmara não respondeu a tempo")
		}
		return nil, fmt.Errorf("ffmpeg: %v", err)
	}
	return out, nil
}

// recordArgs copies the stream to path and, from the same connection, writes
// a preview JPEG per keyframe to stdout as multipart (boundary "ffmpeg").
// -skip_frame only affects the decoder used for the preview; the copied
// stream keeps every frame. -fps_mode vfr drops a preview frame whose
// timestamp goes backwards (seen with Dahua DVR streams) instead of making
// the mpjpeg muxer log a warning.
func recordArgs(url, path string) []string {
	args := []string{"-hide_banner", "-nostats", "-loglevel", "error"}
	args = append(args, inputArgs(url)...)
	// aac_adtstoasc: some cameras (D-Link DCS-2530L) send AAC with ADTS
	// headers, which the MP4 muxer rejects on the first audio packet. The
	// filter leaves plain AAC untouched.
	args = append(args, "-skip_frame", "nokey", "-i", url,
		"-map", "0:v", "-map", "0:a?", "-c", "copy", "-bsf:a", "aac_adtstoasc",
		"-f", "mp4", "-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-y", path,
		"-map", "0:v:0", "-fps_mode", "vfr")
	args = append(args, jpegArgs...)
	return append(args, "-f", "mpjpeg", "pipe:1")
}

// jpegArgs encodes preview frames, for the snapshot and the live preview.
var jpegArgs = []string{"-vf", "scale='min(960,iw)':-2", "-c:v", "mjpeg", "-q:v", "5"}

func inputArgs(url string) []string {
	// ffmpeg needs a keyframe to learn the video size. Joining the stream
	// mid-GOP with the default 5 s probe fails with "dimensions not set"
	// when the camera's keyframe interval is longer (common at night). Probing
	// stops as soon as everything is known, so a bigger limit costs nothing.
	args := []string{"-analyzeduration", "20000000", "-probesize", "50000000"}
	if strings.HasPrefix(url, "rtsp://") || strings.HasPrefix(url, "rtsps://") {
		// -timeout (µs) makes ffmpeg exit when the camera stops sending.
		return append(args, "-rtsp_transport", "tcp", "-timeout", "10000000")
	}
	// Local files (tests): read at native speed like a live stream.
	return append(args, "-re")
}

func (r *Recorder) persist(recording bool) {
	if !recording {
		os.Remove(r.stateFile)
		return
	}
	since := r.startedAt
	if since.IsZero() {
		since = time.Now()
	}
	data, _ := json.Marshal(map[string]any{"recording": true, "since": since})
	if err := os.WriteFile(r.stateFile, data, 0o644); err != nil {
		r.log.Warn("could not persist recording state", "err", err)
	}
}

// uniquePath names files like the 1.x version did: 2006-01-02_150405.mp4.
func uniquePath(dir string, t time.Time) string {
	base := t.Format("2006-01-02_150405")
	path := filepath.Join(dir, base+".mp4")
	for i := 2; fileSize(path) >= 0; i++ {
		path = filepath.Join(dir, fmt.Sprintf("%s_%d.mp4", base, i))
	}
	return path
}

// fileSize returns -1 when the file does not exist.
func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}
