package recorder

// Live view, as HLS.
//
// While recording, the live view is read from the file that the recording
// ffmpeg is writing (fragmented MP4, see fmp4.go): there is no second
// connection to the camera, no extra ffmpeg and no change at all to how the
// recording runs. The file is only read, with a handle that never blocks the
// writer, the finalize remux or a delete (openShared).
//
// While idle, a separate ffmpeg (the "live ffmpeg") writes the stream to a
// temporary file in <recordings>/live the same way, only while someone is
// watching. It is stopped, and waited for, before a recording connects, so the
// camera never sends two streams to this PC at once.
//
// The playlist lists the last few fragments of the file; each fragment starts
// on a keyframe and is served as an HLS segment as it is (no re-encode).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/logging"
)

const (
	// LiveMaxViewers is how many pages may watch the live view at once. Each
	// viewer costs the camera's full bitrate in upload from the PC.
	LiveMaxViewers = 3
	// liveViewerTimeout frees a viewer's place once its page stops asking
	// for the playlist (closed, hidden, or stopped).
	liveViewerTimeout = 30 * time.Second
	// liveWindow is how many segments the playlist lists.
	liveWindow = 6
	// liveWaitFirst is how long a playlist request waits for the first
	// segment (the camera's first keyframe) before giving up.
	liveWaitFirst = 25 * time.Second
	// liveMaxAge restarts the live ffmpeg so its temporary file stays small.
	liveMaxAge = 20 * time.Minute
	// liveRetry is how long a failed live ffmpeg (camera unreachable) is not
	// retried, so open pages don't hammer the camera.
	liveRetry = 15 * time.Second
	// livePath is the URL prefix of the live view endpoints.
	livePath = "/api/live/"
)

var (
	ErrLiveUnavailable = errors.New("o direto ainda não está disponível, tente daqui a pouco")
	ErrLiveFull        = fmt.Errorf("já estão %d pessoas a ver o direto, tente mais tarde", LiveMaxViewers)
	ErrLiveGone        = errors.New("esta parte do direto já não existe")
)

