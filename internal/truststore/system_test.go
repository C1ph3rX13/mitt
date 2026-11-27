//go:build linux

package truststore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type runCall struct {
	name string
	args []string
}

type funcRunner struct {
	calls  []runCall
	handle func(name string, args []string) ([]byte, error)
}

func (f *funcRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, runCall{name: name, args: append([]string(nil), args...)})
	if f.handle != nil {
		return f.handle(name, args)
	}
	return nil, nil
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func argValue(args []string, key string) string {
	for i, arg := range args {
		if arg == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestSystemTargetInstallWritesAnchorAndRunsUpdate(t *testing.T) {
	dir := t.TempDir()
	cert := newTestCertificate(t, "ca")
	runner := &funcRunner{}

	target := systemTarget{
		anchorPath:        filepath.Join(dir, "anchors", "mitt.crt"),
		osReleasePath:     filepath.Join(dir, "os-release"),
		debianVersionPath: filepath.Join(dir, "debian_version"),
		runner:            runner,
	}
	writeTestFile(t, target.osReleasePath, "ID=debian\n")

	note, err := target.install(cert)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if note != "" {
		t.Errorf("note = %q, want empty", note)
	}

	data, err := os.ReadFile(target.anchorPath)
	if err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	if !bytes.Equal(data, cert.pem) {
		t.Error("anchor content differs from the CA PEM")
	}
	info, err := os.Stat(target.anchorPath)
	if err != nil {
		t.Fatalf("stat anchor: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("anchor mode = %o, want 644", got)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "update-ca-certificates" {
		t.Fatalf("runner calls = %+v, want one update-ca-certificates call", runner.calls)
	}
}

func TestSystemTargetRejectsNonDebian(t *testing.T) {
	dir := t.TempDir()
	runner := &funcRunner{}
	target := systemTarget{
		anchorPath:        filepath.Join(dir, "anchors", "mitt.crt"),
		osReleasePath:     filepath.Join(dir, "os-release"),
		debianVersionPath: filepath.Join(dir, "debian_version"),
		runner:            runner,
	}
	writeTestFile(t, target.osReleasePath, "ID=alpine\nID_LIKE=alpine\n")

	_, err := target.install(newTestCertificate(t, "ca"))
	if err == nil || !strings.Contains(err.Error(), "not Debian/Kali") {
		t.Fatalf("error = %v, want not Debian/Kali", err)
	}
	if _, statErr := os.Stat(target.anchorPath); !os.IsNotExist(statErr) {
		t.Errorf("anchor exists after a rejected install: %v", statErr)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner calls = %+v, want none", runner.calls)
	}
}

func TestSystemTargetFallsBackToDebianVersion(t *testing.T) {
	dir := t.TempDir()
	runner := &funcRunner{}
	target := systemTarget{
		anchorPath:        filepath.Join(dir, "anchors", "mitt.crt"),
		osReleasePath:     filepath.Join(dir, "missing-os-release"),
		debianVersionPath: filepath.Join(dir, "debian_version"),
		runner:            runner,
	}
	writeTestFile(t, target.debianVersionPath, "12.0\n")

	if _, err := target.install(newTestCertificate(t, "ca")); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Errorf("runner calls = %+v, want one update call", runner.calls)
	}
}

func TestSystemTargetReportsUpdateFailure(t *testing.T) {
	dir := t.TempDir()
	cert := newTestCertificate(t, "ca")
	runner := &funcRunner{handle: func(name string, args []string) ([]byte, error) {
		return []byte("certificate update exploded"), errors.New("exit status 1")
	}}
	target := systemTarget{
		anchorPath:        filepath.Join(dir, "anchors", "mitt.crt"),
		osReleasePath:     filepath.Join(dir, "os-release"),
		debianVersionPath: filepath.Join(dir, "debian_version"),
		runner:            runner,
	}
	writeTestFile(t, target.osReleasePath, "ID=kali\n")

	_, err := target.install(cert)
	if err == nil {
		t.Fatal("install succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "update-ca-certificates failed") {
		t.Errorf("error = %v, want update-ca-certificates context", err)
	}
	if !strings.Contains(err.Error(), "certificate update exploded") {
		t.Errorf("error = %v, want the command output tail", err)
	}
	if _, statErr := os.Stat(target.anchorPath); statErr != nil {
		t.Errorf("anchor should remain for debugging: %v", statErr)
	}
}

func TestSystemTargetInstalled(t *testing.T) {
	dir := t.TempDir()
	cert := newTestCertificate(t, "ca")
	other := newTestCertificate(t, "other")
	target := systemTarget{anchorPath: filepath.Join(dir, "mitt.crt")}

	if target.installed(cert) {
		t.Error("missing anchor reported as installed")
	}
	writeTestFile(t, target.anchorPath, string(other.pem))
	if target.installed(cert) {
		t.Error("different anchor reported as installed")
	}
	writeTestFile(t, target.anchorPath, string(cert.pem))
	if !target.installed(cert) {
		t.Error("matching anchor reported as not installed")
	}
}
