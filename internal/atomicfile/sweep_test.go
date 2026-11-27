package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedCrashResidue creates a protocol temporary in dir and returns its path.
// When locked it behaves like an in-flight stage; when not it behaves like the
// residue a crashed writer leaves.
func seedCrashResidue(t *testing.T, dir, base string, locked bool) string {
	t.Helper()
	stage, err := NewStage(filepath.Join(dir, base), Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	if _, err := stage.Write([]byte("orphaned bytes")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !locked {
		stage.Abandon()
	}
	t.Cleanup(stage.Abandon)
	ageFile(t, stage.TempPath(), 2*StaleTempMaxAge)
	return stage.TempPath()
}

func ageFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
}

func requireMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still exists (stat err = %v)", path, err)
	}
}

func TestSweepRemovesCrashLeftProtocolTemps(t *testing.T) {
	dir := t.TempDir()
	temp := seedCrashResidue(t, dir, "proxy.json", false)

	report, err := Sweep(dir, StaleTempMaxAge)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(report.Removed) != 1 || report.Removed[0] != filepath.Base(temp) {
		t.Errorf("removed = %v, want [%s]", report.Removed, filepath.Base(temp))
	}
	if len(report.InUse) != 0 {
		t.Errorf("in-use = %v, want none", report.InUse)
	}
	requireMissing(t, temp)

	again, err := Sweep(dir, StaleTempMaxAge)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if !again.Empty() {
		t.Errorf("second sweep = %+v, want empty", again)
	}
}

func TestSweepKeepsTempsHeldByLiveWriter(t *testing.T) {
	dir := t.TempDir()
	temp := seedCrashResidue(t, dir, "body.bin", true)

	// The lock, not the age, is what protects an in-flight write.
	report, err := Sweep(dir, 0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(report.InUse) != 1 || report.InUse[0] != filepath.Base(temp) {
		t.Errorf("in-use = %v, want [%s]", report.InUse, filepath.Base(temp))
	}
	if len(report.Removed) != 0 {
		t.Errorf("removed = %v, want none", report.Removed)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Fatalf("sweep removed a temp with a live writer: %v", err)
	}
}

func TestSweepKeepsFreshTemps(t *testing.T) {
	dir := t.TempDir()
	stage, err := NewStage(filepath.Join(dir, "proxy.json"), Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	stage.Abandon()
	t.Cleanup(stage.Discard)

	report, err := Sweep(dir, StaleTempMaxAge)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !report.Empty() {
		t.Errorf("sweep = %+v, want empty", report)
	}
	if _, err := os.Stat(stage.TempPath()); err != nil {
		t.Fatalf("sweep removed a fresh temp: %v", err)
	}
}

func TestSweepRemovesAgedStagingDirectories(t *testing.T) {
	dir := t.TempDir()
	staging, err := os.MkdirTemp(dir, tempPattern(filepath.Join(dir, "mitt")))
	if err != nil {
		t.Fatalf("create staging directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "SKILL.md"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed staging tree: %v", err)
	}
	ageFile(t, staging, 2*StaleTempMaxAge)

	report, err := Sweep(dir, StaleTempMaxAge)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(report.Removed) != 1 || report.Removed[0] != filepath.Base(staging) {
		t.Errorf("removed = %v, want [%s]", report.Removed, filepath.Base(staging))
	}
	requireMissing(t, staging)
}

func TestSweepLeavesPublishedFilesAndForeignNamesAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte("kept\n"), 0o600); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	foreign := []string{
		"notes.tmp",
		"body-x.tmp",
		".body-123.tmp",
		".proxy-123.json",
		".mitt-log.tmp.bak",
		"mitt-file.tmp",
	}
	for _, name := range foreign {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		ageFile(t, path, 2*StaleTempMaxAge)
	}

	report, err := Sweep(dir, StaleTempMaxAge)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !report.Empty() {
		t.Errorf("sweep = %+v, want empty", report)
	}
	for _, name := range append(foreign, "history.jsonl") {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("sweep touched %s: %v", name, err)
		}
	}
}

func TestSweepWithoutDirectoryIsNotAnError(t *testing.T) {
	report, err := Sweep(filepath.Join(t.TempDir(), "absent"), StaleTempMaxAge)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !report.Empty() {
		t.Errorf("sweep = %+v, want empty", report)
	}
}

func TestSweepIgnoresNonRegularNonDirectoryCandidates(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "."+tempMarker+"link-1"+tempSuffix)
	if err := os.Symlink("/nonexistent", link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// Chtimes follows the link, and the target does not exist, so the entry
	// cannot be aged. A fresh symlink is enough to prove it is not a candidate:
	// the sweep either skips it on age or rejects its mode, never removes it.
	report, err := Sweep(dir, 0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !report.Empty() {
		t.Errorf("sweep = %+v, want empty", report)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("sweep followed and removed a symlink: %v", err)
	}
}

func TestTempPredicateMatchesWriterNames(t *testing.T) {
	dir := t.TempDir()
	for _, base := range []string{"proxy.json", "history.jsonl", "abc-req.bin", "mitt"} {
		stage, err := NewStage(filepath.Join(dir, base), Private)
		if err != nil {
			t.Fatalf("NewStage(%s): %v", base, err)
		}
		if name := filepath.Base(stage.TempPath()); !isTempName(name) {
			t.Errorf("writer temp %q is not a sweep candidate", name)
		}
		stage.Discard()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("stages left entries: %v", entries)
	}
}
