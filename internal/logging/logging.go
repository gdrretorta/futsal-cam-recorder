// Package logging sets up slog to write to a size-rotated file, stdout and an
// in-memory buffer of recent lines that the web UI can show.
package logging

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	maxFileSize = 5 << 20 // rotate after 5 MB
	keepLines   = 500
)

// Logger bundles the slog logger with the recent-lines buffer.
type Logger struct {
	*slog.Logger
	tail *tail
}

// New creates a logger writing to dir/recorder.log (rotated to
// recorder.log.1), to memory and to stdout.
func New(dir string) (*Logger, error) { return newLogger(dir, os.Stdout) }

func newLogger(dir string, stdout io.Writer) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Versions up to 2.15 logged ffmpeg's lines as they were, and those can
	// hold the camera URL with its password: scrub what is already on disk.
	for _, name := range []string{"recorder.log", "recorder.log.1"} {
		scrubFile(filepath.Join(dir, name))
	}
	file, err := newRotatingFile(filepath.Join(dir, "recorder.log"))
	if err != nil {
		return nil, err
	}
	t := &tail{}
	w := allWriters{file, t, stdout}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return &Logger{Logger: slog.New(h), tail: t}, nil
}

// allWriters writes to every writer even when one fails. io.MultiWriter
// stops at the first error, and a Windows service has no console, so
// writing to stdout fails there: the lines then never reached the
// in-memory tail and /api/logs was empty.
type allWriters []io.Writer

func (ws allWriters) Write(p []byte) (int, error) {
	for _, w := range ws {
		w.Write(p)
	}
	return len(p), nil
}

// urlUserinfo matches the credentials of a URL ("rtsp://user:pw@").
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)

// RedactURLs hides the credentials of every URL in s. ffmpeg prints the
// camera URL, password included, in its errors.
func RedactURLs(s string) string { return urlUserinfo.ReplaceAllString(s, "${1}***@") }

// scrubFile redacts the URLs in a log file written by an older version.
func scrubFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if clean := RedactURLs(string(data)); clean != string(data) {
		os.WriteFile(path, []byte(clean), 0o644)
	}
}

// Discard returns a logger that drops everything (for tests).
func Discard() *Logger {
	return &Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), tail: &tail{}}
}

// Recent returns the most recent log lines, oldest first.
func (l *Logger) Recent() []string { return l.tail.lines() }

type tail struct {
	mu  sync.Mutex
	buf []string
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		t.buf = append(t.buf, line)
	}
	if over := len(t.buf) - keepLines; over > 0 {
		t.buf = append(t.buf[:0:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tail) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string{}, t.buf...) // never nil: JSON [] rather than null
}

type rotatingFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func newRotatingFile(path string) (*rotatingFile, error) {
	r := &rotatingFile{path: path}
	return r, r.open()
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > maxFileSize {
		r.f.Close()
		os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// LineWriter adapts a logger to an io.Writer, logging each line written
// (used to forward ffmpeg's stderr).
type LineWriter struct {
	Log    *slog.Logger
	Prefix string
	mu     sync.Mutex
	buf    bytes.Buffer
	last   []string
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			w.buf.WriteString(line)
			break
		}
		if line = RedactURLs(strings.TrimSpace(line)); line != "" {
			w.Log.Warn(w.Prefix + line)
			w.last = append(w.last, line)
			if len(w.last) > 5 {
				w.last = w.last[1:]
			}
		}
	}
	return len(p), nil
}

// LastLine returns the most recent non-empty line, useful as an error message.
func (w *LineWriter) LastLine() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if rest := strings.TrimSpace(w.buf.String()); rest != "" {
		return RedactURLs(rest)
	}
	if len(w.last) == 0 {
		return ""
	}
	return w.last[len(w.last)-1]
}
