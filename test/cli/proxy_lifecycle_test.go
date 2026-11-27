package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runningProxy is a foreground `mitt proxy start` child used by the lifecycle
// tests. stdout and stderr are only read after done is closed.
type runningProxy struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

func (e *testEnv) startProxy(t *testing.T, args ...string) *runningProxy {
	t.Helper()
	return e.startProxyWithEnv(t, nil, args...)
}

func (e *testEnv) startProxyWithEnv(t *testing.T, extraEnv []string, args ...string) *runningProxy {
	t.Helper()
	full := append([]string{"--store", e.store, "proxy", "start"}, args...)
	// The proxy is long-lived by design, so its budget is the backstop for a
	// cleanup that never runs; t.Cleanup is what normally stops it.
	tc := newTimedCommand(proxyBudget, binaryPath, full...)
	cmd := tc.cmd
	cmd.Env = append(append(os.Environ(), "HOME="+e.home), extraEnv...)
	p := &runningProxy{cmd: cmd, done: make(chan struct{}), cancel: tc.cancel}
	cmd.Stdout = &p.stdout
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		tc.cancel()
		t.Fatalf("start proxy: %v", err)
	}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		tc.cancel()
		if p.exited() {
			return
		}
		_ = cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Errorf("proxy pid %d did not exit after SIGKILL", cmd.Process.Pid)
		}
	})
	return p
}

func (p *runningProxy) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *runningProxy) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
		if p.err == nil {
			return 0
		}
		var exitErr *exec.ExitError
		if errors.As(p.err, &exitErr) {
			return exitErr.ExitCode()
		}
		t.Fatalf("wait for proxy: %v", p.err)
		return -1
	case <-time.After(timeout):
		t.Fatalf("proxy did not exit within %s\nstderr:\n%s", timeout, p.stderr.String())
		return -1
	}
}

func (p *runningProxy) failState(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("stdout:\n%s\nstderr:\n%s", p.stdout.String(), p.stderr.String())
}

