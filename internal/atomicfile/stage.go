package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Stage is one file being written before it is published by rename. It owns the
// protocol's temporary name, its writer lock, its permissions, and the order
// bytes become visible: Write, Seal, Publish. A stage that is discarded —
// including one that fails at any step — leaves neither a temporary nor a final
// file behind.
type Stage struct {
	file      *os.File
	tempPath  string
	finalPath string
	policy    Policy
	sealed    bool
	published bool
}

// NewStage creates a locked temporary file beside finalPath. The temporary is
// named by the protocol, carries policy.FileMode, and rejects writes once it is
// sealed. Parent directories are created with policy.DirMode.
func NewStage(finalPath string, policy Policy) (*Stage, error) {
	dir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, policy.DirMode); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, tempPattern(finalPath))
	if err != nil {
		return nil, fmt.Errorf("create temporary for %s: %w", finalPath, err)
	}
	stage := &Stage{file: file, tempPath: file.Name(), finalPath: finalPath, policy: policy}
	// Lock before anything else: the sweeper treats an unlocked temporary as
	// crash residue, and this lock is what marks it as owned by a live writer
	// from now until the rename.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		stage.Discard()
		return nil, fmt.Errorf("lock temporary for %s: %w", finalPath, err)
	}
	if err := file.Chmod(policy.FileMode); err != nil {
		stage.Discard()
		return nil, err
	}
	return stage, nil
}

// Write appends bytes while the stage is open. A sealed stage — one whose bytes
// are already synced but not yet published — rejects further writes so the
// synced prefix can never diverge from what gets renamed into place.
func (s *Stage) Write(p []byte) (int, error) {
	if s == nil || s.file == nil {
		return 0, errors.New("stage is closed")
	}
	if s.sealed {
		return 0, errors.New("stage is already sealed")
	}
	return s.file.Write(p)
}

// TempPath is the temporary path while the stage is unpublished.
func (s *Stage) TempPath() string {
	if s == nil {
		return ""
	}
	return s.tempPath
}

// FinalPath is the path Publish renames the staged bytes to.
func (s *Stage) FinalPath() string {
	if s == nil {
		return ""
	}
	return s.finalPath
}

// Seal makes the staged bytes durable and closes the write side. The writer
// lock stays held until Publish, so a sweep cannot take the temporary between
// the sync and the rename.
func (s *Stage) Seal() error {
	if s == nil || s.file == nil {
		return errors.New("stage is closed")
	}
	if s.sealed {
		return errors.New("stage is already sealed")
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", s.finalPath, err)
	}
	s.sealed = true
	return nil
}

// Published reports whether the rename has happened. A failure after that
// point (a directory sync) must not be rolled back by deleting the new
// content: the old content is already gone, so removing the new file would
// turn a durability error into data loss.
func (s *Stage) Published() bool {
	return s != nil && s.published
}

// Publish renames the sealed temporary into its final path and then releases
// the writer lock. The file's bytes were already forced by Seal, so only the
// directory entry needs syncing here; the descriptor stays open until after the
// rename so the protected temporary name always exists exactly while it is
// owned.
func (s *Stage) Publish() error {
	if s == nil || s.file == nil {
		return errors.New("stage is closed")
	}
	if !s.sealed {
		return errors.New("stage is not sealed")
	}
	if err := os.Rename(s.tempPath, s.finalPath); err != nil {
		return fmt.Errorf("publish %s: %w", s.finalPath, err)
	}
	s.tempPath = ""
	s.published = true
	if err := syncDir(filepath.Dir(s.finalPath)); err != nil {
		_ = s.file.Close()
		s.file = nil
		return err
	}
	if err := s.file.Close(); err != nil {
		s.file = nil
		return fmt.Errorf("close staged file %s: %w", s.finalPath, err)
	}
	s.file = nil
	return nil
}

// Discard removes what this stage produced. Before Publish that is only the
// temporary, so a failed write never touches content that was already at the
// final path. After Publish it also removes the final file, which is the
// rollback a partially committed record needs. It is safe to call more than
// once; Abandon leaves a temporary for the sweep instead.
func (s *Stage) Discard() {
	if s == nil {
		return
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	if s.tempPath != "" {
		_ = os.Remove(s.tempPath)
		s.tempPath = ""
	}
	if s.published {
		_ = os.Remove(s.finalPath)
		s.published = false
	}
}

// Abandon releases the writer lock and leaves the staged bytes under their
// temporary name. It simulates a crash for tests of the sweep: production
// writers only ever Discard or Publish. Discard still removes the temporary,
// because this stage created that name.
func (s *Stage) Abandon() {
	if s == nil || s.file == nil {
		return
	}
	_ = s.file.Close()
	s.file = nil
}
