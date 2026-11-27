//go:build linux

package proxy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// acceptAll is the identity adapter for tests that want liveness, not argv, to
// decide the outcome.
func acceptAll(int) bool { return true }

func writeTestProxyState(t *testing.T, store *session.Store, pid int) {
	t.Helper()
	if err := store.WriteProxyState(&session.ProxyState{
		PID:                 pid,
		Addr:                "127.0.0.1:8080",
		StartedAt:           "2025-01-02T03:04:05Z",
		CAFingerprintSHA256: "deadbeef",
	}); err != nil {
		t.Fatalf("write proxy state: %v", err)
	}
}

// TestReasonTableCoversEveryReason is the completeness check the old pair of
// switches never had: a new reason cannot exist without a token, prose, hint,
// and cleanup policy.
func TestReasonTableCoversEveryReason(t *testing.T) {
	codes := map[string]bool{}
	for reason := notRunningReason(0); reason < notRunningReasonCount; reason++ {
		row, ok := reasonTable[reason]
		if !ok {
			t.Errorf("reason %d has no table row", reason)
			continue
		}
		if row.code == "" || row.text == "" || row.hint == "" {
			t.Errorf("reason %d has an incomplete row: %+v", reason, row)
		}
		if codes[row.code] {
			t.Errorf("reason %d reuses token %q", reason, row.code)
		}
		codes[row.code] = true
	}
}

func TestReadRunStateMissing(t *testing.T) {
	store := session.Open(t.TempDir())

	state, err := readRunState(store, acceptAll)
	if err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if state.Kind() != RunNotRunning || state.Reason() != "missing" {
		t.Fatalf("state = %v/%s, want not running/missing", state.Kind(), state.Reason())
	}
	if state.Stale() {
		t.Error("a missing record must not be treated as stale; there is nothing to remove")
	}
	if _, err := os.Stat(store.ProxyStatePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reading must not create proxy.json, stat err = %v", err)
	}
}

func TestReadRunStateInvalid(t *testing.T) {
	store := session.Open(t.TempDir())
	if err := os.MkdirAll(store.Dir(), 0o700); err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := os.WriteFile(store.ProxyStatePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write invalid state: %v", err)
	}

	state, err := readRunState(store, acceptAll)
	if err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if state.Reason() != "invalid" || !state.Stale() {
		t.Errorf("state = %s stale=%t, want invalid stale", state.Reason(), state.Stale())
	}
}

func TestReadRunStateExited(t *testing.T) {
	store := session.Open(t.TempDir())
	dead, cancel := newTestProcess("true")
	defer cancel()
	if err := dead.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	writeTestProxyState(t, store, dead.Process.Pid)

	state, err := readRunState(store, acceptAll)
	if err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if state.Reason() != "exited" || !state.Stale() {
		t.Errorf("state = %s stale=%t, want exited stale", state.Reason(), state.Stale())
	}
	if state.PID() != dead.Process.Pid {
		t.Errorf("PID() = %d, want the recorded pid %d", state.PID(), dead.Process.Pid)
	}
	if got := state.Summary(); !strings.Contains(got, "no longer running") {
		t.Errorf("Summary() = %q, want the shared sentence", got)
	}
}

func TestReadRunStateOtherProcess(t *testing.T) {
	store := session.Open(t.TempDir())
	sleeper, cancel := newTestProcess("sleep", "30")
	if err := sleeper.Start(); err != nil {
		cancel()
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})
	writeTestProxyState(t, store, sleeper.Process.Pid)

	state, err := readRunState(store, func(int) bool { return false })
	if err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if state.Reason() != "other_process" || !state.Stale() {
		t.Errorf("state = %s stale=%t, want other_process stale", state.Reason(), state.Stale())
	}
}

// TestReadRunStateNilIdentityTrustsNothing pins the safe default: a caller that
// cannot identify processes never turns a live pid into Running.
func TestReadRunStateNilIdentityTrustsNothing(t *testing.T) {
	store := session.Open(t.TempDir())
	writeTestProxyState(t, store, os.Getpid())

	state, err := readRunState(store, nil)
	if err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if state.Running() {
		t.Fatal("a nil identity must not report a running proxy")
	}
	if state.Reason() != "other_process" {
		t.Errorf("reason = %s, want other_process", state.Reason())
	}
}

