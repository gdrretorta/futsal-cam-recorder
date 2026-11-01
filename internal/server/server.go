// Package server exposes the web UI and its JSON API.
//
// Access: [user] any logged-in user, [users] admins and user managers,
// [admin] admins only. While no user exists, [user] endpoints are open to
// everyone (see accessRecorder). A user whose password was chosen by someone
// else can only use /api/me, /api/me/password and /api/logout until they
// change it (403 otherwise). /recordings/{name} is always public, so a
// recording's link can be shared.
//
//	GET    /api/status               recorder, update and disk status; only
//	                                 {title, version, loginRequired} without access
//	POST   /api/recording/start      start recording                          [user]
//	POST   /api/recording/stop       stop recording                           [user]
//	GET    /api/recordings           list recordings                          [user]
//	DELETE /api/recordings/{name}    delete a recording                       [user]
//	GET    /recordings/{name}        play (range requests) or ?download=1     public
//	GET    /api/snapshot             JPEG frame from the camera (cached)      [user]
//	GET    /api/me                   {authRequired, user}; renews the session
//	POST   /api/login                {email, password} → session cookie
//	POST   /api/logout               end the session
//	PUT    /api/me/password          {currentPassword, password}              [account]
//	GET    /api/users                list users                               [users]
//	POST   /api/users                {email, role, password} create a user    [users]
//	PUT    /api/users/{id}           {email?, role?, password?} change a user [users]
//	DELETE /api/users/{id}           delete a user                            [users]
//	GET    /api/settings             settings (password never returned)       [admin]
//	PUT    /api/settings             save settings                            [admin]
//	POST   /api/settings/test        test camera settings, returns a JPEG     [admin]
//	POST   /api/settings/notify-test send a test email to the admins          [admin]
//	POST   /api/update/check         check for updates now                    [admin]
//	GET    /api/logs                 recent log lines                         [admin]
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/joaoopereira/simple-rtsp-recorder/internal/config"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/logging"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/notify"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/recorder"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/sysinfo"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/updater"
)

const snapshotTTL = 5 * time.Second

