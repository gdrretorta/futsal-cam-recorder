// Package service runs the app as a Windows service and installs/removes it.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// IsService reports whether the process was started by the Service Control Manager.
func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// RunFunc runs the app until ctx is cancelled. It returns true when the app
// wants to be restarted (after an update).
type RunFunc func(ctx context.Context) (restart bool, err error)

type handler struct{ run RunFunc }

func (h handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		restart bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		restart, err := h.run(ctx)
		done <- result{restart, err}
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 20000}
				cancel()
				<-done
				return false, 0
			}
		case r := <-done:
			status <- svc.Status{State: svc.StopPending}
			if r.restart || r.err != nil {
				// A non-zero exit code makes the SCM run the recovery
				// actions set by Install, i.e. start us again.
				return false, 1
			}
			return false, 0
		}
	}
}

// Run hands control to the Service Control Manager.
func Run(name string, run RunFunc) error {
	return svc.Run(name, handler{run})
}

// Install (re)creates the service pointing at exePath and starts it. An
// existing service with the same name, including the WinSW wrapper used by
// version 1.x, is removed first.
func Install(name, displayName, description, exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("sem acesso ao gestor de serviços (execute como Administrador): %w", err)
	}
	defer m.Disconnect()

	if err := remove(m, name); err != nil {
		return err
	}
	cleanupLegacy(filepath.Dir(exePath))

	s, err := m.CreateService(name, exePath, mgr.Config{
		DisplayName: displayName,
		Description: description,
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return fmt.Errorf("criar serviço: %w", err)
	}
	defer s.Close()
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if err := s.SetRecoveryActions(actions, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("configurar recuperação: %w", err)
	}
	// Also restart on a clean exit with a non-zero code (used after updates).
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("configurar recuperação: %w", err)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("iniciar serviço: %w", err)
	}
	return nil
}

// SetExe makes the existing service start exePath from now on, and sets its
// display name. Everything else (start type, account, recovery actions) stays
// as it is. The running process is not affected: the change applies the next
// time the service starts.
func SetExe(name, displayName, exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	// Same quoting as mgr.CreateService, used by Install.
	bin, err := windows.UTF16PtrFromString(syscall.EscapeArg(exePath))
	if err != nil {
		return err
	}
	dn, err := windows.UTF16PtrFromString(displayName)
	if err != nil {
		return err
	}
	const keep = windows.SERVICE_NO_CHANGE
	return windows.ChangeServiceConfig(s.Handle, keep, keep, keep, bin, nil, nil, nil, nil, nil, dn)
}

// Uninstall stops and deletes the service.
func Uninstall(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("sem acesso ao gestor de serviços (execute como Administrador): %w", err)
	}
	defer m.Disconnect()
	return remove(m, name)
}

func remove(m *mgr.Mgr, name string) error {
	s, err := m.OpenService(name)
	if err != nil {
		return nil // not installed
	}
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		s.Control(svc.Stop)
		for i := 0; i < 60; i++ {
			if st, err := s.Query(); err != nil || st.State == svc.Stopped {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	err = s.Delete()
	s.Close()
	if err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return fmt.Errorf("remover serviço: %w", err)
	}
	// Deletion completes once every handle is closed; wait for it.
	for i := 0; i < 40; i++ {
		s, err := m.OpenService(name)
		if err != nil {
			return nil
		}
		s.Close()
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("o serviço antigo ainda não foi removido; reinicie o PC e tente novamente")
}

// cleanupLegacy deletes the files of the 1.x WinSW-based installation.
func cleanupLegacy(dir string) {
	for _, f := range []string{"WinSW.exe", "SimpleRTSPRecorder.exe", "SimpleRTSPRecorder.xml", "Install-Service.ps1", "Uninstall-Service.ps1"} {
		os.Remove(filepath.Join(dir, f))
	}
}
