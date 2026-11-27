package cli_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// This file exercises the parts of `ca install` that genuinely mutate a root
// trust store. They run inside throwaway Debian/Kali containers so the host
// trust store is never touched. A missing Docker daemon or usable image is a
// hard failure, not a silent skip: the system trust-store contract is part of
// the acceptance surface.
//
// Set MITT_TEST_IMAGE to a Debian/Kali image that ships update-ca-certificates
// when the default candidates are not present.

const defaultTestImage = "aether-agent:latest"

var (
	testImageOnce sync.Once
	testImage     string
	testImageErr  string
	containerSeq  atomic.Int64
)

func testContainerImage(t *testing.T) string {
	t.Helper()
	testImageOnce.Do(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			testImageErr = "docker is required for the CA system trust-store tests: install Docker and retry"
			return
		}
		candidates := []string{}
		if image := os.Getenv("MITT_TEST_IMAGE"); image != "" {
			candidates = append(candidates, image)
		}
		candidates = append(candidates, defaultTestImage, "usestrix/strix-sandbox:latest")

		rejected := make([]string, 0, len(candidates))
		for _, image := range candidates {
			if output, err := runBounded(t, containerBudget, "docker", "image", "inspect", image); err != nil {
				rejected = append(rejected, image+" (not present: "+strings.TrimSpace(output)+")")
				continue
			}
			output, err := runBounded(t, containerBudget, "docker", "run", "--rm", "--entrypoint", "sh", image, "-c",
				"command -v update-ca-certificates >/dev/null && command -v sleep >/dev/null")
			if err != nil {
				rejected = append(rejected, image+" (no update-ca-certificates/sleep: "+strings.TrimSpace(output)+")")
				continue
			}
			testImage = image
			return
		}
		testImageErr = "no usable Debian/Kali container image for the CA system trust-store tests; " +
			"set MITT_TEST_IMAGE to an image that ships update-ca-certificates. Tried: " + strings.Join(rejected, "; ")
	})
	if testImageErr != "" {
		t.Fatal(testImageErr)
	}
	return testImage
}

type dockerHarness struct {
	t      *testing.T
	name   string
	work   string
	image  string
	target string
}

func newDockerHarness(t *testing.T, runArgs ...string) *dockerHarness {
	t.Helper()
	image := testContainerImage(t)
	name := fmt.Sprintf("mitt-ca-test-%d-%d", os.Getpid(), containerSeq.Add(1))
	work := t.TempDir()

	args := []string{
		"run", "-d", "--name", name,
		"--user", "0:0",
		"--entrypoint", "sleep",
		"-v", binaryPath + ":/mitt:ro",
		"-v", work + ":/work",
	}
	args = append(args, runArgs...)
	args = append(args, image, "infinity")
	if output, err := runBounded(t, containerBudget, "docker", args...); err != nil {
		t.Fatalf("start test container from %s: %v\n%s", image, err, output)
	}
	h := &dockerHarness{t: t, name: name, work: work, image: image}
	t.Cleanup(func() {
		_, _ = runBounded(t, containerBudget, "docker", "rm", "-f", name)
	})
	return h
}

func (h *dockerHarness) run(args ...string) result {
	h.t.Helper()
	return h.runEnv(nil, args...)
}

func (h *dockerHarness) runEnv(extra []string, args ...string) result {
	h.t.Helper()
	cmdArgs := []string{"exec", "-e", "HOME=/root"}
	for _, entry := range extra {
		cmdArgs = append(cmdArgs, "-e", entry)
	}
	cmdArgs = append(cmdArgs, h.name, "/mitt")
	cmdArgs = append(cmdArgs, args...)
	return h.exec(cmdArgs...)
}

func (h *dockerHarness) sh(script string) result {
	h.t.Helper()
	return h.exec("exec", "-e", "HOME=/root", h.name, "sh", "-c", script)
}