// Server holds the dependencies of the HTTP handlers.
type Server struct {
	Version  string
	FFmpeg   string
	Store    *config.Store
	Recorder *recorder.Recorder
	Updater  *updater.Updater
	Log      *logging.Logger
	Static   fs.FS

	auth *auth

	snapMu   sync.Mutex
	snapAt   time.Time
	snapData []byte
	snapErr  error
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	var err error
	if s.auth, err = newAuth(s.Store.BaseDir()); err != nil {
		s.Log.Warn("could not save the session key; sessions will end on restart", "err", err)
	}
	user, users, admin := func(h http.HandlerFunc) http.HandlerFunc { return s.require(accessRecorder, h) },
		func(h http.HandlerFunc) http.HandlerFunc { return s.require(accessUsers, h) },
		func(h http.HandlerFunc) http.HandlerFunc { return s.require(accessAdmin, h) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/recording/start", user(s.start))
	mux.HandleFunc("POST /api/recording/stop", user(s.stop))
	mux.HandleFunc("GET /api/recordings", user(s.list))
	mux.HandleFunc("DELETE /api/recordings/{name}", user(s.remove))
	mux.HandleFunc("GET /recordings/{name}", s.serveRecording)
	mux.HandleFunc("GET /api/snapshot", user(s.snapshot))
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("PUT /api/me/password", s.require(accessAccount, s.changeOwnPassword))
	mux.HandleFunc("GET /api/users", users(s.listUsers))
	mux.HandleFunc("POST /api/users", users(s.createUser))
	mux.HandleFunc("PUT /api/users/{id}", users(s.updateUser))
	mux.HandleFunc("DELETE /api/users/{id}", users(s.deleteUser))
	mux.HandleFunc("GET /api/settings", admin(s.getSettings))
	mux.HandleFunc("PUT /api/settings", admin(s.putSettings))
	mux.HandleFunc("POST /api/settings/test", admin(s.testSettings))
	mux.HandleFunc("POST /api/settings/notify-test", admin(s.testNotify))
	mux.HandleFunc("POST /api/update/check", admin(s.checkUpdate))
	mux.HandleFunc("GET /api/logs", admin(s.logs))
	mux.Handle("GET /", newStaticHandler(s.Static))
	return mux
}

type disk struct {
	Free  uint64 `json:"free"`
	Total uint64 `json:"total"`
}

// status is public, so that a page left open across an update still sees the
// new version and reloads (an error would only show "Sem ligação"). Without
// access it reveals nothing but the title and version.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	cfg := s.Store.Get()
	if u, ok := s.auth.user(r, s.Store); (!ok || u.MustChangePassword) && len(cfg.Users) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"title": cfg.Title, "version": s.Version, "loginRequired": true})
		return
	}
	resp := struct {
		Title      string          `json:"title"`
		Version    string          `json:"version"`
		Configured bool            `json:"configured"`
		Recorder   recorder.Status `json:"recorder"`
		Update     updater.Status  `json:"update"`
		Disk       *disk           `json:"disk,omitempty"`
	}{
		Title:      cfg.Title,
		Version:    s.Version,
		Configured: cfg.Camera.Configured(),
		Recorder:   s.Recorder.Status(),
		Update:     s.Updater.Status(),
	}
	if free, total, err := sysinfo.Disk(existingParent(s.Store.RecordingsDir())); err == nil {
		resp.Disk = &disk{free, total}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	if err := s.Recorder.Start(); err != nil {
		code := http.StatusConflict
		if errors.Is(err, recorder.ErrNotConfigured) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err)
		return
	}
	s.Log.Info("recording requested from UI", "remote", r.RemoteAddr)
	writeJSON(w, http.StatusAccepted, s.Recorder.Status())
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if err := s.Recorder.Stop(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.Log.Info("stop requested from UI", "remote", r.RemoteAddr)
	writeJSON(w, http.StatusAccepted, s.Recorder.Status())
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	list, err := s.Recorder.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// recordingPath validates the {name} parameter and resolves it.
func (s *Server) recordingPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !recorder.ValidName(name) {
		writeError(w, http.StatusBadRequest, errors.New("nome de ficheiro inválido"))
		return "", false
	}
	if s.Recorder.IsActive(name) {
		writeError(w, http.StatusConflict, errors.New("esta gravação ainda está a ser processada"))
		return "", false
	}
	return filepath.Join(s.Store.RecordingsDir(), name), true
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	path, ok := s.recordingPath(w, r)
	if !ok {
		return
	}
	if err := os.Remove(path); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		writeError(w, code, err)
		return
	}
	s.Log.Info("recording deleted", "file", filepath.Base(path), "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveRecording(w http.ResponseWriter, r *http.Request) {
	path, ok := s.recordingPath(w, r)
	if !ok {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+st.Name()+`"`)
	}
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	// One ffmpeg at a time; everyone else shares the cached frame.
	s.snapMu.Lock()
	if time.Since(s.snapAt) > snapshotTTL {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		s.snapData, s.snapErr = s.Recorder.Snapshot(ctx)
		cancel()
		s.snapAt = time.Now()
	}
	data, err := s.snapData, s.snapErr
	s.snapMu.Unlock()
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// settingsDTO is the wire format of the settings. The camera password is
// write-only: GET reports hasPassword, PUT keeps the stored one when
// password is null/absent.
type settingsDTO struct {
	Title  string `json:"title"`
	Camera struct {
		Host        string  `json:"host"`
		Port        int     `json:"port"`
		Path        string  `json:"path"`
		User        string  `json:"user"`
		Password    *string `json:"password,omitempty"`
		HasPassword bool    `json:"hasPassword"`
	} `json:"camera"`
	RecordingsDir string `json:"recordingsDir"`
	RetentionDays int    `json:"retentionDays"`
	AutoUpdate    bool   `json:"autoUpdate"`
	// The Resend API key is write-only, like the camera password.
	Notify struct {
		From               string  `json:"from"`
		SiteURL            string  `json:"siteUrl"`
		LongRecordingHours int     `json:"longRecordingHours"`
		APIKey             *string `json:"apiKey,omitempty"`
		HasAPIKey          bool    `json:"hasApiKey"`
	} `json:"notify"`
}

