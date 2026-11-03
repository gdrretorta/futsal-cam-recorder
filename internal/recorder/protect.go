package recorder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// ProtectedFile lists, in the recordings folder, the recordings marked "não
// apagar" (a JSON array of file names). Retention never deletes them, and
// they can't be deleted from the page until the mark is removed.
const ProtectedFile = "protected.json"

// ErrProtected is returned when deleting a protected recording.
var ErrProtected = errors.New("esta gravação está protegida: tire a estrela antes de a apagar")

// protected reads the set of protected names. Caller holds keepMu.
func (r *Recorder) protected(dir string) map[string]bool {
	set := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(dir, ProtectedFile))
	if err != nil {
		return set
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		r.log.Warn("protected recordings list unreadable", "err", err)
		return set
	}
	for _, n := range names {
		if ValidName(n) {
			set[n] = true
		}
	}
	return set
}

// IsProtected reports whether the recording is marked "não apagar".
func (r *Recorder) IsProtected(name string) bool {
	_, dir := r.source()
	r.keepMu.Lock()
	defer r.keepMu.Unlock()
	return r.protected(dir)[name]
}

// SetProtected marks or unmarks a recording as "não apagar".
func (r *Recorder) SetProtected(name string, on bool) error {
	if !ValidName(name) {
		return errors.New("nome de ficheiro inválido")
	}
	_, dir := r.source()
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		return err
	}
	r.keepMu.Lock()
	defer r.keepMu.Unlock()
	set := r.protected(dir)
	if set[name] == on {
		return nil
	}
	if on {
		set[name] = true
	} else {
		delete(set, name)
	}
	names := make([]string, 0, len(set))
	for n := range set {
		// Drop recordings that no longer exist (deleted by hand).
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	data, _ := json.MarshalIndent(names, "", "  ")
	tmp := filepath.Join(dir, ProtectedFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, ProtectedFile)); err != nil {
		os.Remove(tmp)
		return err
	}
	r.log.Info("recording protection changed", "file", name, "protected", on)
	return nil
}
