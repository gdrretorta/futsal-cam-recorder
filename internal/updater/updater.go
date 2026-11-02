// Package updater keeps the executable up to date from GitHub releases.
//
// Flow: check the latest release → download the exe asset and verify it
// against checksums.txt → wait until nothing is recording → swap the files
// (current exe renamed to .old, new one moved in place) → ask the app to
// restart. As a Windows service the restart is done by the Service Control
// Manager's recovery actions (configured by `install`).
package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// AssetName is the release asset holding the bare executable.
	AssetName     = "futsal-cam-recorder.exe"
	checksumsName = "checksums.txt"
	checkInterval = time.Hour
	applyInterval = 30 * time.Second
)

// Status is served to the UI.
type Status struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	State     string    `json:"state"` // idle, checking, downloading, waiting, applying, error, disabled
	Error     string    `json:"error,omitempty"`
	LastCheck time.Time `json:"lastCheck,omitzero"`
}

// Options configures an Updater.
type Options struct {
	Repo    string // owner/name
	Current string // running version, e.g. v2.0.0 or "dev"
	ExePath string // executable to replace
	Log     *slog.Logger
	// Enabled reports whether auto-update is on (read from settings each time).
	Enabled func() bool
	// TryLock must return true only when no recording is running; it must
	// also block new recordings until Unlock.
	TryLock func() bool
	Unlock  func()
	// Restart is called after the new binary is in place.
	Restart func()
	// APIBase overrides https://api.github.com (tests).
	APIBase string
	Client  *http.Client
}

// Updater checks for and applies updates.
type Updater struct {
	opt   Options
	check chan struct{}

	mu     sync.Mutex
	status Status
	staged string // downloaded binary waiting to be applied
}

// New creates an updater.
func New(opt Options) *Updater {
	if opt.APIBase == "" {
		opt.APIBase = "https://api.github.com"
	}
	if opt.Client == nil {
		opt.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Updater{opt: opt, check: make(chan struct{}, 1), status: Status{Current: opt.Current, State: "idle"}}
}

// Status returns the current update status.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	if !u.opt.Enabled() && s.State == "idle" {
		s.State = "disabled"
	}
	return s
}

// CheckNow triggers a check (and apply, when idle) as soon as possible,
// even when auto-update is disabled.
func (u *Updater) CheckNow() {
	select {
	case u.check <- struct{}{}:
	default:
	}
}

// CleanupOld removes the previous binary left behind by an update.
func (u *Updater) CleanupOld() {
	os.Remove(u.opt.ExePath + ".old")
	os.Remove(u.opt.ExePath + ".new")
}

// Run checks periodically until ctx is cancelled.
func (u *Updater) Run(ctx context.Context) {
	first := time.NewTimer(time.Minute)
	defer first.Stop()
	checkTick := time.NewTicker(checkInterval)
	defer checkTick.Stop()
	applyTick := time.NewTicker(applyInterval)
	defer applyTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			u.auto(ctx)
		case <-checkTick.C:
			u.auto(ctx)
		case <-u.check:
			u.checkAndStage(ctx)
			u.tryApply()
		case <-applyTick.C:
			u.tryApply()
		}
	}
}

func (u *Updater) auto(ctx context.Context) {
	if !u.opt.Enabled() {
		return
	}
	u.checkAndStage(ctx)
	u.tryApply()
}

func (u *Updater) setState(state, errMsg string) {
	u.mu.Lock()
	u.status.State, u.status.Error = state, errMsg
	u.mu.Unlock()
}

// checkAndStage downloads a newer release, if any.
func (u *Updater) checkAndStage(ctx context.Context) {
	if u.opt.Current == "dev" {
		u.setState("error", "versão de desenvolvimento: atualizações automáticas desligadas")
		return
	}
	u.mu.Lock()
	staged := u.staged
	u.mu.Unlock()
	if staged != "" {
		return
	}
	u.setState("checking", "")
	rel, err := u.latest(ctx)
	u.mu.Lock()
	u.status.LastCheck = time.Now()
	if err == nil {
		u.status.Latest = rel.TagName
	}
	u.mu.Unlock()
	if err != nil {
		u.opt.Log.Warn("update check failed", "err", err)
		u.setState("error", "não foi possível verificar atualizações: "+err.Error())
		return
	}
	if !Newer(rel.TagName, u.opt.Current) {
		u.setState("idle", "")
		return
	}
	exeURL, sumURL := rel.asset(AssetName), rel.asset(checksumsName)
	if exeURL == "" || sumURL == "" {
		u.setState("idle", "")
		u.opt.Log.Warn("release has no windows executable, skipping", "release", rel.TagName)
		return
	}
	u.opt.Log.Info("downloading update", "version", rel.TagName)
	u.setState("downloading", "")
	path, err := u.download(ctx, exeURL, sumURL)
	if err != nil {
		u.opt.Log.Error("update download failed", "err", err)
		u.setState("error", "falha ao descarregar atualização: "+err.Error())
		return
	}
	u.mu.Lock()
	u.staged = path
	u.mu.Unlock()
	u.setState("waiting", "")
}

// tryApply swaps the binary when nothing is recording.
func (u *Updater) tryApply() {
	u.mu.Lock()
	staged := u.staged
	u.mu.Unlock()
	if staged == "" || !u.opt.TryLock() {
		return
	}
	u.setState("applying", "")
	exe, old := u.opt.ExePath, u.opt.ExePath+".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		u.fail(staged, fmt.Errorf("rename current exe: %w", err))
		return
	}
	if err := os.Rename(staged, exe); err != nil {
		os.Rename(old, exe) // roll back
		u.fail(staged, fmt.Errorf("move new exe: %w", err))
		return
	}
	u.opt.Log.Info("update installed, restarting", "version", u.Status().Latest)
	// Keep the recorder locked: the process is about to exit.
	u.opt.Restart()
}

func (u *Updater) fail(staged string, err error) {
	u.opt.Unlock()
	os.Remove(staged)
	u.mu.Lock()
	u.staged = ""
	u.mu.Unlock()
	u.opt.Log.Error("update failed", "err", err)
	u.setState("error", "falha ao instalar atualização: "+err.Error())
}

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r release) asset(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

func (u *Updater) latest(ctx context.Context) (release, error) {
	var rel release
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.opt.APIBase+"/repos/"+u.opt.Repo+"/releases/latest", nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.opt.Client.Do(req)
	if err != nil {
		return rel, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return rel, fmt.Errorf("GitHub respondeu %s", resp.Status)
	}
	return rel, json.NewDecoder(resp.Body).Decode(&rel)
}

// download fetches the executable next to the current one and checks its hash.
func (u *Updater) download(ctx context.Context, exeURL, sumURL string) (string, error) {
	want, err := u.expectedHash(ctx, sumURL)
	if err != nil {
		return "", err
	}
	body, err := u.get(ctx, exeURL)
	if err != nil {
		return "", err
	}
	defer body.Close()
	path := u.opt.ExePath + ".new"
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = errors.New("checksum não corresponde")
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func (u *Updater) expectedHash(ctx context.Context, sumURL string) (string, error) {
	body, err := u.get(ctx, sumURL)
	if err != nil {
		return "", err
	}
	defer body.Close()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == AssetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s não está em %s", AssetName, checksumsName)
}

func (u *Updater) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.opt.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// Newer reports whether version a (e.g. v2.1.0) is newer than b.
// Non-semver versions are never newer.
func Newer(a, b string) bool {
	pa, okA := parse(a)
	pb, okB := parse(b)
	if !okA || !okB {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "-") // ignore pre-release suffix
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