func toDTO(c config.Config) settingsDTO {
	var d settingsDTO
	d.Title = c.Title
	d.Camera.Host, d.Camera.Port, d.Camera.Path, d.Camera.User = c.Camera.Host, c.Camera.Port, c.Camera.Path, c.Camera.User
	d.Camera.HasPassword = c.Camera.Password != ""
	d.RecordingsDir, d.RetentionDays, d.AutoUpdate = c.RecordingsDir, c.RetentionDays, c.AutoUpdate
	d.Notify.From, d.Notify.SiteURL, d.Notify.LongRecordingHours = c.Notify.From, c.Notify.SiteURL, c.Notify.LongRecordingHours
	d.Notify.HasAPIKey = c.Notify.ResendAPIKey != ""
	return d
}

// apply merges the DTO into the current settings. Fields only editable in
// config.json (port, ffmpegPath, camera.url) and the users are kept.
func (d settingsDTO) apply(c config.Config) config.Config {
	c.Title = d.Title
	pw := c.Camera.Password
	if d.Camera.Password != nil {
		pw = *d.Camera.Password
	}
	c.Camera = config.Camera{Host: d.Camera.Host, Port: d.Camera.Port, Path: d.Camera.Path, User: d.Camera.User, Password: pw, URL: c.Camera.URL}
	c.RecordingsDir, c.RetentionDays, c.AutoUpdate = d.RecordingsDir, d.RetentionDays, d.AutoUpdate
	key := c.Notify.ResendAPIKey
	if d.Notify.APIKey != nil {
		key = *d.Notify.APIKey
	}
	c.Notify = config.Notify{ResendAPIKey: key, From: d.Notify.From, SiteURL: d.Notify.SiteURL, LongRecordingHours: d.Notify.LongRecordingHours}
	return c
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, toDTO(s.Store.Get()))
}

func (s *Server) readSettings(w http.ResponseWriter, r *http.Request) (settingsDTO, config.Config, bool) {
	var d settingsDTO
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("pedido inválido"))
		return d, config.Config{}, false
	}
	cfg := d.apply(s.Store.Get())
	if err := cfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return d, cfg, false
	}
	return d, cfg, true
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.readSettings(w, r)
	if !ok {
		return
	}
	// Applied inside Edit so that users changed meanwhile are kept.
	var cfg config.Config
	if err := s.Store.Edit(func(c *config.Config) error {
		*c = d.apply(*c)
		cfg = *c
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.MkdirAll(s.Store.RecordingsDir(), 0o755); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("definições guardadas, mas não foi possível criar a pasta de gravações: "+err.Error()))
		return
	}
	s.snapMu.Lock()
	s.snapAt = time.Time{} // camera may have changed
	s.snapMu.Unlock()
	s.Log.Info("settings updated", "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, toDTO(cfg))
}

func (s *Server) testSettings(w http.ResponseWriter, r *http.Request) {
	_, cfg, ok := s.readSettings(w, r)
	if !ok {
		return
	}
	url := cfg.Camera.StreamURL()
	if url == "" {
		writeError(w, http.StatusBadRequest, recorder.ErrNotConfigured)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	img, err := recorder.SnapshotURL(ctx, s.FFmpeg, url)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(img)
}

// testNotify sends a test email to the admins with the settings in the form,
// saved or not.
func (s *Server) testNotify(w http.ResponseWriter, r *http.Request) {
	_, cfg, ok := s.readSettings(w, r)
	if !ok {
		return
	}
	email := notify.TestEmail(cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := notify.Send(ctx, cfg.Notify, email); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, notify.ErrNotConfigured) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err)
		return
	}
	s.Log.Info("test email sent", "to", strings.Join(email.To, ", "))
	writeJSON(w, http.StatusOK, map[string][]string{"to": email.To})
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	s.Updater.CheckNow()
	writeJSON(w, http.StatusAccepted, s.Updater.Status())
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Log.Recent())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// existingParent walks up until it finds a directory that exists, so disk
// usage can be reported even before the recordings folder is created.
func existingParent(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}
