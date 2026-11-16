// Futsal Cam Recorder: records an RTSP camera to MP4 files, controlled from
// a web page. Runs as a Windows service and updates itself from GitHub.
//
//	futsal-cam-recorder.exe            run (as service when started by Windows)
//	futsal-cam-recorder.exe install    install/replace the Windows service and start it
//	futsal-cam-recorder.exe uninstall  stop and remove the Windows service
//	futsal-cam-recorder.exe version    print the version
//	futsal-cam-recorder.exe add-user <email> <role>   create a user, or set its role and
//	                                                   password; password read from stdin;
//	                                                   role is admin, userManager or user
//	futsal-cam-recorder.exe set-admin-password   set the first admin's password (stdin)
//	futsal-cam-recorder.exe has-admin-password   exit code 0 if an admin exists, 1 if not
//
// Flags: -dir <path> uses another base directory (config, logs) — handy for
// development, where the binary lives in a temp folder.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/config"
	"github.com/gdrretorta/futsal-cam-recorder/internal/logging"
	"github.com/gdrretorta/futsal-cam-recorder/internal/notify"
	"github.com/gdrretorta/futsal-cam-recorder/internal/recorder"
	"github.com/gdrretorta/futsal-cam-recorder/internal/server"
	"github.com/gdrretorta/futsal-cam-recorder/internal/service"
	"github.com/gdrretorta/futsal-cam-recorder/internal/sysinfo"
	"github.com/gdrretorta/futsal-cam-recorder/internal/tunnel"
	"github.com/gdrretorta/futsal-cam-recorder/internal/updater"
	"github.com/gdrretorta/futsal-cam-recorder/web"
)

// version is set at build time: -ldflags "-X main.version=v2.0.0".
var version = "dev"

const (
	serviceName = "SimpleRTSPRecorder" // same name as 1.x so upgrades replace it
	displayName = "Futsal Cam Recorder"
	description = "Grava o stream RTSP da câmara, com interface web."
	repo        = "gdrretorta/futsal-cam-recorder"
)

func main() {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fatal(err)
	}
	dir := flag.String("dir", filepath.Dir(exe), "base directory for config, logs and default recordings folder")
	flag.Parse()

	switch cmd := flag.Arg(0); cmd {
	case "", "run":
		if service.IsService() {
			err = service.Run(serviceName, func(ctx context.Context) (bool, error) { return run(ctx, exe, *dir, true) })
			break
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		var restart bool
		restart, err = run(ctx, exe, *dir, false)
		stop()
		if err == nil && restart {
			err = reexec(exe)
		}
	case "install":
		// Don't touch config.json here: the service just started and owns it.
		if err = service.Install(serviceName, displayName, description, exe); err == nil {
			fmt.Println("Serviço instalado e iniciado.")
		}
	case "uninstall":
		if err = service.Uninstall(serviceName); err == nil {
			fmt.Println("Serviço removido.")
		}
	case "version":
		fmt.Println(version)
	case "add-user":
		err = addUser(*dir, flag.Arg(1), flag.Arg(2))
	case "set-admin-password":
		err = setAdminPassword(*dir)
	case "has-admin-password":
		store, oerr := config.Open(*dir)
		if oerr != nil {
			fatal(oerr)
		}
		if !store.HasAdmin() {
			os.Exit(1)
		}
	default:
		err = fmt.Errorf("comando desconhecido %q (use install, uninstall, add-user, set-admin-password, version ou nada para correr)", cmd)
	}
	if err != nil {
		fatal(err)
	}
}

