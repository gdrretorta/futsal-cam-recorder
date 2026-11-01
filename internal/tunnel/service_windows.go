package tunnel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type winService struct{ name string }

// NewService returns the cloudflared Windows service ("Cloudflared" when
// installed with `cloudflared service install`).
func NewService(name string) Service { return winService{name} }

func (w winService) open() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(w.name)
	if err != nil {
		m.Disconnect()
		return nil, nil, fmt.Errorf("serviço %q não encontrado: %w", w.name, err)
	}
	return m, s, nil
}

func (w winService) ExePath() (string, error) {
	m, s, err := w.open()
	if err != nil {
		return "", err
	}
	defer m.Disconnect()
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return "", err
	}
	return ExeFromCommandLine(c.BinaryPathName), nil
}

func (w winService) Stop(ctx context.Context) error {
	m, s, err := w.open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return err
	}
	return wait(ctx, s, svc.Stopped)
}

func (w winService) Start(ctx context.Context) error {
	m, s, err := w.open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return wait(ctx, s, svc.Running)
}

func wait(ctx context.Context, s *mgr.Service, want svc.State) error {
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("o serviço não chegou ao estado %d a tempo", want)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
