package cli_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	kaliDockerfileRel = "scripts/kali/Dockerfile.example"
	kaliReadmeRel     = "scripts/kali/README.md"
)

// runDocker runs one docker subcommand under the container budget, so a wedged
// daemon or a buildkit helper that never returns fails the test instead of
// holding the package open.
func runDocker(t *testing.T, budget time.Duration, args ...string) result {
	t.Helper()
	output, err := runBounded(t, budget, "docker", args...)
	result := result{}
	result.stdout = output
	exit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("docker %v: %v", args, err)
		}
		exit = exitErr.ExitCode()
	}
	result.exit = exit
	return result
}

func runDockerCommand(t *testing.T, args ...string) result {
	t.Helper()
	return runDocker(t, containerBudget, args...)
}

// kaliBaseImage returns a locally available Kali base image. Pulling is left
// to the caller so the test does not silently download a large image.
func kaliBaseImage(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is required for the Kali reference-image acceptance test")
	}
	candidates := make([]string, 0, 2)
	if image := os.Getenv("MITT_KALI_IMAGE"); image != "" {
		candidates = append(candidates, image)
	}
	candidates = append(candidates, "kalilinux/kali-rolling")

	rejected := make([]string, 0, len(candidates))
	for _, image := range candidates {
		output, err := runBounded(t, containerBudget, "docker", "image", "inspect", image)
		if err != nil {
			rejected = append(rejected, image+" ("+strings.TrimSpace(output)+")")
			continue
		}
		return image
	}
	t.Fatalf("no local Kali base image; run `docker pull kalilinux/kali-rolling` or set MITT_KALI_IMAGE. Tried: %s",
		strings.Join(rejected, "; "))
	return ""
}

func TestKaliReferenceDockerfileContract(t *testing.T) {
	dockerfile := readFileString(t, filepath.Join(repoRoot, kaliDockerfileRel))
	for _, want := range []string{
		"ARG BASE_IMAGE=kalilinux/kali-rolling",
		"FROM ${BASE_IMAGE}",
		"apt-get install",
		"ca-certificates",
		"libnss3-tools",
		"COPY mitt /usr/local/bin/mitt",
		"RUN mitt skills install",
		"RUN mitt ca init >/dev/null && mitt ca install",
		"WORKDIR /root",
	} {
		requireContains(t, dockerfile, want)
	}
	if !strings.Contains(strings.ToLower(dockerfile), "not a mitt deliverable") {
		t.Error("Dockerfile.example must state that it is not a mitt deliverable")
	}

	readme := readFileString(t, filepath.Join(repoRoot, kaliReadmeRel))
	for _, want := range []string{
		"docker pull kalilinux/kali-rolling",
		"docker build",
		"docker run",
		"CA private key",
		"Never push",
		"not a deliverable",
	} {
		requireContains(t, readme, want)
	}
}

func TestRepoDoesNotTrackCAPrivateKeys(t *testing.T) {
	out, err := runBounded(t, toolBudget, "git", "-C", repoRoot, "ls-files")
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, path := range strings.Split(out, "\n") {
		if strings.HasSuffix(strings.TrimSpace(path), "ca.key.pem") {
			t.Errorf("CA private key is tracked in git: %s", path)
		}
	}

	ignore := readFileString(t, filepath.Join(repoRoot, ".gitignore"))
	for _, want := range []string{".mitt/", "ca.key.pem"} {
		requireContains(t, ignore, want)
	}
	for _, path := range []string{"ca.key.pem", ".mitt/ca/ca.key.pem"} {
		if _, err := runBounded(t, toolBudget, "git", "-C", repoRoot, "check-ignore", "-q", path); err != nil {
			t.Errorf(".gitignore does not cover %s: %v", path, err)
		}
	}
}

