package recorder

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/sysinfo"
)

var (
	// ErrCutBusy allows one cut at a time, so the disk stays free for the
	// recording.
	ErrCutBusy = errors.New("já está a ser feito um corte, espere que termine")
	// ErrCutRange is a start/end that makes no excerpt.
	ErrCutRange = errors.New("o fim do excerto tem de ser depois do início")
)

// cutMinFree is the space that must stay free on the disk, on top of the
// excerpt, so a cut never fills the disk under the recording.
const cutMinFree = 2 << 30

// Cutting returns the recording being cut, if any.
func (r *Recorder) Cutting() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cutSrc
}

// Cut copies the part of recording name between from and to into a new
// recording, named after the moment the excerpt starts (so the page shows
// its real date and time). With replace, the original is deleted once the
// excerpt is ready (e.g. a recording someone forgot to stop), and an excerpt
// from the start takes the original's name. It runs in
// the background and returns the new file's name at once; the excerpt shows
// as "processing" until it is ready.
//
// The copy is a separate ffmpeg reading the file (-c copy, no re-encode),
// never the camera, so it can't disturb a recording. Without re-encoding the
// excerpt starts at the keyframe just before from (at most a couple of
// seconds earlier).
func (r *Recorder) Cut(name string, from, to time.Duration, replace bool) (string, error) {
	if !ValidName(name) {
		return "", errors.New("nome de ficheiro inválido")
	}
	if from < 0 || to-from < time.Second {
		return "", ErrCutRange
	}
	_, dir := r.source()
	src := filepath.Join(dir, name)
	st, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	if replace && r.IsProtected(name) {
		return "", ErrProtected
	}
	if free, _, err := sysinfo.Disk(dir); err == nil && free < uint64(st.Size())+cutMinFree {
		return "", errors.New("não há espaço livre suficiente no disco para o excerto")
	}

	r.mu.Lock()
	switch {
	case r.cutSrc != "":
		r.mu.Unlock()
		return "", ErrCutBusy
	case r.finalizing[name] || (r.state != StateIdle && r.file == name):
		r.mu.Unlock()
		return "", errors.New("esta gravação ainda está a ser processada")
	}
	start := nameTime(name, st.ModTime()).Add(from)
	out := uniquePath(dir, start)
	outName := filepath.Base(out)
	// Reserve the name: the excerpt shows as processing, and the updater
	// waits for it (TryLock).
	r.cutSrc, r.finalizing[outName] = name, true
	r.mu.Unlock()

	r.bg.Add(1)
	go func() {
		defer r.bg.Done()
		defer func() {
			r.mu.Lock()
			r.cutSrc = ""
			delete(r.finalizing, outName)
			r.mu.Unlock()
		}()
		if err := r.cut(src, out, from, to); err != nil {
			r.log.Warn("cut failed", "file", name, "err", err)
			return
		}
		r.log.Info("excerpt saved", "from", name, "file", outName, "start", from.String(), "end", to.String(), "replace", replace)
		if replace {
			if err := removeRetry(src); err != nil {
				r.log.Warn("could not delete the original after the cut", "file", name, "err", err)
				return
			}
			r.log.Info("original deleted after the cut", "file", name)
			// An excerpt from the very start took a "_2" name because the
			// original still existed: it can have the original's name now.
			if better := filepath.Join(dir, start.Format("2006-01-02_150405")+".mp4"); better != out && fileSize(better) < 0 && os.Rename(out, better) == nil {
				r.log.Info("excerpt renamed", "file", filepath.Base(better))
			}
		}
	}()
	return outName, nil
}

func (r *Recorder) cut(src, out string, from, to time.Duration) error {
	tmp := out + ".tmp" // removed by CleanupTemp if the app stops halfway
	cmd := exec.CommandContext(r.bgCtx, r.ffmpeg, cutArgs(src, tmp, from, to)...)
	configureProcess(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if fileSize(tmp) <= 0 {
		os.Remove(tmp)
		return errors.New("o excerto ficou vazio")
	}
	var err error
	for try := 0; try < 5; try++ {
		if err = os.Rename(tmp, out); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	os.Remove(tmp)
	return err
}

func cutArgs(src, out string, from, to time.Duration) []string {
	secs := func(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 3, 64) }
	return []string{"-hide_banner", "-nostats", "-loglevel", "error",
		"-ss", secs(from), "-i", src, "-t", secs(to - from),
		"-map", "0", "-c", "copy", "-avoid_negative_ts", "make_zero",
		"-movflags", "+faststart", "-f", "mp4", "-y", out}
}

// nameTime is the start time in a recording's name (2006-01-02_150405…),
// else the file's modification time.
func nameTime(name string, mod time.Time) time.Time {
	if len(name) >= 17 {
		if t, err := time.ParseInLocation("2006-01-02_150405", name[:17], time.Local); err == nil {
			return t
		}
	}
	return mod
}

// removeRetry deletes a file, retrying for a while: on Windows a player that
// has it open makes the delete fail.
func removeRetry(path string) error {
	var err error
	for try := 0; try < 10; try++ {
		if err = os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return err
}