// liveProc is a running live ffmpeg.
type liveProc struct {
	path     string
	used     atomic.Int64 // unix nanoseconds of the last request
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

type liveSegment struct {
	seq       int
	off, size int64
	dur       float64
}

// liveIndex is the playlist state, shared by every viewer.
type liveIndex struct {
	mu      sync.Mutex
	path    string // file the segments come from
	gen     int    // changes with path
	cc      int    // discontinuity sequence, also changes with path
	scan    *fragScanner
	init    []byte
	segs    []liveSegment
	seq     int // next media sequence number
	viewers map[string]time.Time
}

// LivePlaylist returns the HLS playlist of the live view. viewer is a
// random id per open page, used to count viewers. It starts the live ffmpeg
// when idle and waits for the first segment.
func (r *Recorder) LivePlaylist(ctx context.Context, viewer string) ([]byte, error) {
	if err := r.index.admit(viewer); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(liveWaitFirst)
	for {
		path, err := r.liveSource(true)
		if err != nil && !errors.Is(err, ErrLiveUnavailable) {
			return nil, err
		}
		if err == nil {
			if pl, err := r.index.playlist(path); err != nil {
				return nil, err
			} else if pl != nil {
				return pl, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, ErrLiveUnavailable
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// LiveInit returns the init section (ftyp + moov) of generation gen.
func (r *Recorder) LiveInit(gen int) ([]byte, error) {
	x := &r.index
	x.mu.Lock()
	defer x.mu.Unlock()
	if gen != x.gen || x.init == nil {
		return nil, ErrLiveGone
	}
	return x.init, nil
}

// LiveSegment returns segment seq of the playlist.
func (r *Recorder) LiveSegment(seq int) ([]byte, error) {
	x := &r.index
	x.mu.Lock()
	path := x.path
	var seg *liveSegment
	for i := range x.segs {
		if x.segs[i].seq == seq {
			s := x.segs[i]
			seg = &s
		}
	}
	x.mu.Unlock()
	if seg == nil {
		return nil, ErrLiveGone
	}
	// Never read a file that is no longer the live one: a finished recording
	// is replaced by its finalized copy, whose bytes are laid out differently.
	if cur, err := r.liveSource(false); err != nil || cur != path {
		return nil, ErrLiveGone
	}
	f, err := openShared(path)
	if err != nil {
		return nil, ErrLiveGone
	}
	defer f.Close()
	data := make([]byte, seg.size)
	if _, err := f.ReadAt(data, seg.off); err != nil {
		return nil, ErrLiveGone
	}
	return data, nil
}

// liveSource returns the file the live view reads: the recording being
// written, or the live ffmpeg's file while idle (started if start is set).
func (r *Recorder) liveSource(start bool) (string, error) {
	r.mu.Lock()
	state, file := r.state, r.file
	r.mu.Unlock()
	switch state {
	case StateRecording:
		if file != "" {
			_, dir := r.source()
			return filepath.Join(dir, file), nil
		}
	case StateIdle:
		if start {
			return r.ensureLive()
		}
		r.liveMu.Lock()
		p := r.liveP
		r.liveMu.Unlock()
		if p != nil {
			p.used.Store(time.Now().UnixNano())
			return p.path, nil
		}
	}
	return "", ErrLiveUnavailable
}

// ensureLive starts the live ffmpeg if it isn't running, and marks it used.
func (r *Recorder) ensureLive() (string, error) {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if p := r.liveP; p != nil {
		select {
		case <-p.done:
			r.liveP = nil
		default:
			p.used.Store(time.Now().UnixNano())
			return p.path, nil
		}
	}
	if r.liveErr != nil && time.Since(r.liveFailAt) < liveRetry {
		return "", r.liveErr
	}
	// Checked while holding liveMu: a recording that starts after this point
	// waits in stopLive (which needs liveMu) and then stops this ffmpeg.
	r.mu.Lock()
	idle := r.state == StateIdle
	r.mu.Unlock()
	if !idle {
		return "", ErrLiveUnavailable
	}
	url, dir := r.source()
	if url == "" {
		return "", ErrNotConfigured
	}
	liveDir := filepath.Join(dir, "live")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		return "", fmt.Errorf("não foi possível criar a pasta do direto: %w", err)
	}
	// Leftovers of a live ffmpeg that didn't stop cleanly (power cut…).
	if old, _ := filepath.Glob(filepath.Join(liveDir, "live-*.mp4")); len(old) > 0 {
		for _, f := range old {
			os.Remove(f)
		}
	}
	path := filepath.Join(liveDir, fmt.Sprintf("live-%d.mp4", time.Now().UnixNano()))
	// The same arguments as a recording: the same input options the camera
	// is known to work with, and preview frames for the snapshot, so the
	// snapshot doesn't open another connection while the live view runs.
	cmd := exec.Command(r.ffmpeg, recordArgs(url, path)...)
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	frames, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr := &logging.LineWriter{Log: r.log, Prefix: "ffmpeg (direto): "}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("não foi possível iniciar o ffmpeg (%s): %w", r.ffmpeg, err)
	}
	p := &liveProc{path: path, stop: make(chan struct{}), done: make(chan struct{})}
	p.used.Store(time.Now().UnixNano())
	r.liveP = p
	r.log.Info("live view started")
	go r.runLive(p, cmd, stdin, frames, stderr)
	return path, nil
}

// runLive watches the live ffmpeg and stops it once nobody has watched for
// liveIdle, when it gets too old, or on stopLive.
func (r *Recorder) runLive(p *liveProc, cmd *exec.Cmd, stdin io.WriteCloser, frames io.Reader, stderr *logging.LineWriter) {
	defer close(p.done)
	readDone := make(chan struct{})
	go func() { r.readFrames(frames); close(readDone) }()
	exited := make(chan error, 1)
	go func() { <-readDone; exited <- cmd.Wait() }()

	started := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var failure error
loop:
	for {
		select {
		case err := <-exited:
			if fileSize(p.path) <= 0 {
				msg := "não foi possível ligar à câmara"
				if line := stderr.LastLine(); line != "" {
					msg += ": " + line
				} else if err != nil {
					msg += ": " + err.Error()
				}
				failure = errors.New(msg)
			}
			break loop
		case <-p.stop:
			quit(cmd, stdin, exited)
			break loop
		case <-tick.C:
			unused := time.Since(time.Unix(0, p.used.Load())) > r.liveIdle
			if unused || time.Since(started) > liveMaxAge {
				quit(cmd, stdin, exited)
				break loop
			}
		}
	}
	os.Remove(p.path)

	r.liveMu.Lock()
	if r.liveP == p {
		r.liveP = nil
	}
	r.liveErr, r.liveFailAt = failure, time.Now()
	r.liveMu.Unlock()
	r.mu.Lock()
	if r.state == StateIdle {
		r.liveFrame, r.liveFrameAt = nil, time.Time{}
	}
	r.mu.Unlock()
	if failure != nil {
		r.log.Warn("live view failed", "err", failure)
	} else {
		r.log.Info("live view stopped")
	}
}

// stopLive stops the live ffmpeg, if any, and waits until it has exited and
// closed its connection to the camera.
func (r *Recorder) stopLive() {
	r.liveMu.Lock()
	p := r.liveP
	r.liveP = nil
	r.liveMu.Unlock()
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
}

// admit counts viewer in, unless the live view is full.
func (x *liveIndex) admit(viewer string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.viewers == nil {
		x.viewers = map[string]time.Time{}
	}
	now := time.Now()
	for id, seen := range x.viewers {
		if now.Sub(seen) > liveViewerTimeout {
			delete(x.viewers, id)
		}
	}
	if _, ok := x.viewers[viewer]; !ok && len(x.viewers) >= LiveMaxViewers {
		return ErrLiveFull
	}
	x.viewers[viewer] = now
	return nil
}

// playlist reads what is new in path and returns the playlist, or nil while
// there is no segment yet.
func (x *liveIndex) playlist(path string) ([]byte, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if path != x.path {
		// Another file (recording started or stopped, live ffmpeg restarted):
		// its segments are a new timeline.
		if x.path != "" {
			x.cc++
		}
		x.path, x.gen, x.scan, x.init, x.segs = path, x.gen+1, newFragScanner(), nil, nil
	}
	if err := x.refresh(); err != nil {
		return nil, err
	}
	if x.init == nil || len(x.segs) == 0 {
		return nil, nil
	}
	target := 1.0
	for _, s := range x.segs {
		target = max(target, s.dur)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n", int(target+0.999))
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", x.segs[0].seq, x.cc)
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%sinit/%d\"\n", livePath, x.gen)
	for _, s := range x.segs {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%sseg/%d\n", s.dur, livePath, s.seq)
	}
	return []byte(b.String()), nil
}

func (x *liveIndex) refresh() error {
	f, err := openShared(x.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // ffmpeg hasn't created it yet
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	frags, err := x.scan.scan(f, st.Size())
	if x.init == nil && x.scan.initEnd > 0 {
		init := make([]byte, x.scan.initEnd)
		if _, err := f.ReadAt(init, 0); err != nil {
			return err
		}
		x.init = init
	}
	for _, fr := range frags {
		x.segs = append(x.segs, liveSegment{seq: x.seq, off: fr.off, size: fr.size, dur: fr.dur})
		x.seq++
	}
	if n := len(x.segs); n > liveWindow {
		x.segs = append([]liveSegment(nil), x.segs[n-liveWindow:]...)
	}
	if err != nil {
		return fmt.Errorf("direto: %w", err)
	}
	return nil
}
