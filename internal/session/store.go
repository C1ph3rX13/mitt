package session

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// Store is the on-disk data directory for one mitt environment.
type Store struct {
	dir string
}

// Open returns a store rooted at dir without touching the filesystem.
func Open(dir string) *Store {
	return &Store{dir: dir}
}

// Dir is the absolute data directory path.
func (s *Store) Dir() string {
	return s.dir
}

// CADir is the root CA directory inside the data directory.
func (s *Store) CADir() string {
	return filepath.Join(s.dir, "ca")
}

// ProxyStatePath is the runtime proxy state file.
func (s *Store) ProxyStatePath() string {
	return filepath.Join(s.dir, "proxy.json")
}

// PortStore returns the isolated data directory for one proxy port. The layout
// is fixed at <store>/ports/<port>/ when --session-per-port is enabled.
func (s *Store) PortStore(port int) *Store {
	return Open(filepath.Join(s.dir, "ports", strconv.Itoa(port)))
}

// EnsureLayout creates the data directory and the bodies directory.
func (s *Store) EnsureLayout() error {
	return s.ensureLayout()
}

// ensureDir creates the data directory with the private policy's mode.
func (s *Store) ensureDir() error {
	if err := atomicfile.EnsureDir(s.dir, atomicfile.Private.DirMode); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	return nil
}

// ensureLayout creates the data directory and the bodies directory.
func (s *Store) ensureLayout() error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	if err := atomicfile.EnsureDir(s.BodiesDir(), atomicfile.Private.DirMode); err != nil {
		return fmt.Errorf("create bodies directory: %w", err)
	}
	return nil
}
