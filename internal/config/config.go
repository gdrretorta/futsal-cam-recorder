// Package config loads, validates and persists the application settings
// (config.json next to the executable).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FileName is the settings file, stored in the base directory.
const FileName = "config.json"

// Camera describes how to reach the RTSP stream.
type Camera struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Path     string `json:"path"`
	User     string `json:"user"`
	Password string `json:"password"`
	// URL, when set, is used as-is instead of host/port/path/user/password.
	// Meant for unusual cameras and for local testing.
	URL string `json:"url,omitempty"`
}

// Config is the full set of settings.
type Config struct {
	Title         string `json:"title"`
	Port          int    `json:"port"`
	Camera        Camera `json:"camera"`
	RecordingsDir string `json:"recordingsDir"`
	// RetentionDays deletes recordings older than this many days. 0 keeps everything.
	RetentionDays int  `json:"retentionDays"`
	AutoUpdate    bool `json:"autoUpdate"`
	// FFmpegPath overrides the ffmpeg binary. Empty means ffmpeg(.exe) next to
	// the executable, falling back to the one on PATH.
	FFmpegPath string `json:"ffmpegPath,omitempty"`
	// AdminPasswordHash is the single admin password of 2.x up to 2.6. It is
	// moved to an admin in Users when the file is loaded (see migrateUsers)
	// and stays empty afterwards.
	AdminPasswordHash string `json:"adminPasswordHash,omitempty"`
	// Notify sends emails to the admins (e.g. a recording running too long).
	Notify Notify `json:"notify"`
	// Tunnel keeps cloudflared (the Cloudflare Tunnel connector) up to date.
	Tunnel Tunnel `json:"tunnel"`
	// Users can log in to the web page. While there are none, the recorder
	// is open to everyone (as before 2.7) and nobody can reach the settings.
	Users []User `json:"users,omitempty"`
}

// Default returns the settings used when no config file exists.
func Default() Config {
	return Config{
		Title:         "Futsal Cam Recorder",
		Port:          8080,
		Camera:        Camera{Port: 554},
		RecordingsDir: "recordings",
		AutoUpdate:    true,
		Notify:        Notify{LongRecordingHours: 3, CameraDownMinutes: 5, LowDiskGB: 20},
		Tunnel:        Tunnel{AutoUpdate: true},
	}
}

// Tunnel configures the cloudflared updates (see internal/tunnel).
type Tunnel struct {
	// AutoUpdate updates cloudflared weekly (Monday 04:00–05:00), once
	// nothing is recording.
	AutoUpdate bool `json:"autoUpdate"`
	// ServiceName is the cloudflared Windows service. File only; default
	// "Cloudflared", the name `cloudflared service install` uses.
	ServiceName string `json:"serviceName,omitempty"`
}

// Notify configures the email alerts, sent through the Resend API.
type Notify struct {
	// ResendAPIKey is write-only in the API, like the camera password.
	ResendAPIKey string `json:"resendApiKey,omitempty"`
	// From is the sender, on a domain verified in Resend, e.g.
	// "Gravador <gravador@retorta.com>".
	From string `json:"from,omitempty"`
	// SiteURL is linked from the emails, e.g. https://futsal.retorta.com.
	SiteURL string `json:"siteUrl,omitempty"`
	// LongRecordingHours alerts the admins when a recording runs this long,
	// and again every hour after that. 0 turns the alert off.
	LongRecordingHours int `json:"longRecordingHours"`
	// CameraDownMinutes alerts the admins when, during a recording, the
	// camera has sent nothing for this many minutes (and again when it is
	// back). 0 turns the alert off.
	CameraDownMinutes int `json:"cameraDownMinutes"`
	// LowDiskGB alerts the admins when the free space of the recordings
	// volume drops below this many GB (then daily while it stays low). 0
	// turns the alert off.
	LowDiskGB int `json:"lowDiskGB"`
}

// Enabled reports whether emails can be sent.
func (n Notify) Enabled() bool { return n.ResendAPIKey != "" && n.From != "" }

// StreamURL builds the RTSP URL including credentials.
func (c Camera) StreamURL() string {
	if c.URL != "" {
		return c.URL
	}
	if c.Host == "" {
		return ""
	}
	// The path may carry a query, as DVRs need
	// (cam/realmonitor?channel=17&subtype=0); keep it unescaped.
	path, query, _ := strings.Cut(c.Path, "?")
	u := url.URL{
		Scheme:   "rtsp",
		Host:     net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:     "/" + strings.TrimPrefix(path, "/"),
		RawQuery: query,
	}
	if c.User != "" {
		u.User = url.UserPassword(c.User, c.Password)
	}
	return u.String()
}

// Configured reports whether there is enough information to connect.
func (c Camera) Configured() bool { return c.StreamURL() != "" }

// Redact removes credentials from a URL so it can be logged.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("***")
	return u.String()
}

// legacyTitle is the default title before the app was renamed.
const legacyTitle = "Simple RTSP Recorder"

