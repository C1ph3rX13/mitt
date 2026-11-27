// Package atomicfile is mitt's persistence protocol: the one place that decides
// how a temporary name is chosen, what permissions a written file or directory
// gets, and the order bytes become visible and durable. Every writer that stages
// bytes and renames them into place goes through it — the store's proxy state,
// staged body files, history lines, the CA key and certificate, the system
// trust anchor, exported certificates, and installed skill trees. Two policies
// name the two permission conventions mitt uses, so the 0600/0700 and 0644/0755
// vocabularies cannot drift between callers.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// StaleTempMaxAge is the quiet period after which a temporary entry without a
// live writer counts as crash residue. The age is a second guard, not the
// primary rule: a writer takes an exclusive flock on a staged file as soon as
// it is created, so a live writer is protected by the lock. The age covers the
// instant between creation and the lock, and it is the only guard a staged
// directory can have.
const StaleTempMaxAge = time.Hour

// Policy is the permission half of the protocol. The durability order is not a
// parameter: every staged file is synced before it is renamed and every rename
// is followed by a directory sync, so a call that returns nil reports bytes a
// crash cannot take back.
type Policy struct {
	// DirMode is the mode for directories created on the way to a path. An
	// existing directory keeps its mode: a shared destination such as the
	// system trust-anchor directory is not mitt's to relabel.
	DirMode os.FileMode
	// FileMode is the mode of the final file.
	FileMode os.FileMode
}

var (
	// Private is the data-directory policy: 0700 directories, 0600 files. The
	// store, the CA directory, staged bodies, proxy state and history all use
	// it.
	Private = Policy{DirMode: 0o700, FileMode: 0o600}
	// Shared is the policy for files another program or the user reads: 0755
	// directories, 0644 files. Trust anchors, exported certificates and
	// installed skill trees use it.
	Shared = Policy{DirMode: 0o755, FileMode: 0o644}
)

// EnsureDir creates dir and corrects its mode. It is for directories mitt owns.
// Write only creates missing parents, because a shared destination may already
// exist with a mode that is not mitt's to change.
func EnsureDir(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	return os.Chmod(dir, mode)
}

// The protocol's temporary names are hidden siblings of the destination,
// marked with ".mitt-" so the sweeper can tell mitt residue from a user's own
// dotfiles, and suffixed ".tmp" so the shape is obvious in a directory listing.
const (
	tempMarker = "mitt-"
	tempSuffix = ".tmp"
)

// tempPattern is the os.CreateTemp pattern for writing finalPath.
func tempPattern(finalPath string) string {
	return "." + tempMarker + filepath.Base(finalPath) + "-*" + tempSuffix
}

// isTempName reports whether name is a temporary name a writer can leave
// behind. Sweep and every writer share this predicate, so the two cannot drift.
func isTempName(name string) bool {
	return strings.HasPrefix(name, "."+tempMarker) && strings.HasSuffix(name, tempSuffix)
}

// Write replaces path with data. Parent directories are created with
// policy.DirMode. The bytes go to a locked protocol temporary file that is
// given policy.FileMode, sealed, and renamed into place; a failure removes the
// temporary and leaves the previous content untouched.
func Write(path string, data []byte, policy Policy) error {
	stage, err := NewStage(path, policy)
	if err != nil {
		return err
	}
	if _, err := stage.Write(data); err != nil {
		stage.Discard()
		return err
	}
	if err := stage.Seal(); err != nil {
		stage.Discard()
		return err
	}
	if err := stage.Publish(); err != nil {
		// Before the rename the temporary is the only trace; after it the new
		// content is already in place, so it must be kept and the error
		// reported.
		if !stage.Published() {
			stage.Discard()
		}
		return err
	}
	return nil
}

// Append adds bytes to path under the protocol's append rules: the file is
// created with policy.FileMode when absent and corrected when it is not, one
// exclusive advisory lock covers the whole call, and a successful return means
// both the bytes and the directory entry are forced.
//
// The write callback owns the bytes. It receives the locked file — opened
// read-write so it can inspect what is already there — may truncate a
// crash-truncated tail before adding the next line, and must not close the
// file. When it returns nil the protocol syncs the file and then the parent
// directory. When it returns an error the protocol does neither: only the
// callback knows whether its bytes became visible, so it also owns putting
// them back.
func Append(path string, policy Policy, write func(*os.File) error) error {
	if err := os.MkdirAll(filepath.Dir(path), policy.DirMode); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, policy.FileMode)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(policy.FileMode); err != nil {
		return err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN) //nolint:errcheck

	if err := write(file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes the directory entries in dir durable. A rename or a newly
// created file is only durable once its parent directory is synced.
func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", dir, err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}
