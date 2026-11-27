package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SwapDir replaces dest with a directory built by build. build receives an
// empty staging directory in dest's parent, so the finished tree is renamed
// into place in one step; an existing dest is moved aside first and restored
// when the swap fails. A failed build removes the staging tree, so the only
// residue a swap can leave is a crash between the two renames — an aged
// protocol temporary that Sweep recognizes.
func SwapDir(dest string, policy Policy, build func(staging string) error) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, policy.DirMode); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, tempPattern(dest))
	if err != nil {
		return fmt.Errorf("create staging directory for %s: %w", dest, err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Chmod(staging, policy.DirMode); err != nil {
		return err
	}
	if err := build(staging); err != nil {
		return err
	}

	backup := ""
	if _, statErr := os.Lstat(dest); statErr == nil {
		backup, err = unusedPath(parent, backupPattern(dest))
		if err != nil {
			return err
		}
		if err := os.Rename(dest, backup); err != nil {
			return err
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}

	if err := os.Rename(staging, dest); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dest)
		}
		return fmt.Errorf("swap %s into place: %w", dest, err)
	}
	success = true
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return syncDir(parent)
}

// backupPattern names the directory moved aside during a swap. It deliberately
// does not match the temporary-name predicate: a crash in the swap window
// leaves the destination missing with its only previous copy under this name,
// and a sweep must not delete that copy.
func backupPattern(dest string) string {
	return "." + filepath.Base(dest) + ".mitt-backup-*"
}

// unusedPath returns a path that does not exist yet in parent, so a rename to
// it stays on one filesystem. The reservation file itself is removed before
// the caller renames a directory over the name.
func unusedPath(parent, pattern string) (string, error) {
	file, err := os.CreateTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}