// Validate checks the settings and fills in defaults for zero values.
func (c *Config) Validate() error {
	d := Default()
	// The app was called Simple RTSP Recorder until 2.8: a title left at
	// that default follows the new name.
	if strings.TrimSpace(c.Title) == "" || c.Title == legacyTitle {
		c.Title = d.Title
	}
	if c.Port == 0 {
		c.Port = d.Port
	}
	if c.Camera.Port == 0 {
		c.Camera.Port = d.Camera.Port
	}
	if strings.TrimSpace(c.RecordingsDir) == "" {
		c.RecordingsDir = d.RecordingsDir
	}
	c.Camera.Host = strings.TrimSpace(c.Camera.Host)
	c.Camera.Path = strings.TrimSpace(c.Camera.Path)
	switch {
	case c.Port < 1 || c.Port > 65535:
		return errors.New("porta HTTP inválida")
	case c.Camera.Port < 1 || c.Camera.Port > 65535:
		return errors.New("porta da câmara inválida")
	case c.RetentionDays < 0:
		return errors.New("dias de retenção não pode ser negativo")
	case strings.ContainsAny(c.Camera.Host, "/@ "):
		return errors.New("endereço da câmara inválido (indique só o IP ou nome)")
	case c.Notify.LongRecordingHours < 0 || c.Notify.LongRecordingHours > 48:
		return errors.New("o aviso de gravação longa tem de ser entre 0 e 48 horas")
	case c.Notify.CameraDownMinutes < 0 || c.Notify.CameraDownMinutes > 240:
		return errors.New("o aviso de câmara sem imagem tem de ser entre 0 e 240 minutos")
	case c.Notify.LowDiskGB < 0 || c.Notify.LowDiskGB > 100000:
		return errors.New("o aviso de pouco espaço em disco tem de ser entre 0 e 100000 GB")
	}
	c.Notify.From = strings.TrimSpace(c.Notify.From)
	c.Notify.SiteURL = strings.TrimRight(strings.TrimSpace(c.Notify.SiteURL), "/")
	c.Notify.ResendAPIKey = strings.TrimSpace(c.Notify.ResendAPIKey)
	if c.Notify.From != "" {
		if _, err := mail.ParseAddress(c.Notify.From); err != nil {
			return errors.New("remetente dos emails inválido (ex.: Gravador <gravador@retorta.com>)")
		}
	}
	if u := c.Notify.SiteURL; u != "" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return errors.New("endereço do site inválido (ex.: https://futsal.retorta.com)")
		}
	}
	return validateUsers(c.Users)
}

// Store gives concurrent access to the settings and persists changes. Edits
// made to config.json by other processes (e.g. set-admin-password while the
// service runs, or by hand) are picked up automatically.
type Store struct {
	editMu  sync.Mutex // serializes read-modify-write (see Edit)
	mu      sync.Mutex
	path    string
	baseDir string
	cfg     Config
	modTime time.Time
}

// Open loads config.json from baseDir. When it does not exist the settings are
// migrated from a legacy prod.env (or .env) if present, otherwise defaults are
// used, and the result is written to disk.
func Open(baseDir string) (*Store, error) {
	s := &Store{path: filepath.Join(baseDir, FileName), baseDir: baseDir}
	cfg, modTime, err := s.read()
	switch {
	case err == nil:
		s.cfg, s.modTime = cfg, modTime
		if migrateUsers(&cfg) {
			if err := s.save(cfg); err != nil {
				return nil, err
			}
			s.cfg = cfg
		}
		return s, nil
	case errors.Is(err, os.ErrNotExist):
		cfg, _, err := migrateLegacy(baseDir)
		if err != nil {
			return nil, err
		}
		s.cfg = cfg
		return s, s.save(cfg)
	default:
		return nil, err
	}
}

// Get returns a copy of the current settings.
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, err := os.Stat(s.path); err == nil && !st.ModTime().Equal(s.modTime) {
		// Changed on disk by someone else. Keep the old settings if the
		// new file is invalid (e.g. half-edited by hand).
		if cfg, modTime, err := s.read(); err == nil {
			s.cfg, s.modTime = cfg, modTime
			if migrateUsers(&cfg) && s.save(cfg) == nil {
				s.cfg = cfg
			}
		}
	}
	cfg := s.cfg
	cfg.Users = slices.Clone(cfg.Users) // callers may modify their copy
	return cfg
}

func (s *Store) read() (Config, time.Time, error) {
	cfg := Default()
	st, err := os.Stat(s.path)
	if err != nil {
		return cfg, time.Time{}, err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return cfg, time.Time{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, time.Time{}, fmt.Errorf("%s: %w", s.path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, time.Time{}, fmt.Errorf("%s: %w", s.path, err)
	}
	return cfg, st.ModTime(), nil
}

// Update validates and persists new settings.
func (s *Store) Update(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.save(cfg); err != nil {
		return err
	}
	s.cfg = cfg
	return nil
}

// Edit applies f to the current settings and saves the result, so that two
// concurrent changes (e.g. two users added at once) don't overwrite each other.
func (s *Store) Edit(f func(*Config) error) error {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	cfg := s.Get()
	if err := f(&cfg); err != nil {
		return err
	}
	return s.Update(cfg)
}

// BaseDir is the directory holding the executable, config and logs.
func (s *Store) BaseDir() string { return s.baseDir }

// RecordingsDir returns the absolute recordings directory.
func (s *Store) RecordingsDir() string {
	dir := s.Get().RecordingsDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(s.baseDir, dir)
	}
	return dir
}

func (s *Store) save(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if st, err := os.Stat(s.path); err == nil {
		s.modTime = st.ModTime()
	}
	return nil
}
