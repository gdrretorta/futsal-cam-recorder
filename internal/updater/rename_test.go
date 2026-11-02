package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateExeName(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, LegacyAssetName)
	if err := os.WriteFile(legacy, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var pointed string
	got, err := MigrateExeName(legacy, func(p string) error { pointed = p; return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, AssetName)
	if got != want || pointed != want {
		t.Fatalf("new exe = %q, service pointed at %q, want %q", got, pointed, want)
	}
	if b, _ := os.ReadFile(want); string(b) != "binary" {
		t.Errorf("new exe = %q", b)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Error("old exe must stay until the new one runs")
	}
	if _, err := os.Stat(want + ".new"); !os.IsNotExist(err) {
		t.Error("temporary copy left behind")
	}

	// Running under the new name: nothing to migrate, the old files go.
	os.WriteFile(legacy+".old", []byte("older"), 0o755)
	if got, err := MigrateExeName(want, func(string) error { t.Fatal("repointed again"); return nil }); got != "" || err != nil {
		t.Fatalf("second migration: %q, %v", got, err)
	}
	CleanupLegacy(want)
	for _, p := range []string{legacy, legacy + ".old"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s not removed", filepath.Base(p))
		}
	}
	if _, err := os.Stat(want); err != nil {
		t.Error("cleanup removed the running exe")
	}
}

func TestMigrateExeNameKeepsOldNameOnFailure(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, LegacyAssetName)
	os.WriteFile(legacy, []byte("binary"), 0o755)
	got, err := MigrateExeName(legacy, func(string) error { return errors.New("access denied") })
	if err == nil || got != "" {
		t.Fatalf("got %q, %v; want an error", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, AssetName)); !os.IsNotExist(err) {
		t.Error("copy left behind after a failed repoint")
	}
	if b, _ := os.ReadFile(legacy); string(b) != "binary" {
		t.Error("old exe changed")
	}
}

func TestCleanupLegacyOnlyFromNewName(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, LegacyAssetName)
	os.WriteFile(legacy, []byte("binary"), 0o755)
	CleanupLegacy(legacy) // still running the old name
	CleanupLegacy(filepath.Join(dir, "dev-build.exe"))
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("removed the exe the service runs")
	}
}
