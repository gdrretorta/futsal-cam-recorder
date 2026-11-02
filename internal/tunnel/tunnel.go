// Package tunnel keeps cloudflared, the Cloudflare Tunnel connector running as
// a Windows service on the same PC, up to date. cloudflared doesn't update
// itself on Windows, and if the tunnel breaks nobody can reach the PC, so:
//
//  1. check that the public site answers through the tunnel (else touch nothing)
//  2. download the new cloudflared-windows-amd64.exe and verify its sha256
//  3. wait until nothing is recording (and block new recordings meanwhile)
//  4. stop the service, keep the old exe as .old, move the new one in, start it
//  5. check the public site again for up to 2 minutes; if it doesn't answer,
//     put the old exe back and start it again
//  6. email the admins the result
//
// It runs weekly (Monday, 04:00–05:00) when enabled, and on demand.
package tunnel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/config"
	"github.com/gdrretorta/futsal-cam-recorder/internal/updater"
)

// AssetName is the cloudflared release asset for Windows.
const AssetName = "cloudflared-windows-amd64.exe"

// LatestURL is the GitHub API endpoint of the latest cloudflared release
// (a variable for tests).
var LatestURL = "https://api.github.com/repos/cloudflare/cloudflared/releases/latest"

// Service controls the cloudflared Windows service.
type Service interface {
	// ExePath is the cloudflared executable the service runs.
	ExePath() (string, error)
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}

// ErrUnsupported is returned off Windows.
var ErrUnsupported = errors.New("só disponível no Windows")

// ErrBusy means an update is already running.
var ErrBusy = errors.New("já está a decorrer uma atualização do túnel")

