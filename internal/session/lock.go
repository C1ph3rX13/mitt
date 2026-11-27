package session

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLocked reports that another process holds the data-directory lock.
var ErrLocked = errors.New("another proxy holds the data-directory lock")

// Lock is an advisory lock on the data directory. The lock is released by the
// kernel when the process exits, so a crash never leaves a lock behind.
type Lock struct {
	file *os.File
}

// AcquireLock takes an exclusive non-blocking lock on the data directory. The
// data directory itself is locked (via flock) so no extra lock file leaks into
// the store contract.
func (s *Store) AcquireLock() (*Lock, error) {
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	file, err := os.Open(s.dir)
	if err != nil {
		return nil, fmt.Errorf("open data directory for locking: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	return &Lock{file: file}, nil
}

// Release unlocks the data directory. It is safe to call more than once.
func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
