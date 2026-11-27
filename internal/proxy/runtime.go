//go:build linux

package proxy

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// ProcessIdentity reports whether pid is a `mitt proxy start` process. The CLI
// supplies it: only the CLI owns the command-line grammar that makes a process
// a proxy, so this package never parses flags. A nil identity trusts no pid,
// which keeps `proxy stop` from signalling a process it cannot identify.
type ProcessIdentity func(pid int) bool

// RunKind is what the data directory says about a proxy.
type RunKind int

const (
	// RunNotRunning is the zero value: no proxy owns the store. Reason says why.
	RunNotRunning RunKind = iota
	// RunRunning: proxy.json names a live process that identity accepts.
	RunRunning
	// RunLocked: another process holds the data-directory lock.
	RunLocked
)

// notRunningReason is why no proxy owns the store. It is unexported on purpose:
// callers act on Reason(), the stable token, instead of re-deriving policy from
// a second enum.
type notRunningReason int

const (
	// reasonMissing: there is no proxy.json.
	reasonMissing notRunningReason = iota
	// reasonInvalid: proxy.json cannot describe a process.
	reasonInvalid
	// reasonExited: the recorded pid is no longer alive.
	reasonExited
	// reasonOtherProcess: the recorded pid is alive but is not a proxy.
	reasonOtherProcess

	notRunningReasonCount
)

// reasonRow is the whole policy for one not-running reason: the stable token
// callers see, the single human sentence every diagnostic uses, the hint the
// command contract offers, and whether the recorded file must be removed
// before a new run can publish.
type reasonRow struct {
	code  string
	text  string
	hint  string
	stale bool
}

var reasonTable = map[notRunningReason]reasonRow{
	reasonMissing: {
		code: "missing",
		text: "proxy.json does not exist",
		hint: "start it with `mitt proxy start`",
	},
	reasonInvalid: {
		code:  "invalid",
		text:  "proxy.json cannot be parsed",
		hint:  "start it with `mitt proxy start`; the unreadable record is replaced",
		stale: true,
	},
	reasonExited: {
		code:  "exited",
		text:  "the recorded process is no longer running",
		hint:  "start it with `mitt proxy start`; the stale record is replaced",
		stale: true,
	},
	reasonOtherProcess: {
		code:  "other_process",
		text:  "the recorded pid belongs to a different process",
		hint:  "start it with `mitt proxy start`; mitt never signals a process it cannot identify",
		stale: true,
	},
}

// RunState is one classified answer to "is a proxy running for this data
// directory?". It is a value: it carries the decoded record when there is one
// and never performs I/O itself. Running, Locked, and NotRunning are the three
// outcomes a caller can act on; a NotRunning state always carries a reason.
type RunState struct {
	kind   RunKind
	state  *session.ProxyState
	reason notRunningReason
}

// Kind reports the classified outcome.
func (s RunState) Kind() RunKind { return s.kind }

// Running reports whether a live, identity-confirmed proxy owns the store.
func (s RunState) Running() bool { return s.kind == RunRunning }

// NotRunning reports whether no proxy owns the store.
func (s RunState) NotRunning() bool { return s.kind == RunNotRunning }

// Stale reports whether the decoded record must be removed before a new proxy
// can publish its own state. A missing record is not stale: there is nothing
// to remove.
func (s RunState) Stale() bool {
	row, ok := reasonTable[s.reason]
	return s.NotRunning() && ok && row.stale
}

// State returns the decoded record, or nil when there is none worth keeping.
func (s RunState) State() *session.ProxyState { return s.state }

// PID returns the recorded pid, or 0 when no record names one.
func (s RunState) PID() int {
	if s.state == nil {
		return 0
	}
	return s.state.PID
}

// Addr returns the recorded listen address, or "" when no record names one.
func (s RunState) Addr() string {
	if s.state == nil {
		return ""
	}
	return s.state.Addr
}

// Reason returns the stable not-running token, or "" when a proxy is running.
func (s RunState) Reason() string {
	if !s.NotRunning() {
		return ""
	}
	return reasonTable[s.reason].code
}

// ReasonText returns the one human sentence for this reason.
func (s RunState) ReasonText() string {
	if !s.NotRunning() {
		return ""
	}
	return reasonTable[s.reason].text
}

// Hint returns the actionable next step for this reason, or "" when a proxy is
// running. The command contract forwards it verbatim, so the remedy has one
// author.
func (s RunState) Hint() string {
	if !s.NotRunning() {
		return ""
	}
	return reasonTable[s.reason].hint
}

// Summary is the pid-plus-reason phrase every stale-record diagnostic uses.
func (s RunState) Summary() string {
	text := s.ReasonText()
	if pid := s.PID(); pid > 0 {
		return fmt.Sprintf("pid %d: %s", pid, text)
	}
	return text
}

