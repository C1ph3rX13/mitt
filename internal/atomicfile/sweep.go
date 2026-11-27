package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// SweepReport is what one sweep of a directory changed.
type SweepReport struct {
	// Removed lists the temporary entries deleted from the directory.
	Removed []string
	// InUse lists the temporary files kept because a live writer holds them.
	InUse []string
}

// Empty reports whether the sweep found nothing to do.
func (r SweepReport) Empty() bool {
	return len(r.Removed) == 0 && len(r.InUse) == 0
}

// Sweep removes crash residue from dir. A candidate is an entry whose name the
// protocol can produce (see isTempName) and that satisfies both:
//
//  1. no live writer holds its flock. The kernel releases the lock when a
//     process dies, so an abandoned file is unlocked while an in-flight stage —
//     held open from creation until its rename — is not; and
//  2. its mtime is at least maxAge old. This covers the instant between
//     os.CreateTemp and the writer's lock, when an unfinished file is briefly
//     unlocked.
//
// A staged directory cannot hold an flock, so age is its only guard. Both
// conditions are re-checked on the locked descriptor and inode, so the sweep
// only deletes what it verified, not a path that was swapped underneath. Only
// protocol names are candidates: published files, history.jsonl, and the user's
// own dotfiles are never touched. A failure on one entry is reported but does
// not stop the others.
func Sweep(dir string, maxAge time.Duration) (SweepReport, error) {
	report := SweepReport{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, fmt.Errorf("list %s: %w", dir, err)
	}

	now := time.Now()
	var failures []error
	for _, entry := range entries {
		name := entry.Name()
		if !isTempName(name) {
			continue
		}
		outcome, err := sweepEntry(filepath.Join(dir, name), now, maxAge)
		switch {
		case err != nil:
			if !errors.Is(err, fs.ErrNotExist) {
				failures = append(failures, err)
			}
		case outcome == sweepInUse:
			report.InUse = append(report.InUse, name)
		case outcome == sweepRemoved:
			report.Removed = append(report.Removed, name)
		}
	}
	return report, errors.Join(failures...)
}

type sweepOutcome int

const (
	sweepSkipped sweepOutcome = iota
	sweepInUse
	sweepRemoved
)

// sweepEntry removes one candidate unless a live writer owns it. The lock is
// taken on the opened file and both the inode identity and the age are
// re-checked afterwards, so the sweep only ever deletes the inode it proved
// abandoned.
func sweepEntry(path string, now time.Time, maxAge time.Duration) (sweepOutcome, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return sweepSkipped, err
	}
	if now.Sub(info.ModTime()) < maxAge {
		return sweepSkipped, nil
	}
	if info.IsDir() {
		if err := os.RemoveAll(path); err != nil {
			return sweepSkipped, fmt.Errorf("remove stale staging directory %s: %w", path, err)
		}
		return sweepRemoved, nil
	}
	if !info.Mode().IsRegular() {
		return sweepSkipped, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return sweepSkipped, err
	}
	defer file.Close()

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return sweepInUse, nil
		}
		return sweepSkipped, fmt.Errorf("lock temporary %s: %w", path, err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN) //nolint:errcheck

	locked, err := file.Stat()
	if err != nil {
		return sweepSkipped, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return sweepSkipped, err
	}
	if !os.SameFile(locked, current) {
		// The path was replaced between the check and the lock; leave it for
		// the writer that now owns the new inode.
		return sweepSkipped, nil
	}
	if now.Sub(locked.ModTime()) < maxAge {
		return sweepSkipped, nil
	}
	if err := os.Remove(path); err != nil {
		return sweepSkipped, fmt.Errorf("remove stale temporary %s: %w", path, err)
	}
	return sweepRemoved, nil
}
