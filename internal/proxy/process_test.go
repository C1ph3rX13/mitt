//go:build linux

package proxy

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// testProcessBudget bounds a helper process a test starts. `go test ./...`
// runs packages concurrently, so an unbounded child that never exits is a way
// to wedge a whole machine instead of failing one test. The budget is generous
// against how long the helper should live; its job is to turn a hang into a
// failure that names the process.
const testProcessBudget = 30 * time.Second

// newTestProcess builds a helper process that cannot outlive the budget. The
// child gets its own process group and the deadline kills the whole group, so
// a wrapper that spawns children cannot leave them behind; WaitDelay is the
// second backstop for a descendant that still holds stdout.
func newTestProcess(name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), testProcessBudget)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd, cancel
}