// Status is served to the UI.
type Status struct {
	// State: idle, checking, downloading, waiting (for the recording to end),
	// updating, verifying, rollingBack, unsupported.
	State     string    `json:"state"`
	Installed string    `json:"installed,omitempty"`
	Latest    string    `json:"latest,omitempty"`
	LastCheck time.Time `json:"lastCheck,omitzero"`
	// Result of the last update attempt, shown as is.
	Message string `json:"message,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
}

// Updater updates cloudflared.
type Updater struct {
	Service Service
	Config  func() config.Config
	// AppVersion is this app's version: the health check expects the public
	// site to answer with it, which proves the request went through the tunnel
	// to this PC.
	AppVersion string
	// TryLock must succeed only when nothing is recording, and block new
	// recordings until Unlock.
	TryLock func() bool
	Unlock  func()
	// Notify emails the admins.
	Notify func(subject, text string)
	Log    *slog.Logger

	// For tests.
	Client        *http.Client
	ExeVersion    func(path string) (string, error)
	Now           func() time.Time
	WaitEvery     time.Duration // between attempts to lock the recorder
	CheckEvery    time.Duration // between health checks after the swap
	VerifyTimeout time.Duration // how long the new version has to bring the tunnel up

	once    sync.Once // fills the defaults
	mu      sync.Mutex
	ctx     context.Context // the app's, from Run: outlives the HTTP request
	status  Status
	running bool
	lastRun string // date of the last weekly run
}

func (u *Updater) defaults() { u.once.Do(u.fillDefaults) }

func (u *Updater) fillDefaults() {
	if u.Client == nil {
		u.Client = &http.Client{Timeout: 2 * time.Minute}
	}
	if u.ExeVersion == nil {
		u.ExeVersion = exeVersion
	}
	if u.Now == nil {
		u.Now = time.Now
	}
	if u.WaitEvery == 0 {
		u.WaitEvery = time.Minute
	}
	if u.CheckEvery == 0 {
		u.CheckEvery = 5 * time.Second
	}
	if u.VerifyTimeout == 0 {
		u.VerifyTimeout = 2 * time.Minute
	}
}

// Status returns the current state.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.status.State == "" {
		return Status{State: "idle"}
	}
	return u.status
}

func (u *Updater) set(f func(*Status)) {
	u.mu.Lock()
	f(&u.status)
	u.mu.Unlock()
}

func (u *Updater) setState(state string) { u.set(func(s *Status) { s.State = state }) }

// Refresh reads the installed version and the latest release, in the
// background; it is what the settings page shows.
func (u *Updater) Refresh(ctx context.Context) {
	u.defaults()
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return
	}
	u.mu.Unlock()
	installed, _, err := u.installed()
	if errors.Is(err, ErrUnsupported) {
		u.setState("unsupported")
		return
	}
	rel, rerr := u.latest(ctx)
	u.set(func(s *Status) {
		if s.State == "" || s.State == "unsupported" {
			s.State = "idle"
		}
		s.Installed, s.LastCheck = installed, u.Now()
		if rerr == nil {
			s.Latest = rel.Version
		}
		if err != nil && s.Message == "" {
			s.Message, s.Failed = err.Error(), true
		}
	})
}

// UpdateNow starts an update in the background (the "update now" button).
func (u *Updater) UpdateNow() error {
	u.defaults()
	u.mu.Lock()
	defer u.mu.Unlock()
	ctx := u.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := u.Service.ExePath(); errors.Is(err, ErrUnsupported) {
		return ErrUnsupported
	}
	if u.running {
		return ErrBusy
	}
	u.running = true
	go u.update(ctx, "manual")
	return nil
}

// Run refreshes the versions now and then, and updates weekly when enabled.
func (u *Updater) Run(ctx context.Context) {
	u.defaults()
	u.mu.Lock()
	u.ctx = ctx
	u.mu.Unlock()
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute): // let the app settle after a start
	}
	u.Refresh(ctx)
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if u.dueWeekly() {
			u.mu.Lock()
			start := !u.running // a manual update may be running already
			u.running = true
			u.mu.Unlock()
			if start {
				u.update(ctx, "weekly")
			}
		}
	}
}

// dueWeekly is true once on Mondays between 04:00 and 05:00 (local time).
func (u *Updater) dueWeekly() bool {
	now := u.Now()
	if !u.Config().Tunnel.AutoUpdate || now.Weekday() != time.Monday || now.Hour() != 4 {
		return false
	}
	day := now.Format("2006-01-02")
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.lastRun == day || u.status.State == "unsupported" {
		return false
	}
	u.lastRun = day
	return true
}

func (u *Updater) installed() (version, exe string, err error) {
	if exe, err = u.Service.ExePath(); err != nil {
		return "", "", err
	}
	version, err = u.ExeVersion(exe)
	return version, exe, err
}

// update runs the whole flow; u.running is already set.
func (u *Updater) update(ctx context.Context, why string) {
	defer u.set(func(s *Status) { s.State = "idle" })
	defer func() { u.mu.Lock(); u.running = false; u.mu.Unlock() }()
	u.Log.Info("cloudflared update started", "why", why)
	msg, failed := u.doUpdate(ctx)
	u.set(func(s *Status) { s.Message, s.Failed = msg, failed })
	if failed {
		u.Log.Warn("cloudflared update", "result", msg)
	} else {
		u.Log.Info("cloudflared update", "result", msg)
	}
}

func (u *Updater) doUpdate(ctx context.Context) (msg string, failed bool) {
	u.setState("checking")
	if u.Config().Notify.SiteURL == "" {
		return "indique o endereço público do site nas Definições: é com ele que se confirma que o túnel volta", true
	}
	installed, exe, err := u.installed()
	if err != nil {
		return "não foi possível encontrar o cloudflared: " + err.Error(), true
	}
	rel, err := u.latest(ctx)
	if err != nil {
		return "não foi possível ver a última versão do cloudflared: " + err.Error(), true
	}
	u.set(func(s *Status) { s.Installed, s.Latest, s.LastCheck = installed, rel.Version, u.Now() })
	if !updater.Newer(rel.Version, installed) {
		return fmt.Sprintf("o cloudflared já está na versão mais recente (%s)", installed), false
	}
	// If the tunnel is already down, or the address is wrong, a swap could
	// only make it worse, and the check after it would mean nothing.
	if err := u.healthy(ctx); err != nil {
		return "o site não responde pelo túnel, por isso nada foi alterado: " + err.Error(), true
	}

	u.setState("downloading")
	staged, err := u.download(ctx, rel, exe+".new")
	if err != nil {
		return "a transferência do cloudflared falhou: " + err.Error(), true
	}
	defer os.Remove(staged) // only left over on failure
	if v, err := u.ExeVersion(staged); err != nil || v != rel.Version {
		return fmt.Sprintf("o cloudflared transferido não arranca como esperado (versão %q, %v)", v, err), true
	}

	u.setState("waiting")
	for !u.TryLock() {
		select {
		case <-ctx.Done():
			return "atualização cancelada", true
		case <-time.After(u.WaitEvery):
		}
	}
	defer u.Unlock()

	u.setState("updating")
	old := exe + ".old"
	if err := u.swap(ctx, exe, staged, old); err != nil {
		return u.rollback(ctx, exe, old, installed, rel.Version, "a troca falhou: "+err.Error())
	}
	u.setState("verifying")
	if err := u.waitHealthy(ctx); err != nil {
		return u.rollback(ctx, exe, old, installed, rel.Version, "o túnel não voltou: "+err.Error())
	}
	msg = fmt.Sprintf("o cloudflared foi atualizado da versão %s para a %s e o túnel está a funcionar", installed, rel.Version)
	u.set(func(s *Status) { s.Installed = rel.Version })
	u.notify("cloudflared atualizado", msg)
	return msg, false
}

func (u *Updater) swap(ctx context.Context, exe, staged, old string) error {
	stopCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := u.Service.Stop(stopCtx); err != nil {
		return fmt.Errorf("parar o serviço: %w", err)
	}
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(staged, exe); err != nil {
		return err
	}
	startCtx, cancel2 := context.WithTimeout(ctx, time.Minute)
	defer cancel2()
	return u.Service.Start(startCtx)
}

// rollback puts the previous cloudflared back after a failed swap.
func (u *Updater) rollback(ctx context.Context, exe, old, installed, latest, why string) (string, bool) {
	u.setState("rollingBack")
	u.Log.Warn("cloudflared update failed, rolling back", "reason", why)
	stopCtx, cancel := context.WithTimeout(ctx, time.Minute)
	u.Service.Stop(stopCtx) // may already be stopped
	cancel()
	if _, err := os.Stat(old); err == nil {
		os.Remove(exe)
		if err := os.Rename(old, exe); err != nil {
			msg := fmt.Sprintf("a atualização para a versão %s falhou (%s) e não foi possível repor a versão %s: %v. O túnel pode estar em baixo.", latest, why, installed, err)
			u.notify("falha grave ao atualizar o cloudflared", msg)
			return msg, true
		}
	}
	startCtx, cancel2 := context.WithTimeout(ctx, time.Minute)
	err := u.Service.Start(startCtx)
	cancel2()
	if err == nil {
		err = u.waitHealthy(ctx)
	}
	if err != nil {
		msg := fmt.Sprintf("a atualização para a versão %s falhou (%s); a versão %s foi reposta, mas o túnel continua sem responder: %v", latest, why, installed, err)
		u.notify("falha grave ao atualizar o cloudflared", msg)
		return msg, true
	}
	msg := fmt.Sprintf("a atualização para a versão %s falhou (%s); a versão %s foi reposta e o túnel está a funcionar", latest, why, installed)
	u.notify("atualização do cloudflared revertida", msg)
	return msg, true
}

func (u *Updater) notify(subject, text string) {
	if u.Notify != nil {
		u.Notify(subject, strings.ToUpper(text[:1])+text[1:]+".")
	}
}

// healthy checks that the public site answers with this app's version, which
// only happens when the request crossed Cloudflare and the tunnel to this PC.
func (u *Updater) healthy(ctx context.Context) error {
	nonce := make([]byte, 6)
	rand.Read(nonce)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := u.Config().Notify.SiteURL + "/api/status?tunnel-check=" + hex.EncodeToString(nonce)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := u.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("o site respondeu %s", resp.Status)
	}
	var st struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st); err != nil || st.Version != u.AppVersion {
		return fmt.Errorf("o site respondeu, mas não é este gravador (versão %q)", st.Version)
	}
	return nil
}

func (u *Updater) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(u.VerifyTimeout)
	for {
		err := u.healthy(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(u.CheckEvery):
		}
	}
}

type release struct {
	Version string
	URL     string
	SHA256  string
}

// sumInBody finds "cloudflared-windows-amd64.exe: <sha256>" in the release
// notes, for releases whose assets have no digest.
var sumInBody = regexp.MustCompile(regexp.QuoteMeta(AssetName) + `:?\s+([0-9a-fA-F]{64})`)

func (u *Updater) latest(ctx context.Context) (release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LatestURL, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.Client.Do(req)
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("GitHub respondeu %s", resp.Status)
	}
	var r struct {
		Tag    string `json:"tag_name"`
		Body   string `json:"body"`
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"` // "sha256:…"
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return release{}, err
	}
	rel := release{Version: strings.TrimPrefix(r.Tag, "v")}
	for _, a := range r.Assets {
		if a.Name == AssetName {
			rel.URL = a.URL
			rel.SHA256 = strings.ToLower(strings.TrimPrefix(a.Digest, "sha256:"))
		}
	}
	if rel.SHA256 == "" {
		if m := sumInBody.FindStringSubmatch(r.Body); m != nil {
			rel.SHA256 = strings.ToLower(m[1])
		}
	}
	if rel.URL == "" {
		return release{}, fmt.Errorf("a versão %s não tem o ficheiro %s", rel.Version, AssetName)
	}
	return rel, nil
}

// download saves the release next to the current exe and checks its sha256.
// Without a published checksum nothing is installed.
func (u *Updater) download(ctx context.Context, rel release, path string) (string, error) {
	if len(rel.SHA256) != 64 {
		return "", errors.New("a versão não publica o checksum sha256, por isso não é instalada")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s", resp.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != rel.SHA256 {
		err = errors.New("checksum não corresponde")
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// exeVersion runs "cloudflared --version" ("cloudflared version 2025.8.1 (built …)").
func exeVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	v := versionRe.FindString(string(out))
	if v == "" {
		return "", fmt.Errorf("versão desconhecida: %q", strings.TrimSpace(string(out)))
	}
	return v, nil
}

// ExeFromCommandLine takes the executable out of a service's command line
// (which for cloudflared holds the tunnel token: never log it).
func ExeFromCommandLine(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			return cmd[1 : end+1]
		}
		return strings.Trim(cmd, `"`)
	}
	if i := strings.Index(strings.ToLower(cmd), ".exe"); i >= 0 {
		return cmd[:i+4]
	}
	path, _, _ := strings.Cut(cmd, " ")
	return path
}
