//go:build !windows

// Package service runs the app as a Windows service. On other systems
// (development) the app only runs in the foreground.
package service

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("o serviço só está disponível em Windows")

// RunFunc runs the app until ctx is cancelled; true means restart.
type RunFunc func(ctx context.Context) (restart bool, err error)

func IsService() bool                 { return false }
func Run(string, RunFunc) error       { return errUnsupported }
func Install(_, _, _, _ string) error { return errUnsupported }
func Uninstall(string) error          { return errUnsupported }
func SetExe(_, _, _ string) error     { return errUnsupported }
