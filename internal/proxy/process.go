//go:build linux

package proxy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// processExited reports whether pid is no longer a live process. Zombies count
// as exited so `proxy stop` can confirm shutdown before the parent reaps the
// child.
func processExited(pid int) bool {
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}

	data, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(readErr, fs.ErrNotExist) {
		return true
	}
	if readErr == nil {
		if state, ok := procState(string(data)); ok && (state == 'Z' || state == 'X' || state == 'x') {
			return true
		}
	}
	return false
}

// procState extracts the single-letter process state from /proc/<pid>/stat.
func procState(stat string) (byte, bool) {
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 || closeParen+2 >= len(stat) {
		return 0, false
	}
	return stat[closeParen+2], true
}
