package cli_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Budgets for external processes a test starts. A test that waits forever hangs
// the package, and `go test ./...` runs several packages at once, so one
// unbounded child is a way to take the whole machine down instead of failing.
// Each budget is generous against the observed runtime; its job is to turn a
// hang into a failure that names the process.
const (
	// binaryBudget bounds one `mitt` invocation. The slowest legitimate case is
	// a full proxy lifecycle, measured in seconds.
	binaryBudget = 60 * time.Second
	// proxyBudget bounds a proxy child's whole lifetime. t.Cleanup already
	// kills it; this is the backstop for a cleanup that never runs.
	proxyBudget = 120 * time.Second
	// containerBudget bounds one docker run/exec/rm invocation.
	containerBudget = 2 * time.Minute
	// imageBuildBudget bounds the Kali reference image build, which runs a full
	// apt-get install inside the container.
	imageBuildBudget = 8 * time.Minute
	// toolBudget bounds a local tool invocation: git, make, file, ldd, sh.
	toolBudget = 2 * time.Minute
)

// timedCommand is an external command with a deadline. exec.Cmd keeps its
// context private, so the deadline travels alongside it.
type timedCommand struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// newTimedCommand builds an external command that cannot outlive budget.
//
// The child gets its own process group and the deadline kills the whole group,
// because `make` spawns `go build` and a `docker` CLI spawns helper processes;
// killing only the direct child would leave the real work running. WaitDelay is
// the second backstop: even if a descendant outlives the group kill, Wait
// returns instead of blocking on the output pipe forever.
func newTimedCommand(budget time.Duration, name string, args ...string) timedCommand {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	return timedCommand{cmd: cmd, cancel: cancel}
}

// run runs the command to completion and releases its deadline. Use this when
// the test has no use for the child's stdout/stderr wiring.
func (tc timedCommand) run() ([]byte, error) {
	defer tc.cancel()
	return tc.cmd.CombinedOutput()
}

// start runs the command to completion while letting the caller wire up
// stdout, stderr and stdin first. The deadline stays armed until Wait returns,
// so release cancels it afterwards.
func (tc timedCommand) start() error {
	return tc.cmd.Start()
}

// wait blocks for the command to finish and releases the deadline. It must be
// called exactly once for a command that was started.
func (tc timedCommand) wait() error {
	defer tc.cancel()
	return tc.cmd.Wait()
}

// exceededBudget reports whether the command was stopped by its own deadline
// rather than exiting on its own.
func exceededBudget(cmd *exec.Cmd) bool {
	if cmd.ProcessState == nil {
		return false
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// runBounded runs an external command and reports a budget overrun as a test
// failure that names the process, so a hang in git/docker/make surfaces at the
// call site instead of at the package timeout.
func runBounded(t *testing.T, budget time.Duration, name string, args ...string) (string, error) {
	t.Helper()
	tc := newTimedCommand(budget, name, args...)
	output, err := tc.run()
	if err != nil && exceededBudget(tc.cmd) {
		t.Fatalf("%s %v exceeded its %s budget (killed):\n%s", name, args, budget, output)
	}
	return string(output), err
}

// runToExit runs a command to completion and returns it, so a test that needs
// the finished process (for its pid) still gets the budget. The process group
// and deadline stay attached to the returned value.
func runToExit(t *testing.T, budget time.Duration, name string, args ...string) *exec.Cmd {
	t.Helper()
	tc := newTimedCommand(budget, name, args...)
	if _, err := tc.run(); err != nil {
		if exceededBudget(tc.cmd) {
			t.Fatalf("%s %v exceeded its %s budget (killed)", name, args, budget)
		}
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return tc.cmd
}

// TestTestHarnessBoundsEveryExternalProcess pins the budget invariant across
// the whole repository: every test that starts an external process must do it
// through a bounded builder. `go test ./...` runs packages concurrently and a
// hung child holds the package open with no output, so one stray `exec.Command`
// is enough to make a test run look stalled instead of failed.
//
// The allowed files are the bounded builders themselves, not their callers:
// test/cli's builders live in bounds_test.go, and internal/proxy's in
// process_test.go. The same rule covers the constructors that would otherwise
// bypass a budget: a hand-rolled exec.CommandContext with context.Background
// and os.StartProcess are both unbounded children in disguise.
func TestTestHarnessBoundsEveryExternalProcess(t *testing.T) {
	allowed := map[string]bool{
		filepath.Join("test", "cli", "bounds_test.go"):        true,
		filepath.Join("internal", "proxy", "process_test.go"): true,
	}
	unbounded := []string{
		"exec.Command(",
		"exec.CommandContext(",
		"os.StartProcess(",
		"syscall.Exec(",
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
		if !strings.HasSuffix(path, "_test.go") || allowed[rel] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(data)
		for _, constructor := range unbounded {
			if strings.Contains(source, constructor) {
				t.Errorf("%s starts an external process with %s; use a bounded builder so the test cannot hang", rel, constructor)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan test sources: %v", err)
	}
}