func TestReadRunStateRunning(t *testing.T) {
	store := session.Open(t.TempDir())
	writeTestProxyState(t, store, os.Getpid())

	state, err := readRunState(store, acceptAll)
	if err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if !state.Running() {
		t.Fatalf("state = %v, want running", state.Kind())
	}
	if state.Reason() != "" || state.Hint() != "" {
		t.Errorf("running state must have no reason or hint, got %q / %q", state.Reason(), state.Hint())
	}
	if state.State() == nil || state.State().Addr != "127.0.0.1:8080" {
		t.Errorf("running state must carry the decoded record, got %+v", state.State())
	}
}

// TestStatusCleansStaleRecordOnce proves the cleanup policy is a property of
// the value: one stale read removes the file, and the second read is missing.
func TestStatusCleansStaleRecordOnce(t *testing.T) {
	store := session.Open(t.TempDir())
	dead, cancel := newTestProcess("true")
	defer cancel()
	if err := dead.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	writeTestProxyState(t, store, dead.Process.Pid)

	first, err := Status(store, acceptAll)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if first.Reason() != "exited" {
		t.Fatalf("first status reason = %s, want exited", first.Reason())
	}
	if _, err := os.Stat(store.ProxyStatePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale record must be removed, stat err = %v", err)
	}

	second, err := Status(store, acceptAll)
	if err != nil {
		t.Fatalf("second status: %v", err)
	}
	if second.Reason() != "missing" {
		t.Errorf("second status reason = %s, want missing", second.Reason())
	}
}

// TestStatusLeavesStateAloneWhileLocked proves the lock decides ownership: a
// stale-looking record is not deleted while another process owns the store,
// because that owner may be between publishing state and serving requests.
func TestStatusLeavesStateAloneWhileLocked(t *testing.T) {
	store := session.Open(t.TempDir())
	dead, cancel := newTestProcess("true")
	defer cancel()
	if err := dead.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	writeTestProxyState(t, store, dead.Process.Pid)

	lock, err := store.AcquireLock()
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	state, err := Status(store, acceptAll)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if state.Reason() != "exited" {
		t.Errorf("reason = %s, want exited", state.Reason())
	}
	if _, err := os.Stat(store.ProxyStatePath()); err != nil {
		t.Errorf("a locked store's record must not be removed: %v", err)
	}
}

func TestLockedRunStateKeepsRecordForDiagnostics(t *testing.T) {
	store := session.Open(t.TempDir())
	writeTestProxyState(t, store, 4242)

	state := lockedRunState(store)
	if state.Kind() != RunLocked {
		t.Fatalf("state = %v, want locked", state.Kind())
	}
	if state.PID() != 4242 || state.Addr() != "127.0.0.1:8080" {
		t.Errorf("locked state = pid %d addr %q, want the recorded record", state.PID(), state.Addr())
	}
}

func TestWarnStaleNamesPidAndReason(t *testing.T) {
	state := notRunning(reasonOtherProcess, &session.ProxyState{PID: 4242})
	var buf bytes.Buffer

	warnStale(&buf, filepath.Join("/store", "proxy.json"), state)

	warning := buf.String()
	for _, want := range []string{"warning", "proxy.json", "pid 4242", "different process"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning %q must contain %q", warning, want)
		}
	}
}

// TestProcStateReadsTheLetterAfterTheComm keeps the zombie check tied to the
// field it actually reads; a comm name with spaces or parentheses must not
// shift it.
func TestProcStateReadsTheLetterAfterTheComm(t *testing.T) {
	for _, tc := range []struct {
		name string
		stat string
		want byte
		ok   bool
	}{
		{name: "sleeping", stat: "1234 (mitt) S 1 1234 1234 0 -1", want: 'S', ok: true},
		{name: "zombie", stat: "1234 (mitt proxy) Z 1 1234 1234 0 -1", want: 'Z', ok: true},
		{name: "comm with parens", stat: "1234 (a(b)c) R 1 1234", want: 'R', ok: true},
		{name: "no comm", stat: "garbage", ok: false},
		{name: "truncated", stat: "1234 (mitt)", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := procState(tc.stat)
			if ok != tc.ok {
				t.Fatalf("procState(%q) ok = %t, want %t", tc.stat, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("procState(%q) = %q, want %q", tc.stat, got, tc.want)
			}
		})
	}
}