func TestReferenceKaliImageBuildsAndTrustsCA(t *testing.T) {
	base := kaliBaseImage(t)
	dockerfile := filepath.Join(repoRoot, kaliDockerfileRel)
	tag := fmt.Sprintf("mitt-kali-acceptance:%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		// Cleanup must not outlive the test result; the budget bounds a stuck
		// daemon without failing a test that already finished.
		_, _ = runBounded(t, containerBudget, "docker", "image", "rm", "-f", tag)
	})

	build := runDocker(t, imageBuildBudget,
		"build",
		"--build-arg", "BASE_IMAGE="+base,
		"-f", dockerfile,
		"-t", tag,
		repoRoot,
	)
	requireExit(t, build, 0)
	buildLog := build.stdout + build.stderr
	for _, secret := range []string{"BEGIN EC PRIVATE KEY", "ca_key_pem"} {
		if strings.Contains(buildLog, secret) {
			t.Errorf("docker build log leaked CA private key material (%q)", secret)
		}
	}

	which := runDockerCommand(t, "run", "--rm", tag, "sh", "-c", "command -v mitt")
	requireExit(t, which, 0)
	requireStdoutEquals(t, which, "/usr/local/bin/mitt")

	stat := runDockerCommand(t, "run", "--rm", tag, "sh", "-c", "stat -c '%a %s' /usr/local/bin/mitt")
	requireExit(t, stat, 0)
	fields := strings.Fields(stat.stdout)
	if len(fields) != 2 {
		t.Fatalf("stat output = %q, want mode and size", stat.stdout)
	}
	if fields[0] != "755" {
		t.Errorf("installed binary mode = %s, want 755", fields[0])
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("parse binary size %q: %v", fields[1], err)
	}
	if size >= 20*1024*1024 {
		t.Errorf("installed binary size = %d, want < 20 MiB", size)
	}

	for _, path := range []string{
		"/root/.agents/skills/mitt/SKILL.md",
		"/root/.agents/skills/mitt/reference/commands.md",
		"/root/.agents/skills/mitt/reference/workflows.md",
		"/root/.agents/skills/mitt/reference/playwright-integration.md",
	} {
		got := runDockerCommand(t, "run", "--rm", tag, "test", "-f", path)
		if got.exit != 0 {
			t.Errorf("reference image is missing %s", path)
		}
	}

	install := runDockerCommand(t, "run", "--rm", tag, "mitt", "skills", "install")
	requireExit(t, install, 0)
	installOutput := decodeJSONObject(t, install.stdout)
	if installOutput["skills_dir"] != "/root/.agents/skills" {
		t.Errorf("skills_dir = %v, want /root/.agents/skills", installOutput["skills_dir"])
	}
	skills, _ := installOutput["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("skills = %v, want one entry", installOutput["skills"])
	}
	skill, _ := skills[0].(map[string]any)
	if skill["action"] != "unchanged" {
		t.Errorf("action = %v, want unchanged", skill["action"])
	}

	keyMode := runDockerCommand(t, "run", "--rm", tag, "stat", "-c", "%a", "/root/.mitt/ca/ca.key.pem")
	requireExit(t, keyMode, 0)
	requireStdoutEquals(t, keyMode, "600")

	certMode := runDockerCommand(t, "run", "--rm", tag, "stat", "-c", "%a", "/usr/local/share/ca-certificates/mitt.crt")
	requireExit(t, certMode, 0)
	requireStdoutEquals(t, certMode, "644")

	status := runDockerCommand(t, "run", "--rm", tag, "mitt", "ca", "status")
	requireExit(t, status, 0)
	statusOutput := decodeJSONObject(t, status.stdout)
	if statusOutput["ca_exists"] != true {
		t.Errorf("ca status ca_exists = %v, want true", statusOutput["ca_exists"])
	}
	if statusOutput["installed"] != true {
		t.Errorf("ca status installed = %v, want true", statusOutput["installed"])
	}
	if statusOutput["ca_path"] != "/root/.mitt/ca" {
		t.Errorf("ca status ca_path = %v, want /root/.mitt/ca", statusOutput["ca_path"])
	}
	assertTargets(t, statusOutput["install_targets"], []string{"system", "nss"})

	ldd := runDockerCommand(t, "run", "--rm", tag, "ldd", "/usr/local/bin/mitt")
	if ldd.exit == 0 {
		t.Errorf("ldd succeeded for /usr/local/bin/mitt; want a static binary:\n%s", ldd.stdout)
	}
	lddOutput := ldd.stdout + ldd.stderr
	if !strings.Contains(lddOutput, "not a dynamic executable") && !strings.Contains(lddOutput, "statically linked") {
		t.Errorf("unexpected ldd output for static binary:\n%s", lddOutput)
	}
}
