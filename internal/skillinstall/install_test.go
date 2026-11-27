package skillinstall

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"testing/fstest"

	"github.com/C1ph3rX13/mitt/internal/output"
)

// testBundle is a two-file skill tree with one nested directory.
func testBundle() fstest.MapFS {
	return fstest.MapFS{
		"mitt/SKILL.md":              {Data: []byte("skill body")},
		"mitt/reference/commands.md": {Data: []byte("commands")},
		"mitt/reference/trouble.md":  {Data: []byte("troubleshooting")},
		"other/README.md":            {Data: []byte("other")},
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

func TestInstallWritesEverySkillWithSharedModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")

	results, err := Install(testBundle(), root, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, result := range results {
		if result.Action != Installed {
			t.Errorf("%s action = %s, want installed", result.Name, result.Action)
		}
	}

	data, err := os.ReadFile(filepath.Join(root, "mitt", "reference", "commands.md"))
	if err != nil {
		t.Fatalf("read installed reference: %v", err)
	}
	if string(data) != "commands" {
		t.Errorf("content = %q, want commands", data)
	}
	fileInfo, err := os.Stat(filepath.Join(root, "mitt", "SKILL.md"))
	if err != nil {
		t.Fatalf("stat installed file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode = %o, want 644", got)
	}
	dirInfo, err := os.Stat(filepath.Join(root, "mitt", "reference"))
	if err != nil {
		t.Fatalf("stat installed dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o755 {
		t.Errorf("dir mode = %o, want 755", got)
	}

	// The install must not leave a staging tree or backup beside the skills.
	if want := []string{"mitt", "other"}; !equalStrings(entryNames(t, root), want) {
		t.Errorf("root entries = %v, want %v", entryNames(t, root), want)
	}
}

func TestInstallSecondRunIsUnchanged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	if _, err := Install(testBundle(), root, false); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	results, err := Install(testBundle(), root, false)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	for _, result := range results {
		if result.Action != Unchanged {
			t.Errorf("%s action = %s, want unchanged", result.Name, result.Action)
		}
	}
}

func TestInstallRefusesADifferingTreeWithoutForce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	if _, err := Install(testBundle(), root, false); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	edited := filepath.Join(root, "mitt", "SKILL.md")
	if err := os.WriteFile(edited, []byte("locally edited"), 0o644); err != nil {
		t.Fatalf("edit installed file: %v", err)
	}

	_, err := Install(testBundle(), root, false)
	if err == nil {
		t.Fatal("Install replaced a differing tree without --force")
	}
	var appErr *output.AppError
	if !errors.As(err, &appErr) || appErr.Code != output.CodeSkillExists {
		t.Fatalf("error = %v, want SKILL_EXISTS", err)
	}
	if data, _ := os.ReadFile(edited); string(data) != "locally edited" {
		t.Errorf("refused install still changed the file: %q", data)
	}
}

// TestInstallForceReplacesTheWholeTree pins the directory-swap contract: stale
// files from an older bundle cannot linger, and the swap leaves no staging
// directory behind.
func TestInstallForceReplacesTheWholeTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	if _, err := Install(testBundle(), root, false); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	stale := filepath.Join(root, "mitt", "stale.md")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}

	results, err := Install(testBundle(), root, true)
	if err != nil {
		t.Fatalf("forced Install: %v", err)
	}
	for _, result := range results {
		if result.Name == "mitt" && result.Action != Updated {
			t.Errorf("mitt action = %s, want updated", result.Action)
		}
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale file survived the swap: %v", err)
	}
	if want := []string{"mitt", "other"}; !equalStrings(entryNames(t, root), want) {
		t.Errorf("root entries = %v, want %v", entryNames(t, root), want)
	}
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
