package cli

import (
	"io"
	"testing"
)

// TestIsProxyStartResolvesAgainstCommandTree proves process identity comes from
// the same command tree that runs the CLI. The old session-side parser assumed
// one known boolean persistent flag and that every other flag ate the next
// token, so a new boolean flag could make a live proxy look foreign.
func TestIsProxyStartResolvesAgainstCommandTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "plain", args: []string{"proxy", "start"}, want: true},
		{name: "persistent flag before subcommand", args: []string{"--store", "/tmp/x", "proxy", "start"}, want: true},
		{name: "persistent flag between subcommands", args: []string{"proxy", "--store", "/tmp/x", "start"}, want: true},
		{name: "flag with equals", args: []string{"--store=/tmp/x", "proxy", "start"}, want: true},
		{name: "boolean flag", args: []string{"--format", "json", "proxy", "start"}, want: true},
		{name: "boolean start flag", args: []string{"proxy", "start", "--events"}, want: true},
		{name: "persistent flag with equals between subcommands", args: []string{"proxy", "--format=json", "start"}, want: true},
		{name: "start flags", args: []string{"proxy", "start", "--addr", "127.0.0.1:8080"}, want: true},
		{name: "start flag value looks like a command", args: []string{"proxy", "start", "--addr", "proxy"}, want: true},
		{name: "different command", args: []string{"proxy", "status"}, want: false},
		{name: "other command", args: []string{"history"}, want: false},
		{name: "no arguments", args: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProxyStart(tc.args); got != tc.want {
				t.Errorf("isProxyStart(%q) = %t, want %t", tc.args, got, tc.want)
			}
		})
	}
}

// TestIsProxyStartAcceptsNewBooleanFlag is the regression the previous
// hand-written flag list could not survive: it knew one global flag was boolean and
// assumed every other flag consumed the next token, so one new boolean
// persistent flag made a live proxy look like a foreign process.
func TestIsProxyStartAcceptsNewBooleanFlag(t *testing.T) {
	root := newRootCmd(io.Discard, io.Discard)
	root.PersistentFlags().Bool("telemetry", false, "test-only boolean persistent flag")

	for _, args := range [][]string{
		{"--telemetry", "proxy", "start"},
		{"proxy", "--telemetry", "start"},
		{"proxy", "start", "--telemetry"},
	} {
		if !isProxyStartIn(root, args) {
			t.Errorf("isProxyStartIn(%q) = false, want true", args)
		}
	}
}

func TestSameExecutableComparesBasenames(t *testing.T) {
	for _, tc := range []struct {
		name       string
		argv0      string
		executable string
		want       bool
	}{
		{name: "same path", argv0: "/usr/local/bin/mitt", executable: "/usr/local/bin/mitt", want: true},
		{name: "relative invocation", argv0: "./mitt", executable: "/repo/mitt", want: true},
		{name: "different binary", argv0: "/usr/bin/sleep", executable: "/repo/mitt", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameExecutable(tc.argv0, tc.executable); got != tc.want {
				t.Errorf("sameExecutable(%q, %q) = %t, want %t", tc.argv0, tc.executable, got, tc.want)
			}
		})
	}
}