// run starts everything and blocks until ctx is cancelled or an update asks
// for a restart.
func run(ctx context.Context, exe, baseDir string, asService bool) (restart bool, err error) {
	log, err := logging.New(filepath.Join(baseDir, "logs"))
	if err != nil {
		return false, err
	}
	store, err := config.Open(baseDir)
	if err != nil {
		log.Error("config", "err", err)
		return false, err
	}
	cfg := store.Get()
	ffmpeg := findFFmpeg(cfg.FFmpegPath, filepath.Dir(exe))
	log.Info("starting", "version", version, "dir", baseDir, "ffmpeg", ffmpeg)

	if asService && renameExe(exe, log) {
		return true, nil
	}
	if asService {
		lockInstallDir(baseDir, store.RecordingsDir(), log)
	}

	rec := recorder.New(ffmpeg, func() (string, string) {
		return store.Get().Camera.StreamURL(), store.RecordingsDir()
	}, filepath.Join(baseDir, "recording.state"), log.Logger)
	rec.CleanupTemp()

	restartCh := make(chan struct{}, 1)
	upd := updater.New(updater.Options{
		Repo: repo, Current: version, ExePath: exe, Log: log.Logger,
		Enabled: func() bool { return store.Get().AutoUpdate },
		TryLock: rec.TryLock,
		Unlock:  rec.Unlock,
		Restart: func() { restartCh <- struct{}{} },
	})
	upd.CleanupOld()

	tunnelName := cfg.Tunnel.ServiceName
	if tunnelName == "" {
		tunnelName = "Cloudflared"
	}
	tun := &tunnel.Updater{
		Service: tunnel.NewService(tunnelName), Config: store.Get, AppVersion: version,
		TryLock: rec.TryLock, Unlock: rec.Unlock, Log: log.Logger,
		Notify: func(subject, text string) { emailAdmins(store, log, subject, text) },
	}

	srv := &server.Server{Version: version, FFmpeg: ffmpeg, Store: store, Recorder: rec, Updater: upd, Tunnel: tun, Log: log, Static: web.FS, TrustLocal: true}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	port := strconv.Itoa(cfg.Port)
	ln, err := net.Listen("tcp", net.JoinHostPort("", port))
	if err != nil {
		log.Error("listen", "err", err)
		return false, err
	}
	serveErr := make(chan error, 4)
	serve := func(ln net.Listener) {
		go func() {
			// A listener closed on purpose (localOnly) is not an error.
			if err := httpSrv.Serve(ln); !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
			}
		}()
	}
	serve(ln)
	log.Info("web interface ready", "url", fmt.Sprintf("http://localhost:%d", cfg.Port))

	bgCtx, cancelBg := context.WithCancel(ctx)
	defer cancelBg()
	go localOnly(bgCtx, srv, ln, port, serve, log)
	go upd.Run(bgCtx)
	go retention(bgCtx, rec, store)
	alerts := &notify.Watcher{
		Config:    store.Get,
		StartedAt: func() *time.Time { return rec.Status().StartedAt },
		Log:       log.Logger,
	}
	go alerts.Run(bgCtx)
	health := &notify.HealthWatcher{
		Config: store.Get,
		Camera: func() (bool, bool, string) {
			st := rec.Status()
			down := st.State == recorder.StateConnecting || st.State == recorder.StateReconnecting
			return st.StartedAt != nil && st.State != recorder.StateStopping, down, st.Error
		},
		Disk: func() (uint64, uint64, error) { return sysinfo.Disk(store.RecordingsDir()) },
		Log:  log.Logger,
	}
	go health.Run(bgCtx)
	go tun.Run(bgCtx)
	rec.Resume()

	select {
	case <-ctx.Done():
	case <-restartCh:
		restart = true
	case err = <-serveErr:
		log.Error("http server", "err", err)
	}

	log.Info("shutting down", "restart", restart)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdownCtx)
	rec.Shutdown(shutdownCtx)
	return restart, err
}

// renameExe moves a PC upgraded by auto-update from 2.8.1 or older, whose
// service still runs simple-rtsp-recorder.exe, to futsal-cam-recorder.exe. It
// returns true when the service now points at the new exe and must restart
// (nothing has started yet, so a recording to resume just waits a few
// seconds). On failure the app keeps running under the old name, which still
// updates normally, and tries again at the next start.
func renameExe(exe string, log *logging.Logger) (restart bool) {
	newExe, err := updater.MigrateExeName(exe, func(p string) error {
		return service.SetExe(serviceName, displayName, p)
	})
	if err != nil {
		log.Warn("could not switch to the new exe name, keeping the old one", "err", err)
		return false
	}
	if newExe != "" {
		log.Info("service switched to the new exe name, restarting", "exe", newExe)
		return true
	}
	updater.CleanupLegacy(exe)
	return false
}