// waitReady waits for proxy.json to name the child pid, then confirms the
// public status contract. Polling the file first avoids racing `proxy status`,
// which is allowed to clean a stale record before start replaces it.
func (p *runningProxy) waitReady(t *testing.T, env *testEnv) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-p.done:
			t.Fatalf("proxy exited before becoming ready: %v\n%s", p.err, p.failState(t))
		default:
		}

		if data, err := os.ReadFile(env.proxyStatePath()); err == nil {
			var state map[string]any
			if json.Unmarshal(data, &state) == nil {
				if pid, _ := state["pid"].(float64); int(pid) == p.cmd.Process.Pid {
					got := env.runWithStore(t, "proxy", "status")
					if got.exit == 0 {
						status := decodeJSONObject(t, got.stdout)
						if status["running"] != true {
							t.Fatalf("status.running = %v, want true\n%s", status["running"], got.stdout)
						}
						return status
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy did not become ready within 5s\n%s", p.failState(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *testEnv) readProxyState(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.store, "proxy.json"))
	if err != nil {
		t.Fatalf("read proxy.json: %v", err)
	}
	return decodeJSONObject(t, string(data))
}

func (e *testEnv) proxyStatePath() string {
	return filepath.Join(e.store, "proxy.json")
}

func (e *testEnv) mustInitCA(t *testing.T) {
	t.Helper()
	got := e.runWithStore(t, "ca", "init")
	requireExit(t, got, 0)
}

func caPEMFingerprint(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestProxyStartRequiresExistingCA(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "proxy", "start", "--addr", "127.0.0.1:0")

	requireExit(t, got, 78)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "CA_MISSING" {
		t.Errorf("error.code = %v, want CA_MISSING", errObj["code"])
	}
	if hint, _ := errObj["hint"].(string); !strings.Contains(hint, "mitt ca init") {
		t.Errorf("error.hint = %q, want an actionable ca init hint", hint)
	}
	if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
		t.Fatalf("starter must not write proxy.json when CA is missing, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.store, "ca")); !os.IsNotExist(err) {
		t.Fatalf("start must not create a CA implicitly, stat err = %v", err)
	}
}

func TestProxyStartStatusStopLifecycle(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)
	wantFingerprint := caPEMFingerprint(t, filepath.Join(env.store, "ca", "ca.pem"))

	proxy := env.startProxy(t, "--addr", "127.0.0.1:0")
	status := proxy.waitReady(t, env)

	if got := int(status["pid"].(float64)); got != proxy.cmd.Process.Pid {
		t.Errorf("status.pid = %d, want child pid %d", got, proxy.cmd.Process.Pid)
	}
	if status["ca_fingerprint_sha256"] != wantFingerprint {
		t.Errorf("status.ca_fingerprint_sha256 = %v, want %s", status["ca_fingerprint_sha256"], wantFingerprint)
	}
	startedAt, _ := status["started_at"].(string)
	if _, err := time.Parse(time.RFC3339Nano, startedAt); err != nil {
		t.Errorf("status.started_at = %q, want RFC3339: %v", startedAt, err)
	}
	listeners, ok := status["listeners"].([]any)
	if !ok || len(listeners) != 1 {
		t.Fatalf("status.listeners = %#v, want one listener", status["listeners"])
	}
	listener, _ := listeners[0].(map[string]any)
	addr, _ := listener["addr"].(string)
	if addr == "" || !strings.Contains(addr, "127.0.0.1:") {
		t.Errorf("listener.addr = %q, want the actual bound address", addr)
	}
	if listener["store"] != env.store {
		t.Errorf("listener.store = %v, want %s", listener["store"], env.store)
	}
	if status["addr"] != addr {
		t.Errorf("status.addr = %v, want listener addr %q", status["addr"], addr)
	}

	disk := env.readProxyState(t)
	if got := int(disk["pid"].(float64)); got != proxy.cmd.Process.Pid {
		t.Errorf("proxy.json pid = %d, want %d", got, proxy.cmd.Process.Pid)
	}
	if disk["addr"] != addr {
		t.Errorf("proxy.json addr = %v, want %q", disk["addr"], addr)
	}

	stopped := env.runWithStore(t, "proxy", "stop")
	requireExit(t, stopped, 0)
	stopOutput := decodeJSONObject(t, stopped.stdout)
	if stopOutput["stopped"] != true {
		t.Errorf("stop.stopped = %v, want true", stopOutput["stopped"])
	}
	if got := int(stopOutput["pid"].(float64)); got != proxy.cmd.Process.Pid {
		t.Errorf("stop.pid = %d, want %d", got, proxy.cmd.Process.Pid)
	}

	if exit := proxy.waitExit(t, 5*time.Second); exit != 0 {
		t.Errorf("proxy exit code = %d, want 0\n%s", exit, proxy.failState(t))
	}
	if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
		t.Errorf("proxy.json must be removed on normal exit, stat err = %v", err)
	}
	if proxy.stdout.Len() != 0 {
		t.Errorf("proxy start must not write stdout by default, got:\n%s", proxy.stdout.String())
	}

	after := env.runWithStore(t, "proxy", "status")
	requireExit(t, after, 69)
	if code := errorEnvelope(t, after)["code"]; code != "NOT_RUNNING" {
		t.Errorf("status after stop code = %v, want NOT_RUNNING", code)
	}
}

func TestProxyStartRejectsSecondInstanceAsLocked(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	first := env.startProxy(t, "--addr", "127.0.0.1:0")
	firstStatus := first.waitReady(t, env)

	second := env.runWithStore(t, "proxy", "start", "--addr", "127.0.0.1:0")
	requireExit(t, second, 69)
	errObj := errorEnvelope(t, second)
	if errObj["code"] != "LOCKED" {
		t.Errorf("second start error.code = %v, want LOCKED", errObj["code"])
	}
	if errObj["retryable"] != true {
		t.Errorf("LOCKED retryable = %v, want true", errObj["retryable"])
	}
	if details, _ := errObj["details"].(map[string]any); details["pid"] != firstStatus["pid"] {
		t.Errorf("LOCKED details.pid = %v, want %v", details["pid"], firstStatus["pid"])
	}

	still := env.runWithStore(t, "proxy", "status")
	requireExit(t, still, 0)
	if pid := decodeJSONObject(t, still.stdout)["pid"]; pid != firstStatus["pid"] {
		t.Errorf("first proxy was disturbed: pid = %v, want %v", pid, firstStatus["pid"])
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	if exit := first.waitExit(t, 5*time.Second); exit != 0 {
		t.Errorf("first proxy exit = %d, want 0", exit)
	}
}

func TestProxyStartReturnsAddrInUse(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	defer listener.Close()
	addr := listener.Addr().String()

	got := env.runWithStore(t, "proxy", "start", "--addr", addr)

	requireExit(t, got, 69)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "ADDR_IN_USE" {
		t.Errorf("error.code = %v, want ADDR_IN_USE", errObj["code"])
	}
	if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
		t.Errorf("failed start must not leave proxy.json, stat err = %v", err)
	}
}

func TestProxyStatusCleansStaleState(t *testing.T) {
	env := newTestEnv(t)

	t.Run("dead pid", func(t *testing.T) {
		dead := runToExit(t, toolBudget, "true")
		env.writeProxyState(t, map[string]any{
			"pid":                   dead.Process.Pid,
			"addr":                  "127.0.0.1:8080",
			"started_at":            "2025-01-02T03:04:05Z",
			"ca_fingerprint_sha256": "deadbeef",
			"listeners":             []any{},
		})

		got := env.runWithStore(t, "proxy", "status")
		requireExit(t, got, 69)
		if code := errorEnvelope(t, got)["code"]; code != "NOT_RUNNING" {
			t.Errorf("error.code = %v, want NOT_RUNNING", code)
		}
		if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
			t.Errorf("stale proxy.json must be removed, stat err = %v", err)
		}
	})

	t.Run("cmdline mismatch", func(t *testing.T) {
		env.writeProxyState(t, map[string]any{
			"pid":                   os.Getpid(),
			"addr":                  "127.0.0.1:8080",
			"started_at":            "2025-01-02T03:04:05Z",
			"ca_fingerprint_sha256": "deadbeef",
			"listeners":             []any{},
		})

		got := env.runWithStore(t, "proxy", "status")
		requireExit(t, got, 69)
		if code := errorEnvelope(t, got)["code"]; code != "NOT_RUNNING" {
			t.Errorf("error.code = %v, want NOT_RUNNING", code)
		}
		if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
			t.Errorf("mismatched proxy.json must be removed, stat err = %v", err)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		if err := os.MkdirAll(env.store, 0o700); err != nil {
			t.Fatalf("create store: %v", err)
		}
		if err := os.WriteFile(env.proxyStatePath(), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write invalid proxy.json: %v", err)
		}

		got := env.runWithStore(t, "proxy", "status")
		requireExit(t, got, 69)
		if code := errorEnvelope(t, got)["code"]; code != "NOT_RUNNING" {
			t.Errorf("error.code = %v, want NOT_RUNNING", code)
		}
		if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
			t.Errorf("invalid proxy.json must be removed, stat err = %v", err)
		}
	})
}

