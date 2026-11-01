//go:build !windows

package tunnel

import "context"

type otherService struct{}

// NewService returns the cloudflared Windows service; off Windows it does nothing.
func NewService(name string) Service { return otherService{} }

func (otherService) ExePath() (string, error)        { return "", ErrUnsupported }
func (otherService) Stop(ctx context.Context) error  { return ErrUnsupported }
func (otherService) Start(ctx context.Context) error { return ErrUnsupported }