func (h *dockerHarness) exec(args ...string) result {
	h.t.Helper()
	tc := newTimedCommand(containerBudget, "docker", args...)
	var stdout, stderr bytes.Buffer
	tc.cmd.Stdout = &stdout
	tc.cmd.Stderr = &stderr
	if err := tc.start(); err != nil {
		tc.cancel()
		h.t.Fatalf("docker %v: %v", args, err)
	}
	err := tc.wait()
	exit := 0
	if err != nil {
		if exceededBudget(tc.cmd) {
			h.t.Fatalf("docker %v exceeded its %s budget (killed)\nstdout:\n%s\nstderr:\n%s",
				args, containerBudget, stdout.String(), stderr.String())
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			h.t.Fatalf("docker %v: %v", args, err)
		}
		exit = exitErr.ExitCode()
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), exit: exit}
}

func (h *dockerHarness) readWork(t *testing.T, name string) string {
	t.Helper()
	return readFileString(t, filepath.Join(h.work, name))
}

func requireStdoutEquals(t *testing.T, got result, want string) {
	t.Helper()
	if strings.TrimSpace(got.stdout) != want {
		t.Fatalf("stdout = %q, want %q\nstderr:\n%s", strings.TrimSpace(got.stdout), want, got.stderr)
	}
}

func TestCAInstallSystemTrustStoreInThrowawayContainer(t *testing.T) {
	h := newDockerHarness(t)

	initRes := h.run("ca", "init")
	requireExit(t, initRes, 0)
	caPEM := h.sh("cat /root/.mitt/ca/ca.pem")
	requireExit(t, caPEM, 0)
	caHash := sha256Hex(caPEM.stdout)

	installed := h.run("ca", "install")
	requireExit(t, installed, 0)
	output := decodeJSONObject(t, installed.stdout)
	assertTargets(t, output["installed_to"], []string{"system"})
	if output["output_path"] != "" {
		t.Errorf("output_path = %v, want empty for a system install", output["output_path"])
	}
	if output["sha256"] != caHash {
		t.Errorf("sha256 = %v, want %v", output["sha256"], caHash)
	}

	mode := h.sh("stat -c %a /usr/local/share/ca-certificates/mitt.crt")
	requireExit(t, mode, 0)
	requireStdoutEquals(t, mode, "644")
	installedHash := h.sh("sha256sum /usr/local/share/ca-certificates/mitt.crt | cut -d' ' -f1")
	requireExit(t, installedHash, 0)
	requireStdoutEquals(t, installedHash, caHash)
	symlink := h.sh("test -L /etc/ssl/certs/mitt.pem && echo yes")
	requireExit(t, symlink, 0)
	requireStdoutEquals(t, symlink, "yes")

	second := h.run("ca", "install")
	requireExit(t, second, 0)
	output = decodeJSONObject(t, second.stdout)
	assertTargets(t, output["installed_to"], []string{"system"})
	count := h.sh("ls /etc/ssl/certs | grep -c '^mitt' || true")
	requireExit(t, count, 0)
	requireStdoutEquals(t, count, "1")

	status := h.run("ca", "status")
	requireExit(t, status, 0)
	statusOutput := decodeJSONObject(t, status.stdout)
	if statusOutput["installed"] != true {
		t.Errorf("installed = %v, want true", statusOutput["installed"])
	}
	assertTargets(t, statusOutput["install_targets"], []string{"system"})

	requireExit(t, h.sh("printf 'tampered\\n' > /usr/local/share/ca-certificates/mitt.crt"), 0)
	tampered := h.run("ca", "status")
	requireExit(t, tampered, 0)
	tamperedOutput := decodeJSONObject(t, tampered.stdout)
	if tamperedOutput["installed"] != false {
		t.Errorf("installed = %v, want false after the system file changed", tamperedOutput["installed"])
	}
	assertTargets(t, tamperedOutput["install_targets"], []string{})
}