func TestProxyStartRecoversStaleLock(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	dead := runToExit(t, toolBudget, "true")
	env.writeProxyState(t, map[string]any{
		"pid":                   dead.Process.Pid,
		"addr":                  "127.0.0.1:8080",
		"started_at":            "2025-01-02T03:04:05Z",
		"ca_fingerprint_sha256": "deadbeef",
		"listeners":             []any{},
	})

	proxy := env.startProxy(t, "--addr", "127.0.0.1:0")
	status := proxy.waitReady(t, env)

	if got := int(status["pid"].(float64)); got != proxy.cmd.Process.Pid {
		t.Errorf("status.pid = %d, want new pid %d", got, proxy.cmd.Process.Pid)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
	if warning := strings.ToLower(proxy.stderr.String()); !strings.Contains(warning, "stale") ||
		!strings.Contains(warning, "warning") {
		t.Errorf("stale lock recovery must warn on stderr, got:\n%s", proxy.stderr.String())
	}
}

func TestProxyStartRecoversCmdlineMismatch(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)
	env.writeProxyState(t, map[string]any{
		"pid":                   os.Getpid(),
		"addr":                  "127.0.0.1:8080",
		"started_at":            "2025-01-02T03:04:05Z",
		"ca_fingerprint_sha256": "deadbeef",
		"listeners":             []any{},
	})

	proxy := env.startProxy(t, "--addr", "127.0.0.1:0")
	status := proxy.waitReady(t, env)

	if got := int(status["pid"].(float64)); got != proxy.cmd.Process.Pid {
		t.Errorf("status.pid = %d, want new pid %d", got, proxy.cmd.Process.Pid)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
	if warning := strings.ToLower(proxy.stderr.String()); !strings.Contains(warning, "warning") {
		t.Errorf("cmdline mismatch recovery must warn on stderr, got:\n%s", proxy.stderr.String())
	}
}

func TestProxySignalsCleanUpState(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  os.Signal
		exit int
	}{
		{name: "SIGTERM", sig: syscall.SIGTERM, exit: 0},
		{name: "SIGINT", sig: os.Interrupt, exit: 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.mustInitCA(t)

			proxy := env.startProxy(t, "--addr", "127.0.0.1:0")
			proxy.waitReady(t, env)

			if err := proxy.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatalf("signal proxy: %v", err)
			}
			if exit := proxy.waitExit(t, 5*time.Second); exit != tc.exit {
				t.Errorf("exit code = %d, want %d\n%s", exit, tc.exit, proxy.failState(t))
			}
			if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
				t.Errorf("proxy.json must be removed after %s, stat err = %v", tc.name, err)
			}
		})
	}
}

