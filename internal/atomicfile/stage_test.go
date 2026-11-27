package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStageOrdersWriteSealPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	stage, err := NewStage(path, Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	defer stage.Discard()

	if _, err := stage.Write([]byte("payload")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("final path exists before publish (stat err = %v)", err)
	}
	if err := stage.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := stage.Write([]byte("more")); err == nil {
		t.Error("write after seal succeeded, want an error")
	}
	if err := stage.Seal(); err == nil {
		t.Error("second seal succeeded, want an error")
	}
	if err := stage.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("final content = %q, want payload", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat final: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("final mode = %o, want 600", got)
	}
}

func TestStagePublishRequiresSeal(t *testing.T) {
	stage, err := NewStage(filepath.Join(t.TempDir(), "state.json"), Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	if err := stage.Publish(); err == nil {
		t.Error("publish without seal succeeded, want an error")
	}
	stage.Discard()
}

func TestStageTempNameIsProtocolShaped(t *testing.T) {
	dir := t.TempDir()
	stage, err := NewStage(filepath.Join(dir, "proxy.json"), Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	defer stage.Discard()

	name := filepath.Base(stage.TempPath())
	if !isTempName(name) {
		t.Errorf("temporary %q is not a protocol name", name)
	}
	if filepath.Dir(stage.TempPath()) != dir {
		t.Errorf("temporary in %s, want same directory as the destination", filepath.Dir(stage.TempPath()))
	}
	if info, err := os.Stat(stage.TempPath()); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("temporary is not a regular file: %v", err)
	}
}

func TestStageHoldsWriterLockUntilPublish(t *testing.T) {
	stage, err := NewStage(filepath.Join(t.TempDir(), "body.bin"), Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	defer stage.Discard()
	if _, err := stage.Write([]byte("bytes")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := stage.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}

	// A second opener cannot take the writer lock while the stage is sealed but
	// unpublished, which is what protects the seal→rename window from a sweep.
	other, err := os.Open(stage.TempPath())
	if err != nil {
		t.Fatalf("open temporary: %v", err)
	}
	defer other.Close()
	err = syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		t.Errorf("lock on a sealed stage = %v, want EWOULDBLOCK", err)
	}
	if err := stage.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func TestStageDiscardRemovesFinalAfterPublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body.bin")
	stage, err := NewStage(path, Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	if _, err := stage.Write([]byte("bytes")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := stage.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := stage.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	stage.Discard()

	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("published file survived discard (stat err = %v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("discard left entries: %v", entries)
	}
}

func TestStageDiscardRemovesTemporary(t *testing.T) {
	dir := t.TempDir()
	stage, err := NewStage(filepath.Join(dir, "body.bin"), Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	temp := stage.TempPath()
	if _, err := stage.Write([]byte("bytes")); err != nil {
		t.Fatalf("write: %v", err)
	}
	stage.Discard()

	if _, err := os.Stat(temp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("temporary survived discard (stat err = %v)", err)
	}
}

// TestStageDiscardBeforePublishKeepsTheExistingFile pins the rollback boundary:
// a stage that never published must not delete content that was already at the
// final path. A crash-residue cleanup would otherwise turn a failed write into
// data loss.
func TestStageDiscardBeforePublishKeepsTheExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.json")
	if err := os.WriteFile(path, []byte("previous record"), 0o600); err != nil {
		t.Fatalf("seed final file: %v", err)
	}

	stage, err := NewStage(path, Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	if _, err := stage.Write([]byte("replacement")); err != nil {
		t.Fatalf("write: %v", err)
	}
	stage.Discard()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final file after discard: %v", err)
	}
	if string(data) != "previous record" {
		t.Errorf("final content = %q, want the previous record", data)
	}
}
