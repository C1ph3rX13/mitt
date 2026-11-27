package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const maxBinarySize = 20 * 1024 * 1024

func TestMakeBuildProducesStaticBinaryUnder20MB(t *testing.T) {
	tc := newTimedCommand(toolBudget, "make", "build")
	tc.cmd.Dir = repoRoot
	if out, err := tc.run(); err != nil {
		t.Fatalf("make build failed: %v\n%s", err, out)
	}

	info, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatalf("stat built binary: %v", err)
	}
	if info.Size() >= maxBinarySize {
		t.Errorf("binary size = %d bytes, want < %d", info.Size(), maxBinarySize)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("binary mode = %v, want executable", info.Mode())
	}

	fileOut, err := runBounded(t, toolBudget, "file", binaryPath)
	if err != nil {
		t.Fatalf("file: %v\n%s", err, fileOut)
	}
	if !strings.Contains(fileOut, "ELF 64-bit") || !strings.Contains(fileOut, "x86-64") {
		t.Errorf("unexpected file output: %s", fileOut)
	}
	if !strings.Contains(fileOut, "statically linked") {
		t.Errorf("binary is not statically linked: %s", fileOut)
	}

	// ldd is expected to fail on a static binary, so its exit status is the
	// assertion and the output is only there for the message.
	lddOut, lddErr := runBounded(t, toolBudget, "ldd", binaryPath)
	if lddErr == nil {
		t.Errorf("ldd succeeded, binary has dynamic dependencies:\n%s", lddOut)
	}
	if !strings.Contains(lddOut, "not a dynamic executable") && !strings.Contains(lddOut, "statically linked") {
		t.Errorf("unexpected ldd output:\n%s", lddOut)
	}
}

func TestInstallScriptInstallsExecutableOnPath(t *testing.T) {
	destdir := t.TempDir()

	tc := newTimedCommand(toolBudget, "sh", "./install.sh")
	tc.cmd.Dir = repoRoot
	tc.cmd.Env = append(os.Environ(), "DESTDIR="+destdir)
	if out, err := tc.run(); err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}

	installed := filepath.Join(destdir, "usr", "local", "bin", "mitt")
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("installed mode = %04o, want 0755", got)
	}

	which := newTimedCommand(toolBudget, "sh", "-c", "command -v mitt")
	which.cmd.Env = append(os.Environ(), "PATH="+filepath.Join(destdir, "usr", "local", "bin")+":/usr/bin:/bin")
	out, err := which.run()
	if err != nil {
		t.Fatalf("which mitt failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != installed {
		t.Errorf("which mitt = %q, want %q", got, installed)
	}
}
