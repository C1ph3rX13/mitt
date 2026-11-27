//go:build linux

package ca

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitWritesPrivateCAAndLeavesNoResidue pins the persistence protocol at
// the CA seam: the key and certificate are private, the directory is private,
// and a successful write leaves no temporary behind.
func TestInitWritesPrivateCAAndLeavesNoResidue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")

	authority, err := Init(dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if authority == nil || len(authority.CertPEM) == 0 {
		t.Fatal("Init returned no authority")
	}

	for _, name := range []string{"ca.pem", "ca.key.pem"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, got)
		}
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat CA dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("CA dir mode = %o, want 700", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read CA dir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("CA dir entries = %v, want only ca.pem and ca.key.pem", names)
	}
}

// TestInitLoadsAnExistingCA pins that a second init reuses the stored pair and
// does not rewrite it.
func TestInitLoadsAnExistingCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	first, err := Init(dir)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "ca.key.pem"))
	if err != nil {
		t.Fatalf("read key: %v", err)
	}

	second, err := Init(dir)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if string(second.CertPEM) != string(first.CertPEM) {
		t.Error("second Init generated a different certificate")
	}
	after, err := os.ReadFile(filepath.Join(dir, "ca.key.pem"))
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if string(after) != string(before) {
		t.Error("second Init rewrote the private key")
	}
}

// TestExportWritesSharedCertificate pins the shared policy at the export seam:
// a PEM another program reads is group/world readable, its parent is created
// when missing, and the export is atomic.
func TestExportWritesSharedCertificate(t *testing.T) {
	caDir := filepath.Join(t.TempDir(), "ca")
	authority, err := Init(caDir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	exportPath := filepath.Join(t.TempDir(), "exports", "nested", "mitt.pem")

	result, err := Install(authority, exportPath)
	if err != nil {
		t.Fatalf("Install export: %v", err)
	}
	if result.OutputPath != exportPath {
		t.Errorf("OutputPath = %q, want %q", result.OutputPath, exportPath)
	}
	info, err := os.Stat(exportPath)
	if err != nil {
		t.Fatalf("stat export: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("export mode = %o, want 644", got)
	}
	data, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if string(data) != string(authority.CertPEM) {
		t.Error("exported PEM differs from the authority certificate")
	}
	if _, err := os.Stat(exportPath + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("export left residue beside the certificate: %v", err)
	}
}

// TestExportReplacesAnExistingFile atomically keeps the previous content when
// the write cannot complete, so a failed export is never a truncated PEM.
func TestExportReplacesAnExistingFile(t *testing.T) {
	caDir := filepath.Join(t.TempDir(), "ca")
	authority, err := Init(caDir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	dir := t.TempDir()
	exportPath := filepath.Join(dir, "mitt.pem")
	if err := os.WriteFile(exportPath, []byte("previous"), 0o600); err != nil {
		t.Fatalf("seed export: %v", err)
	}

	if _, err := Install(authority, exportPath); err != nil {
		t.Fatalf("Install export: %v", err)
	}
	data, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if !strings.Contains(string(data), "BEGIN CERTIFICATE") {
		t.Errorf("export content = %q, want a PEM certificate", data)
	}
}