func TestProxyStopErrors(t *testing.T) {
	t.Run("not running", func(t *testing.T) {
		env := newTestEnv(t)
		got := env.runWithStore(t, "proxy", "stop")
		requireExit(t, got, 69)
		if code := errorEnvelope(t, got)["code"]; code != "NOT_RUNNING" {
			t.Errorf("error.code = %v, want NOT_RUNNING", code)
		}
	})

	t.Run("dead pid", func(t *testing.T) {
		env := newTestEnv(t)
		dead := runToExit(t, toolBudget, "true")
		env.writeProxyState(t, map[string]any{
			"pid":                   dead.Process.Pid,
			"addr":                  "127.0.0.1:8080",
			"started_at":            "2025-01-02T03:04:05Z",
			"ca_fingerprint_sha256": "deadbeef",
			"listeners":             []any{},
		})

		got := env.runWithStore(t, "proxy", "stop")
		requireExit(t, got, 69)
		if code := errorEnvelope(t, got)["code"]; code != "NOT_RUNNING" {
			t.Errorf("error.code = %v, want NOT_RUNNING", code)
		}
	})

	t.Run("cmdline mismatch", func(t *testing.T) {
		env := newTestEnv(t)
		// The child only has to be alive and to not be `mitt proxy start`; the
		// budget bounds it if cleanup somehow does not.
		tc := newTimedCommand(proxyBudget, "sleep", "30")
		sleeper := tc.cmd
		if err := sleeper.Start(); err != nil {
			tc.cancel()
			t.Fatalf("start sleep: %v", err)
		}
		t.Cleanup(func() {
			tc.cancel()
			_ = sleeper.Process.Kill()
			_ = sleeper.Wait()
		})
		env.writeProxyState(t, map[string]any{
			"pid":                   sleeper.Process.Pid,
			"addr":                  "127.0.0.1:8080",
			"started_at":            "2025-01-02T03:04:05Z",
			"ca_fingerprint_sha256": "deadbeef",
			"listeners":             []any{},
		})

		got := env.runWithStore(t, "proxy", "stop")
		requireExit(t, got, 69)
		errObj := errorEnvelope(t, got)
		if errObj["code"] != "NOT_RUNNING" {
			t.Errorf("error.code = %v, want NOT_RUNNING", errObj["code"])
		}
		details, _ := errObj["details"].(map[string]any)
		if details["pid"] != float64(sleeper.Process.Pid) {
			t.Errorf("details.pid = %v, want %d", details["pid"], sleeper.Process.Pid)
		}
		if details["reason"] != "other_process" {
			t.Errorf("details.reason = %v, want other_process", details["reason"])
		}
		if err := sleeper.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("proxy stop must not signal a mismatched process: %v", err)
		}
		if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
			t.Errorf("stop must clean a record it refuses to use, stat err = %v", err)
		}
	})
}