// lockInstallDir keeps the install folder to SYSTEM and the Administrators
// (see service.LockDir). A failure only logs: the app runs as before.
func lockInstallDir(baseDir, recordings string, log *logging.Logger) {
	if filepath.Dir(baseDir) == baseDir {
		log.Warn("the app runs from a drive's root folder: not restricting it", "dir", baseDir)
		return
	}
	keepOpen := recordings
	if rel, err := filepath.Rel(baseDir, recordings); err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		keepOpen = "" // not inside the install folder: untouched
	}
	changed, err := service.LockDir(baseDir, keepOpen)
	switch {
	case err != nil:
		log.Warn("could not restrict the install folder to administrators", "err", err)
	case changed:
		log.Info("install folder restricted to administrators and SYSTEM", "dir", baseDir, "recordings", keepOpen)
	}
}

// localOnly stops listening on the network (plain HTTP, readable on the LAN)
// once the public site is shown to reach this app from loopback, i.e.
// cloudflared on this PC points at localhost. Until that is proven (tunnel
// down, no site address, cloudflared pointing at the PC's LAN address),
// nothing changes, and if the site stops answering after the switch, the
// network listener comes back: this must never cut the owner off.
func localOnly(ctx context.Context, srv *server.Server, all net.Listener, port string, serve func(net.Listener), log *logging.Logger) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = time.Hour
		loopback, err := srv.TunnelOrigin(ctx)
		if err != nil {
			continue // tunnel down or not set up: try again later
		}
		if !loopback {
			log.Info("the tunnel reaches this app over the network: still listening on all addresses")
			return
		}
		all.Close()
		var local []net.Listener
		for _, host := range []string{"127.0.0.1", "::1"} {
			if ln, err := net.Listen("tcp", net.JoinHostPort(host, port)); err == nil {
				serve(ln)
				local = append(local, ln)
			}
		}
		if len(local) > 0 {
			if _, err = srv.TunnelOrigin(ctx); err == nil {
				log.Info("listening on this PC only (the tunnel reaches it through localhost)")
				return
			}
			for _, ln := range local {
				ln.Close()
			}
		}
		log.Warn("the public site stopped answering on localhost only: listening on all addresses again", "err", err)
		for {
			if all, err = net.Listen("tcp", net.JoinHostPort("", port)); err == nil {
				serve(all)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// emailAdmins sends a message to the admins, when emails are configured.
func emailAdmins(store *config.Store, log *logging.Logger, subject, text string) {
	cfg := store.Get()
	if !cfg.Notify.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	email := notify.Email{To: notify.AdminEmails(cfg), Subject: cfg.Title + ": " + subject, Text: text}
	if err := notify.Send(ctx, cfg.Notify, email); err != nil {
		log.Warn("email to admins not sent", "subject", subject, "err", err)
	}
}

func retention(ctx context.Context, rec *recorder.Recorder, store *config.Store) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		rec.Prune(store.Get().RetentionDays)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// findFFmpeg prefers the configured path, then ffmpeg next to the exe, then PATH.
func findFFmpeg(configured, exeDir string) string {
	if configured != "" {
		return configured
	}
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	if local := filepath.Join(exeDir, name); fileExists(local) {
		return local
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return name
}

// readPassword reads one line from stdin, so the password never appears in
// the process list. A running service picks up the change immediately.
func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("nenhuma palavra-passe recebida")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func addUser(dir, email, role string) error {
	if email == "" || role == "" {
		return errors.New("uso: futsal-cam-recorder.exe add-user <email> <admin|userManager|user> (palavra-passe no stdin)")
	}
	r, err := config.ParseRole(role)
	if err != nil {
		return err
	}
	store, err := config.Open(dir)
	if err != nil {
		return err
	}
	pw, err := readPassword("Palavra-passe de " + email + ": ")
	if err != nil {
		return err
	}
	u, err := store.SetUser(email, r, pw)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nUtilizador %s (%s) guardado.\n", u.Email, u.Role)
	return nil
}

func setAdminPassword(dir string) error {
	store, err := config.Open(dir)
	if err != nil {
		return err
	}
	pw, err := readPassword("Nova palavra-passe de administrador: ")
	if err != nil {
		return err
	}
	if err := store.SetAdminPassword(pw); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "\nPalavra-passe de administrador guardada.")
	return nil
}

// reexec starts the (updated) binary again when running in a console.
func reexec(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return errors.New("atualização instalada, mas não foi possível reiniciar: " + err.Error())
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "erro:", err)
	os.Exit(1)
}
