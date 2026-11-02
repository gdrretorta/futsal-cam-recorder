package updater

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LegacyAssetName is the name of the exe until 2.8.1, when the app was
// called Simple RTSP Recorder. Releases keep publishing a copy under this
// name, because PCs on 2.8.1 or older look for it.
const LegacyAssetName = "simple-rtsp-recorder.exe"

// MigrateExeName moves a PC that still runs the exe under its old name to
// the new one. Auto-update from 2.8.1 or older replaces the running exe in
// place, so after that update the service still runs simple-rtsp-recorder.exe.
//
// When exe has the old name, MigrateExeName copies it to futsal-cam-recorder.exe
// in the same folder, checks the copy, and calls repoint to make the service
// start the copy from now on. It returns the new path; the caller must then
// restart so the service runs it. The old exe is left in place (a running exe
// can't be deleted on Windows) and is removed by CleanupLegacy once the new
// one runs. On any error nothing changes: the copy is removed and the service
// keeps running the old name, which still updates normally.
//
// It returns "" when exe already has another name.
func MigrateExeName(exe string, repoint func(newExe string) error) (string, error) {
	if !strings.EqualFold(filepath.Base(exe), LegacyAssetName) {
		return "", nil
	}
	target := filepath.Join(filepath.Dir(exe), AssetName)
	if err := copyVerified(exe, target); err != nil {
		os.Remove(target)
		return "", err
	}
	if err := repoint(target); err != nil {
		os.Remove(target)
		return "", fmt.Errorf("point the service at %s: %w", target, err)
	}
	return target, nil
}

// CleanupLegacy removes the exe with the old name (and what an update left
// next to it) once exe, the running one, has the new name. Only call it from
// the service: the service runs exe, so the old file is no longer used.
func CleanupLegacy(exe string) {
	if !strings.EqualFold(filepath.Base(exe), AssetName) {
		return
	}
	legacy := filepath.Join(filepath.Dir(exe), LegacyAssetName)
	for _, p := range []string{legacy, legacy + ".old", legacy + ".new"} {
		os.Remove(p)
	}
}

// copyVerified copies src to dst through a temporary file and checks that
// dst ends up with the same content.
func copyVerified(src, dst string) error {
	want, err := fileHash(src)
	if err != nil {
		return err
	}
	tmp := dst + ".new"
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if serr := out.Sync(); err == nil {
		err = serr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		os.Remove(dst)
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	got, err := fileHash(dst)
	if err != nil {
		return err
	}
	if got != want {
		return errors.New("copy of the exe doesn't match the original")
	}
	return nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return string(h.Sum(nil)), nil
}
