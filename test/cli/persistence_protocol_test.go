package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPersistenceWritesGoThroughTheProtocol pins the candidate-08 invariant:
// every place that stages bytes and renames them into place uses
// internal/atomicfile. A new writer that reaches for os.CreateTemp or
// os.WriteFile would reintroduce a second temporary-naming convention, a second
// permission vocabulary, and residue the sweep cannot recognize.
//
// The allowlist is deliberately about roles, not files:
//   - internal/atomicfile is the protocol itself;
//   - truststore/nss.go writes a temporary PEM that must stay alive for an
//     external certutil call, which is not a replace and cannot be atomic.
func TestPersistenceWritesGoThroughTheProtocol(t *testing.T) {
	protocolDir := filepath.Join("internal", "atomicfile") + string(filepath.Separator)
	allowed := map[string]bool{
		filepath.Join("internal", "truststore", "nss.go"): true,
	}
	// stagedWrite is the write half of the protocol. os.MkdirAll and os.Chmod
	// are included because a permission literal outside the protocol is the
	// other half of what drifted (five write sequences, four permission
	// conventions).
	forbidden := []string{
		"os.CreateTemp(",
		"os.MkdirTemp(",
		"os.WriteFile(",
		"os.OpenFile(",
	}

	err := filepath.WalkDir(repoRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasPrefix(rel, protocolDir) || allowed[rel] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(data)
		for _, pattern := range forbidden {
			if strings.Contains(source, pattern) {
				t.Errorf("%s calls %s; stage and publish through internal/atomicfile so temporary names, permissions and crash residue stay in one place", rel, strings.TrimSuffix(pattern, "("))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan sources: %v", err)
	}
}
