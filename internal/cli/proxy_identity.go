package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/proxy"
	"github.com/spf13/cobra"
)

// proxyProcessIdentity reports whether pid's command line runs
// `mitt proxy start`. Identity is the CLI's business: only the CLI knows the
// command tree and therefore each flag's arity. Resolving the recorded argv
// through that same tree replaces the old hand-maintained list of boolean
// flags, which silently broke as soon as a new boolean flag was added.
func proxyProcessIdentity() proxy.ProcessIdentity {
	executable, err := os.Executable()
	if err != nil {
		executable = ""
	}
	return func(pid int) bool {
		args, ok := processArgs(pid)
		if !ok || len(args) == 0 {
			return false
		}
		if !sameExecutable(args[0], executable) {
			return false
		}
		return isProxyStart(args[1:])
	}
}

// isProxyStart asks cobra to resolve the recorded arguments against this
// binary's command tree. A flag value that happens to read like a subcommand
// therefore cannot be mistaken for one.
func isProxyStart(args []string) bool {
	return isProxyStartIn(newRootCmd(io.Discard, io.Discard), args)
}

// isProxyStartIn is isProxyStart against a caller-supplied tree. The identity
// decision is a property of the tree, not of a hand-written flag list, so a new
// boolean flag is picked up the moment it is registered.
func isProxyStartIn(root *cobra.Command, args []string) bool {
	cmd, _, err := root.Find(args)
	return err == nil && cmd != nil && cmd.CommandPath() == "mitt proxy start"
}

func processArgs(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, false
	}
	args := strings.Split(string(data), "\x00")
	for len(args) > 0 && args[len(args)-1] == "" {
		args = args[:len(args)-1]
	}
	return args, len(args) > 0
}

// sameExecutable reports whether argv0 names the binary that is running this
// check. The basename comparison tolerates the common case of a different
// relative path or a symlinked invocation directory.
func sameExecutable(argv0, executable string) bool {
	if executable == "" {
		return filepath.Base(argv0) == "mitt"
	}
	if argv0 == executable {
		return true
	}
	return filepath.Base(argv0) == filepath.Base(executable)
}