func notRunning(reason notRunningReason, state *session.ProxyState) RunState {
	if _, ok := reasonTable[reason]; !ok {
		panic("proxy: not-running reason has no table row")
	}
	return RunState{kind: RunNotRunning, state: state, reason: reason}
}

// readRunState reads <store>/proxy.json and classifies it without touching the
// file: Running when the record names a live process that identity accepts,
// otherwise NotRunning with the reason. A missing or unparseable record is a
// state, not an error; only unexpected filesystem failures are errors.
//
// It is unexported on purpose: callers ask Status or Stop, so nobody can read a
// state and forget the cleanup policy that goes with it.
func readRunState(store *session.Store, identity ProcessIdentity) (RunState, error) {
	state, err := store.ReadProxyState()
	switch {
	case err == nil:
	case errors.Is(err, session.ErrProxyStateMissing):
		return notRunning(reasonMissing, nil), nil
	case errors.Is(err, session.ErrProxyStateInvalid):
		return notRunning(reasonInvalid, nil), nil
	default:
		return RunState{}, fmt.Errorf("read proxy state: %w", err)
	}

	pid := state.PID
	if processExited(pid) {
		return notRunning(reasonExited, state), nil
	}
	if identity == nil || !identity(pid) {
		// The process may have exited between the liveness and identity
		// checks; report that as exited rather than as a pid mismatch.
		if processExited(pid) {
			return notRunning(reasonExited, state), nil
		}
		return notRunning(reasonOtherProcess, state), nil
	}
	return RunState{kind: RunRunning, state: state}, nil
}

// Status returns the classified runtime state, removing a stale record when the
// data-directory lock is free. When a start (or another owner) holds the lock,
// the record belongs to that owner and is left untouched.
func Status(store *session.Store, identity ProcessIdentity) (RunState, error) {
	state, err := readRunState(store, identity)
	if err != nil || !state.Stale() {
		return state, err
	}

	lock, lockErr := store.AcquireLock()
	if lockErr != nil {
		// A start is in progress (or another owner holds the lock). Re-read
		// without deleting anything; the owner is responsible for its state.
		return readRunState(store, identity)
	}
	defer lock.Release()

	state, err = readRunState(store, identity)
	if err == nil && state.Stale() {
		_ = store.RemoveProxyState()
	}
	return state, err
}

// Stop signals the live proxy recorded in store and waits for it to exit. It
// returns the state it acted on: Running when the proxy was signalled and
// confirmed gone, NotRunning with a reason when there was nothing to stop. Only
// an unexpected filesystem, signal, or timeout failure is an error.
func Stop(store *session.Store, identity ProcessIdentity) (RunState, error) {
	state, err := Status(store, identity)
	if err != nil || !state.Running() {
		return state, err
	}

	pid := state.PID()
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			removeProxyStateForPID(store, pid)
			return notRunning(reasonExited, state.state), nil
		}
		return RunState{}, fmt.Errorf("signal proxy pid %d: %w", pid, err)
	}

	deadline := time.Now().Add(stopWaitTimeout)
	for !processExited(pid) {
		if time.Now().After(deadline) {
			return RunState{}, &StopTimeoutError{PID: pid}
		}
		time.Sleep(stopPollInterval)
	}
	removeProxyStateForPID(store, pid)
	return state, nil
}

// StopTimeoutError reports a SIGTERM that did not end the proxy in time.
type StopTimeoutError struct {
	PID int
}

func (e *StopTimeoutError) Error() string {
	return fmt.Sprintf("proxy pid %d did not exit within %s", e.PID, stopWaitTimeout)
}

// lockedRunState classifies a store whose data-directory lock is held by
// another process. Liveness and identity are not judged: the lock owner owns
// the record and may be between writing it and serving requests. The decoded
// record is kept for its pid and address in diagnostics.
func lockedRunState(store *session.Store) RunState {
	state, err := store.ReadProxyState()
	if err != nil {
		state = nil
	}
	return RunState{kind: RunLocked, state: state}
}

// removeProxyStateForPID removes proxy.json only when it still describes the
// process that was stopped. A new proxy may have started after the old pid
// exited, and stop must not delete that fresh state.
func removeProxyStateForPID(store *session.Store, pid int) {
	lock, err := store.AcquireLock()
	if err != nil {
		// A new proxy owns the store; never delete its fresh state.
		return
	}
	defer lock.Release()

	state, err := store.ReadProxyState()
	if err == nil && state.PID == pid {
		_ = store.RemoveProxyState()
	}
}

func warnStale(w io.Writer, path string, state RunState) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "mitt: warning: replacing stale %s (%s)\n", path, state.Summary())
}
