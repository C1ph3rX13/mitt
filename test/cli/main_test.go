package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var (
	repoRoot   string
	binaryPath string
)

func TestMain(m *testing.M) {
	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate repository:", err)
		os.Exit(1)
	}
	repoRoot = root

	// The build is the gate every test in this package depends on, so it gets a
	// deadline too: a wedged `go build` must fail the package, not hang it.
	build := newTimedCommand(toolBudget, "make", "build")
	build.cmd.Dir = repoRoot
	if out, err := build.run(); err != nil {
		fmt.Fprintf(os.Stderr, "make build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	binaryPath = filepath.Join(repoRoot, "mitt")
	os.Exit(m.Run())
}

func findRepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found")
		}
		dir = parent
	}
}

type testEnv struct {
	home  string
	store string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create isolated HOME: %v", err)
	}
	return &testEnv{home: home, store: filepath.Join(dir, "store")}
}

type result struct {
	stdout string
	stderr string
	exit   int
}

// runMitt is the one place a `mitt` invocation is built. Every helper below
// goes through it, so a bounded deadline, an isolated environment, and the
// exit-code decoding cannot drift between them.
func runMitt(t *testing.T, env []string, stdin io.Reader, args ...string) result {
	t.Helper()
	cmd := newTimedCommand(binaryBudget, binaryPath, args...)
	cmd.cmd.Env = env
	cmd.cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.cmd.Stdout = &stdout
	cmd.cmd.Stderr = &stderr
	if err := cmd.start(); err != nil {
		cmd.cancel()
		t.Fatalf("run mitt %v: %v", args, err)
	}
	err := cmd.wait()
	exit := 0
	if err != nil {
		if exceededBudget(cmd.cmd) {
			t.Fatalf("mitt %v exceeded its %s budget (killed)\nstdout:\n%s\nstderr:\n%s",
				args, binaryBudget, stdout.String(), stderr.String())
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run mitt %v: %v", args, err)
		}
		exit = exitErr.ExitCode()
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), exit: exit}
}

func (e *testEnv) run(t *testing.T, args ...string) result {
	t.Helper()
	return runMitt(t, append(os.Environ(), "HOME="+e.home), nil, args...)
}

func (e *testEnv) runWithStore(t *testing.T, args ...string) result {
	t.Helper()
	return e.run(t, append([]string{"--store", e.store}, args...)...)
}

// runWithStdin runs mitt against the test store with stdin attached, which
// exercises the `--patch -` input source.
func (e *testEnv) runWithStdin(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return runMitt(t, append(os.Environ(), "HOME="+e.home), strings.NewReader(stdin),
		append([]string{"--store", e.store}, args...)...)
}

// runWithEnv runs mitt with HOME isolated and the given extra environment
// entries taking precedence over the inherited environment.
func (e *testEnv) runWithEnv(t *testing.T, extra []string, args ...string) result {
	t.Helper()
	env := []string{"HOME=" + e.home}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "HOME" || envOverridden(extra, key) {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, extra...)
	return runMitt(t, env, nil, args...)
}

func envOverridden(extra []string, key string) bool {
	for _, entry := range extra {
		name, _, _ := strings.Cut(entry, "=")
		if name == key {
			return true
		}
	}
	return false
}

func (e *testEnv) writeProxyState(t *testing.T, state map[string]any) {
	t.Helper()
	if err := os.MkdirAll(e.store, 0o700); err != nil {
		t.Fatalf("create store: %v", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal proxy state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(e.store, "proxy.json"), data, 0o600); err != nil {
		t.Fatalf("write proxy state: %v", err)
	}
}

func requireExit(t *testing.T, got result, want int) {
	t.Helper()
	if got.exit != want {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", got.exit, want, got.stdout, got.stderr)
	}
}

func decodeJSONObject(t *testing.T, data string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(data))
	var value map[string]any
	if err := dec.Decode(&value); err != nil {
		t.Fatalf("decode JSON object: %v\ninput:\n%s", err, data)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("expected exactly one JSON value, got trailing data:\n%s", data)
	}
	return value
}

func errorEnvelope(t *testing.T, got result) map[string]any {
	t.Helper()
	if strings.TrimSpace(got.stdout) != "" {
		t.Fatalf("stdout must not contain data on error, got:\n%s", got.stdout)
	}
	if strings.TrimSpace(got.stderr) == "" {
		t.Fatal("stderr must contain the error object")
	}
	envelope := decodeJSONObject(t, got.stderr)
	errObj, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("stderr is not an {error: ...} envelope:\n%s", got.stderr)
	}
	for _, key := range []string{"code", "message", "hint", "retryable", "details"} {
		if _, ok := errObj[key]; !ok {
			t.Errorf("error object missing %q: %v", key, errObj)
		}
	}
	if _, ok := errObj["details"].(map[string]any); !ok {
		t.Errorf("error.details must be an object, got %T", errObj["details"])
	}
	return errObj
}