func TestProxyStartKeepsIPv4WildcardAddress(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	proxy := env.startProxy(t, "--addr", "0.0.0.0:0")
	status := proxy.waitReady(t, env)

	addr, _ := status["addr"].(string)
	if !strings.HasPrefix(addr, "0.0.0.0:") {
		t.Errorf("status.addr = %q, want an IPv4 wildcard address", addr)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
}

func TestProxyStartUsesCAOverride(t *testing.T) {
	env := newTestEnv(t)
	altCA := filepath.Join(t.TempDir(), "alternate-ca")
	got := env.runWithStore(t, "ca", "init", "--dir", altCA)
	requireExit(t, got, 0)
	wantFingerprint := caPEMFingerprint(t, filepath.Join(altCA, "ca.pem"))

	proxy := env.startProxy(t, "--addr", "127.0.0.1:0", "--ca", altCA)
	status := proxy.waitReady(t, env)

	if status["ca_fingerprint_sha256"] != wantFingerprint {
		t.Errorf("ca_fingerprint_sha256 = %v, want %s", status["ca_fingerprint_sha256"], wantFingerprint)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
}

func TestProxyCommandsAreRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	for _, path := range []string{"mitt proxy start", "mitt proxy status", "mitt proxy stop"} {
		if _, ok := doc.Command.find(path); !ok {
			t.Errorf("spec missing %q", path)
		}
	}

	start, ok := doc.Command.find("mitt proxy start")
	if !ok {
		t.Fatal("spec missing mitt proxy start")
	}
	for _, code := range []string{"CA_MISSING", "LOCKED", "ADDR_IN_USE"} {
		if !containsString(start.ErrorCodes, code) {
			t.Errorf("proxy start error_codes = %v, want %s", start.ErrorCodes, code)
		}
	}
	addr, ok := flagByName(start.Flags, "addr")
	if !ok {
		t.Fatal("proxy start missing --addr")
	}
	if addr.Default != "0.0.0.0:8080" {
		t.Errorf("--addr default = %q, want 0.0.0.0:8080", addr.Default)
	}
	if _, ok := flagByName(start.Flags, "ca"); !ok {
		t.Error("proxy start missing --ca")
	}

	stop, ok := doc.Command.find("mitt proxy stop")
	if !ok {
		t.Fatal("spec missing mitt proxy stop")
	}
	if !containsString(stop.ErrorCodes, "NOT_RUNNING") {
		t.Errorf("proxy stop error_codes = %v, want NOT_RUNNING", stop.ErrorCodes)
	}
	if stop.OutputSchema == nil {
		t.Error("proxy stop must register a success output schema")
	}
}

func TestProxyStopReportsStaleProcessAsNotRunning(t *testing.T) {
	env := newTestEnv(t)
	dead := runToExit(t, toolBudget, "true")
	env.writeProxyState(t, map[string]any{
		"pid":                   dead.Process.Pid,
		"addr":                  "127.0.0.1:8080",
		"started_at":            "2025-01-02T03:04:05Z",
		"ca_fingerprint_sha256": "deadbeef",
		"listeners":             []any{},
	})

	got := env.runWithStore(t, "proxy", "stop")

	requireExit(t, got, 69)
	if code := errorEnvelope(t, got)["code"]; code != "NOT_RUNNING" {
		t.Errorf("error.code = %v, want NOT_RUNNING", code)
	}
}

// TestProxyStatusAndStopAgreeOnEveryState pins the report's complaint shut: the
// same proxy.json condition used to yield NOT_RUNNING (69) from status and
// INTERNAL_ERROR (1) from stop. Both commands now read one classified value and
// share one mapping, so every state produces one code and one exit code.
func TestProxyStatusAndStopAgreeOnEveryState(t *testing.T) {
	deadPID := func(t *testing.T) int {
		t.Helper()
		dead := runToExit(t, toolBudget, "true")
		return dead.Process.Pid
	}

	for _, tc := range []struct {
		name   string
		state  map[string]any
		reason string
		pid    bool
	}{
		{
			name:   "missing",
			reason: "missing",
		},
		{
			name: "invalid",
			// Written directly below so the bytes are not valid JSON.
			reason: "invalid",
		},
		{
			name: "exited",
			state: map[string]any{
				"pid":                   deadPID(t),
				"addr":                  "127.0.0.1:8080",
				"started_at":            "2025-01-02T03:04:05Z",
				"ca_fingerprint_sha256": "deadbeef",
				"listeners":             []any{},
			},
			reason: "exited",
			pid:    true,
		},
		{
			name: "other process",
			state: map[string]any{
				"pid":                   os.Getpid(),
				"addr":                  "127.0.0.1:8080",
				"started_at":            "2025-01-02T03:04:05Z",
				"ca_fingerprint_sha256": "deadbeef",
				"listeners":             []any{},
			},
			reason: "other_process",
			pid:    true,
		},
	} {
		for _, command := range []string{"status", "stop"} {
			t.Run(tc.name+"/"+command, func(t *testing.T) {
				env := newTestEnv(t)
				switch {
				case tc.name == "invalid":
					if err := os.MkdirAll(env.store, 0o700); err != nil {
						t.Fatalf("create store: %v", err)
					}
					if err := os.WriteFile(env.proxyStatePath(), []byte("{not json"), 0o600); err != nil {
						t.Fatalf("write invalid proxy.json: %v", err)
					}
				case tc.state != nil:
					env.writeProxyState(t, tc.state)
				}

				got := env.runWithStore(t, "proxy", command)

				requireExit(t, got, 69)
				errObj := errorEnvelope(t, got)
				if errObj["code"] != "NOT_RUNNING" {
					t.Errorf("error.code = %v, want NOT_RUNNING", errObj["code"])
				}
				details, _ := errObj["details"].(map[string]any)
				if details["reason"] != tc.reason {
					t.Errorf("details.reason = %v, want %s", details["reason"], tc.reason)
				}
				if tc.pid && details["pid"] == nil {
					t.Errorf("details.pid missing, want the recorded pid")
				}
				if tc.reason != "missing" {
					if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
						t.Errorf("a stale record must be cleaned by %s, stat err = %v", command, err)
					}
				}
			})
		}
	}
}

