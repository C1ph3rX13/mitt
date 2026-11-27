// Package skillinstall copies the skill documentation embedded in the mitt
// binary into an agent skill root.
package skillinstall

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
	"github.com/C1ph3rX13/mitt/internal/output"
)

// Action describes what Install did to one skill directory.
type Action string

const (
	// Installed means the destination did not exist before.
	Installed Action = "installed"
	// Updated means an existing destination was replaced because it differed.
	Updated Action = "updated"
	// Unchanged means the destination already matched the bundled tree.
	Unchanged Action = "unchanged"
)

// Result reports the outcome for one skill.
type Result struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Action Action `json:"action" schema:"enum=installed|updated|unchanged"`
}

// Install writes every top-level directory in bundle to root, one directory
// per skill. An identical tree is left untouched. A differing tree is an
// error unless force is true; with force the whole skill directory is replaced
// so files from older versions cannot linger.
func Install(bundle fs.FS, root string, force bool) ([]Result, error) {
	names, err := skillNames(bundle)
	if err != nil {
		return nil, err
	}
	if err := checkRoot(root); err != nil {
		return nil, err
	}

	plans := make([]plan, 0, len(names))
	for _, name := range names {
		dest := filepath.Join(root, name)
		equal, exists, err := treeEqual(bundle, name, dest)
		if err != nil {
			return nil, installError(dest, err)
		}
		action := Installed
		switch {
		case equal:
			action = Unchanged
		case exists && !force:
			return nil, output.NewError(
				output.CodeSkillExists,
				fmt.Sprintf("skill %q already exists at %s and differs from the bundled copy", name, dest),
				"re-run with --force to replace it, or pass --dest to choose another skill root",
			).WithDetail("skill", name).WithDetail("path", dest)
		case exists:
			action = Updated
		}
		plans = append(plans, plan{name: name, dest: dest, action: action})
	}

	// The root is created by SwapDir along with each skill's parent, so no
	// separate mkdir is needed; it would also relabel an existing root that the
	// user chose. An all-Unchanged run needs no root creation at all, because
	// the destinations that produced that verdict already exist.
	results := make([]Result, 0, len(plans))
	for _, p := range plans {
		if p.action != Unchanged {
			if err := replace(bundle, p.name, p.dest); err != nil {
				return nil, installError(p.dest, err)
			}
		}
		results = append(results, Result{Name: p.name, Path: p.dest, Action: p.action})
	}
	return results, nil
}

type plan struct {
	name   string
	dest   string
	action Action
}

func skillNames(bundle fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(bundle, ".")
	if err != nil {
		return nil, output.InternalErrorf("read embedded skills: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, output.InternalErrorf("embedded skill bundle is empty")
	}
	return names, nil
}

func checkRoot(root string) error {
	info, err := os.Stat(root)
	switch {
	case err == nil && !info.IsDir():
		return output.NewError(
			output.CodeInvalidRequest,
			fmt.Sprintf("--dest %q is not a directory", root),
			"pass a directory path, or remove the file at that path",
		).WithDetail("path", root)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return installError(root, err)
	}
	return nil
}

// treeEqual reports whether dest is a directory whose complete tree matches
// the bundled skill. exists is false when dest does not exist at all.
func treeEqual(bundle fs.FS, name, dest string) (equal, exists bool, err error) {
	info, err := os.Lstat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !info.IsDir() {
		return false, true, nil
	}
	equal, err = dirEqual(bundle, name, dest)
	return equal, true, err
}

// dirEqual compares every entry, including extra and missing files. File modes
// are ignored; content is authoritative.
func dirEqual(bundle fs.FS, srcPath, destPath string) (bool, error) {
	srcEntries, err := fs.ReadDir(bundle, srcPath)
	if err != nil {
		return false, fmt.Errorf("read bundled skill %s: %w", srcPath, err)
	}
	destEntries, err := os.ReadDir(destPath)
	if err != nil {
		return false, err
	}
	if len(srcEntries) != len(destEntries) {
		return false, nil
	}
	for i, srcEntry := range srcEntries {
		destEntry := destEntries[i]
		if srcEntry.Name() != destEntry.Name() {
			return false, nil
		}
		destInfo, err := destEntry.Info()
		if err != nil {
			return false, err
		}
		if srcEntry.IsDir() != destInfo.IsDir() {
			return false, nil
		}
		srcChild := path.Join(srcPath, srcEntry.Name())
		destChild := filepath.Join(destPath, destEntry.Name())
		if srcEntry.IsDir() {
			equal, err := dirEqual(bundle, srcChild, destChild)
			if err != nil || !equal {
				return equal, err
			}
			continue
		}
		if !destInfo.Mode().IsRegular() {
			return false, nil
		}
		srcData, err := fs.ReadFile(bundle, srcChild)
		if err != nil {
			return false, fmt.Errorf("read bundled skill file %s: %w", srcChild, err)
		}
		destData, err := os.ReadFile(destChild)
		if err != nil {
			return false, err
		}
		if !bytes.Equal(srcData, destData) {
			return false, nil
		}
	}
	return true, nil
}

// replace builds the skill tree in a temporary directory and swaps it into
// place. The old directory is moved aside first so a failed swap can restore
// it instead of leaving the destination missing. The directory swap itself is
// the persistence protocol's operation, so the staging name, the backup, and
// the crash residue match every other writer.
func replace(bundle fs.FS, name, dest string) error {
	return atomicfile.SwapDir(dest, atomicfile.Shared, func(staging string) error {
		return copyTree(bundle, name, staging)
	})
}

func copyTree(bundle fs.FS, srcPath, destPath string) error {
	entries, err := fs.ReadDir(bundle, srcPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		src := path.Join(srcPath, entry.Name())
		dest := filepath.Join(destPath, entry.Name())
		if entry.IsDir() {
			if err := atomicfile.EnsureDir(dest, atomicfile.Shared.DirMode); err != nil {
				return err
			}
			if err := copyTree(bundle, src, dest); err != nil {
				return err
			}
			continue
		}
		data, err := fs.ReadFile(bundle, src)
		if err != nil {
			return err
		}
		if err := atomicfile.Write(dest, data, atomicfile.Shared); err != nil {
			return err
		}
	}
	return nil
}

func installError(path string, err error) *output.AppError {
	return output.NewError(
		output.CodeSkillInstallFailed,
		fmt.Sprintf("install bundled skills at %s: %v", path, err),
		"check that the destination is writable; pass --dest to choose another skill root",
	).WithDetail("path", path)
}
