package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestSwapDirInstallsBuiltTreeAndReplacesExisting(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "mitt")
	if err := os.MkdirAll(filepath.Join(dest, "old"), 0o755); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "old", "file.md"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}

	err := SwapDir(dest, Shared, func(staging string) error {
		if err := os.MkdirAll(filepath.Join(staging, "reference"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(staging, "reference", "doc.md"), []byte("fresh"), 0o644)
	})
	if err != nil {
		t.Fatalf("SwapDir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "reference", "doc.md")); err != nil {
		t.Fatalf("swap did not install the new tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "old")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old tree survived the swap (stat err = %v)", err)
	}
	entries := entryNames(t, parent)
	if want := []string{"mitt"}; !equalStrings(entries, want) {
		t.Errorf("parent entries = %v, want %v (swap left residue)", entries, want)
	}
}

func TestSwapDirCreatesDestinationWhenMissing(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "skills", "mitt")
	err := SwapDir(dest, Shared, func(staging string) error {
		return os.WriteFile(filepath.Join(staging, "SKILL.md"), []byte("doc"), 0o644)
	})
	if err != nil {
		t.Fatalf("SwapDir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed file: %v", err)
	}
	if string(data) != "doc" {
		t.Errorf("content = %q, want doc", data)
	}
}

func TestSwapDirFailedBuildLeavesDestinationAlone(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "mitt")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte("previous"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	buildErr := errors.New("copy failed")
	err := SwapDir(dest, Shared, func(staging string) error {
		if err := os.WriteFile(filepath.Join(staging, "partial.md"), []byte("x"), 0o644); err != nil {
			return err
		}
		return buildErr
	})
	if !errors.Is(err, buildErr) {
		t.Fatalf("SwapDir = %v, want the build error", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if err != nil {
		t.Fatalf("destination was disturbed: %v", err)
	}
	if string(data) != "previous" {
		t.Errorf("destination content = %q, want previous", data)
	}
	entries := entryNames(t, parent)
	if want := []string{"mitt"}; !equalStrings(entries, want) {
		t.Errorf("parent entries = %v, want %v (failed build left residue)", entries, want)
	}
}

func TestSwapDirBackupIsNotASweepCandidate(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "mitt")
	name := backupPattern(dest)
	if isTempName(name) {
		t.Errorf("backup name %q matches the temporary predicate; a sweep could delete the only previous copy", name)
	}
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