// TestProxyStopImmediatelyAfterReady pins the report's readiness case: the
// instant proxy.json is visible through `proxy status`, a stop must still run
// the whole teardown — signal handled, state removed, ports rebound, lock
// retaken. Start registers its signal handler before it acquires anything, so
// a stop that races the last step of startup cannot hit the default
// disposition and leave a bound port or a record behind.
func TestProxyStopImmediatelyAfterReady(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	proxy := env.startProxy(t, "--addr", "127.0.0.1:0")
	status := proxy.waitReady(t, env)
	addr, _ := status["addr"].(string)
	if addr == "" {
		t.Fatalf("status.addr is missing: %v", status)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	if exit := proxy.waitExit(t, 5*time.Second); exit != 0 {
		t.Errorf("proxy exit code = %d, want 0\n%s", exit, proxy.failState(t))
	}
	if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
		t.Errorf("proxy.json must be removed after an immediate stop, stat err = %v", err)
	}
	assertPortFree(t, addr)

	// The lock and the record are both free again: a second run can start and
	// become ready on the same store.
	second := env.startProxy(t, "--addr", "127.0.0.1:0")
	secondStatus := second.waitReady(t, env)
	secondAddr, _ := secondStatus["addr"].(string)
	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	if exit := second.waitExit(t, 5*time.Second); exit != 0 {
		t.Errorf("second proxy exit code = %d, want 0\n%s", exit, second.failState(t))
	}
	assertPortFree(t, secondAddr)
}

// TestProxyRestartsAfterFailedStartGivesPortsBack covers the other half of the
// acceptance: a start that fails after binding some endpoints must not keep
// them. The failed child has exited, so the next start can rebind the same
// ports and take the lock; the failure must also leave no proxy.json.
func TestProxyRestartsAfterFailedStartGivesPortsBack(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	start, blocked := blockedSecondPort(t)
	blockedAddr := fmt.Sprintf("127.0.0.1:%d", start+1)
	firstAddr := fmt.Sprintf("127.0.0.1:%d", start)
	got := env.runWithStore(t, "proxy", "start",
		"--addr", "127.0.0.1:0",
		"--port-range", fmt.Sprintf("%d-%d", start, start+1),
	)
	requireExit(t, got, 69)
	if code := errorEnvelope(t, got)["code"]; code != "ADDR_IN_USE" {
		t.Errorf("error.code = %v, want ADDR_IN_USE", code)
	}
	if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
		t.Errorf("failed start must not leave proxy.json, stat err = %v", err)
	}
	_ = blocked.Close()

	// The port the failed run had already bound is bindable again, and a run
	// over the same range can publish state because the lock came back too.
	assertPortFree(t, firstAddr)
	proxy := env.startProxy(t,
		"--addr", "127.0.0.1:0",
		"--port-range", fmt.Sprintf("%d-%d", start, start+1),
	)
	status := proxy.waitReady(t, env)
	listeners := statusListeners(t, status)
	requireListenerPorts(t, listeners, []string{firstAddr, blockedAddr})

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	if exit := proxy.waitExit(t, 5*time.Second); exit != 0 {
		t.Errorf("proxy exit code = %d, want 0\n%s", exit, proxy.failState(t))
	}
	for _, addr := range []string{firstAddr, blockedAddr} {
		assertPortFree(t, addr)
	}
}
