// Simple RTSP Recorder: records an RTSP camera to MP4 files, controlled from
// a web page. Runs as a Windows service and updates itself from GitHub.
//
//	simple-rtsp-recorder.exe            run (as service when started by Windows)
//	simple-rtsp-recorder.exe install    install/replace the Windows service and start it
//	simple-rtsp-recorder.exe uninstall  stop and remove the Windows service
//	simple-rtsp-recorder.exe version    print the version
//	simple-rtsp-recorder.exe add-user <email> <role>   create a user, or set its role and
//	                                                   password; password read from stdin;
//	                                                   role is admin, userManager or user
//	simple-rtsp-recorder.exe set-admin-password   set the first admin's password (stdin)
//	simple-rtsp-recorder.exe has-admin-password   exit code 0 if an admin exists, 1 if not
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

	"github.com/joaoopereira/simple-rtsp-recorder/internal/config"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/logging"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/recorder"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/server"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/service"
	"github.com/joaoopereira/simple-rtsp-recorder/internal/updater"
	"github.com/joaoopereira/simple-rtsp-recorder/web"
)

// version is set at build time: -ldflags "-X main.version=v2.0.0".
var version = "dev"

const (
	serviceName = "SimpleRTSPRecorder" // same name as 1.x so upgrades replace it
	displayName = "Simple RTSP Recorder"
	description = "Grava o stream RTSP da câmara, com interface web."
	repo        = "joaoopereira/simple-rtsp-recorder"
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
			err = service.Run(serviceName, func(ctx context.Context) (bool, error) { return run(ctx, exe, *dir) })
			break
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		var restart bool
		restart, err = run(ctx, exe, *dir)
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
func run(ctx context.Context, exe, baseDir string) (restart bool, err error) {
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

	srv := &server.Server{Version: version, FFmpeg: ffmpeg, Store: store, Recorder: rec, Updater: upd, Log: log, Static: web.FS}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(cfg.Port)))
	if err != nil {
		log.Error("listen", "err", err)
		return false, err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	log.Info("web interface ready", "url", fmt.Sprintf("http://localhost:%d", cfg.Port))

	bgCtx, cancelBg := context.WithCancel(ctx)
	defer cancelBg()
	go upd.Run(bgCtx)
	go retention(bgCtx, rec, store)
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
		return errors.New("uso: simple-rtsp-recorder.exe add-user <email> <admin|userManager|user> (palavra-passe no stdin)")
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
