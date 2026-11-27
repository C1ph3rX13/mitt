package cli

import (
	"errors"
	"io"

	"github.com/C1ph3rX13/mitt/internal/ca"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/proxy"
	"github.com/C1ph3rX13/mitt/internal/session"
	"github.com/C1ph3rX13/mitt/internal/spec"
	"github.com/spf13/cobra"
)

// errInterrupted asks Execute to exit 130 after the proxy has cleaned up.
var errInterrupted = errors.New("interrupted by SIGINT")

type proxyStatus struct {
	Running bool `json:"running"`
	session.ProxyState
}

type proxyStopResult struct {
	Stopped bool `json:"stopped"`
	PID     int  `json:"pid"`
}

func newProxyCmd(opts *options, stdout io.Writer) *cobra.Command {
	proxyCmd := &cobra.Command{
		Use:   "proxy",
		Short: "Run and inspect the capture proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	spec.Declare(proxyCmd, spec.Contract{Args: []spec.Positional{}})
	proxyCmd.AddCommand(newProxyStartCmd(opts, stdout))
	proxyCmd.AddCommand(newProxyStatusCmd(opts, stdout))
	proxyCmd.AddCommand(newProxyStopCmd(opts, stdout))
	return proxyCmd
}

func newProxyStartCmd(opts *options, stdout io.Writer) *cobra.Command {
	var addr string
	var explicitCA string
	var allowTargets string
	var denyTargets string
	var http2ALPN string
	var portRange string
	var sessionPerPort bool
	var events bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Run the capture proxy in the foreground",
		RunE: func(cmd *cobra.Command, args []string) error {
			// --http2-alpn is a semantic value rather than a compatibility
			// rule, so it is resolved here instead of in a hand-written
			// PreRunE; Declare owns that hook for the declared constraints.
			alpnMode, err := proxy.ParseALPNMode(http2ALPN)
			if err != nil {
				return output.NewError(
					output.CodeInvalidRequest,
					err.Error(),
					"use one of: h1, h2, h2,h1",
				).WithDetail("value", http2ALPN).WithDetail("allowed", proxy.ALPNModes)
			}
			dir, err := opts.dataDir()
			if err != nil {
				return err
			}
			plan, err := proxy.NewListenPlan(proxy.ListenPlanRequest{
				Store:          session.Open(dir),
				Addr:           addr,
				PortRange:      portRange,
				PortRangeSet:   cmd.Flags().Changed("port-range"),
				SessionPerPort: sessionPerPort,
			})
			if err != nil {
				return proxyListenPlanError(err, addr, portRange)
			}
			return runProxyStart(cmd, opts, stdout, plan, addr, explicitCA, allowTargets, denyTargets, alpnMode)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", proxy.DefaultAddr, "listen address")
	cmd.Flags().StringVar(&explicitCA, "ca", "", "CA directory (default <store>/ca)")
	cmd.Flags().StringVar(&allowTargets, "allow-targets", "", "comma-separated allowed target hosts; *.example.com matches subdomains only")
	cmd.Flags().StringVar(&denyTargets, "deny-targets", "", "comma-separated denied target hosts; takes priority over --allow-targets")
	cmd.Flags().StringVar(&http2ALPN, "http2-alpn", string(proxy.ALPNModeH1), "ALPN protocols to advertise during CONNECT MITM: h1, h2, h2,h1")
	cmd.Flags().StringVar(&portRange, "port-range", "", "listen on every port in a-b; the host comes from --addr")
	cmd.Flags().BoolVar(&sessionPerPort, "session-per-port", false, "give each --port-range port its own <store>/ports/<port> data directory")
	cmd.Flags().BoolVar(&events, "events", false, "stream captured events to stdout as NDJSON for the lifetime of the run")
	spec.Declare(cmd, spec.Contract{
		Args:  []spec.Positional{},
		Flags: []spec.FlagRule{{Name: "http2-alpn", Enum: proxy.ALPNModes}},
		Constraints: []spec.Constraint{
			// Per-port data directories only exist because a port range
			// creates one endpoint per port; without it the flag has no
			// meaning. ListenPlan still refuses the combination, but the
			// caller-facing rule is declared here so spec publishes it.
			spec.Requires("session-per-port-needs-range", output.CodeInvalidRequest, "--session-per-port", "--port-range"),
		},
		Errors: []string{
			output.CodeCAMissing,
			output.CodeLocked,
			output.CodeAddrInUse,
			output.CodeInvalidRequest,
			output.CodeInternalError,
		},
		// A foreground proxy is silent by default: supervisors can leave the
		// pipe unread. --events opts into the live stream, which only JSON can
		// render because records are written as they happen.
		Outputs: []spec.Output{
			spec.Schema[session.Event]("proxy-event", "proxy event").
				Named("events").
				WhenFlag("--events").
				Live().
				Described("One NDJSON line emitted by `mitt proxy start --events` for each captured exchange or MITM failure."),
		},
	})
	return cmd
}

func newProxyStatusCmd(opts *options, stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the proxy is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProxyStatus(opts, stdout)
		},
	}
	spec.Declare(cmd, spec.Contract{
		Args:    []spec.Positional{},
		Errors:  []string{output.CodeNotRunning, output.CodeInternalError},
		Outputs: []spec.Output{spec.Schema[proxyStatus]("proxy-status", "proxy status")},
	})
	return cmd
}

func newProxyStopCmd(opts *options, stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProxyStop(opts, stdout)
		},
	}
	spec.Declare(cmd, spec.Contract{
		Args:    []spec.Positional{},
		Errors:  []string{output.CodeNotRunning, output.CodeInternalError},
		Outputs: []spec.Output{spec.Schema[proxyStopResult]("proxy-stop", "proxy stop")},
	})
	return cmd
}

// proxyListenPlanError maps listen-plan validation failures onto the command
// contract. Plan errors stay in the proxy package; the code, hint and details
// live here with the rest of the command contract.
//
// ErrSessionPerPort is normally unreachable from the CLI — the declared
// `session-per-port-needs-range` constraint rejects the combination in PreRunE
// first. The branch stays because ListenPlan owns that invariant for callers
// other than this command, and an invariant should not lose its adapter.
func proxyListenPlanError(err error, addr, portRange string) error {
	switch {
	case errors.Is(err, proxy.ErrPortRange):
		return output.NewError(
			output.CodeInvalidRequest,
			err.Error(),
			"use --port-range a-b with 1 <= a <= b <= 65535",
		).WithDetail("port_range", portRange)
	case errors.Is(err, proxy.ErrSessionPerPort):
		return output.NewError(
			output.CodeInvalidRequest,
			err.Error(),
			"pass both: --port-range a-b --session-per-port",
		).WithDetail("session_per_port", true)
	case errors.Is(err, proxy.ErrInvalidAddr):
		return output.NewError(
			output.CodeInvalidRequest,
			err.Error(),
			"pass --addr as host:port, for example 127.0.0.1:8080",
		).WithDetail("addr", failedListenAddr(err, addr))
	default:
		return err
	}
}

func runProxyStart(cmd *cobra.Command, opts *options, stdout io.Writer, plan proxy.ListenPlan, addr, explicitCA, allowTargets, denyTargets string, alpnMode proxy.ALPNMode) error {
	dir := plan.RootStore().Dir()
	caDir, err := opts.caDir(explicitCA)
	if err != nil {
		return err
	}
	authority, err := ca.Load(caDir)
	if err != nil {
		return proxyCAError(caDir, err)
	}

	// The declared `events` mode is the event stream switch. Without it a
	// long-running proxy stays silent on stdout so background supervisors can
	// leave the pipe unread.
	var eventSink io.Writer
	if opts.modeIs("events") {
		eventSink = stdout
	}

	err = proxy.Start(proxy.StartOptions{
		Plan:      plan,
		Authority: authority,
		Scope:     proxy.NewScope(allowTargets, denyTargets),
		Identity:  proxyProcessIdentity(),
		HTTP2ALPN: alpnMode,
		Events:    eventSink,
		Stderr:    opts.mode.Diag,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, proxy.ErrInterrupted):
		return errInterrupted
	case errors.Is(err, proxy.ErrLocked):
		return proxyLockedError(dir, err)
	case errors.Is(err, proxy.ErrAddrInUse):
		return output.NewError(
			output.CodeAddrInUse,
			err.Error(),
			"choose a different --addr or --port-range, or stop the process using this address",
		).WithDetail("addr", failedListenAddr(err, addr))
	case errors.Is(err, proxy.ErrInvalidAddr):
		return output.NewError(
			output.CodeInvalidRequest,
			err.Error(),
			"pass --addr as host:port, for example 127.0.0.1:8080",
		).WithDetail("addr", failedListenAddr(err, addr))
	default:
		return output.NewError(
			output.CodeInternalError,
			err.Error(),
			"check permissions on the data directory and retry",
		).WithDetail("store", dir).WithDetail("addr", addr)
	}
}

func runProxyStatus(opts *options, stdout io.Writer) error {
	dir, err := opts.dataDir()
	if err != nil {
		return err
	}
	run, err := proxy.Status(session.Open(dir), proxyProcessIdentity())
	if err != nil {
		return output.NewError(
			output.CodeInternalError,
			err.Error(),
			"check permissions on the data directory",
		).WithDetail("store", dir)
	}
	if !run.Running() {
		return proxyNotRunningError(dir, run)
	}

	return opts.writer().Object(proxyStatus{Running: true, ProxyState: *run.State()})
}

func runProxyStop(opts *options, stdout io.Writer) error {
	dir, err := opts.dataDir()
	if err != nil {
		return err
	}
	run, err := proxy.Stop(session.Open(dir), proxyProcessIdentity())
	if err != nil {
		var timeout *proxy.StopTimeoutError
		if errors.As(err, &timeout) {
			return output.NewError(
				output.CodeInternalError,
				err.Error(),
				"inspect the process and retry, or terminate it manually",
			).WithDetail("pid", timeout.PID).WithDetail("store", dir)
		}
		return output.NewError(
			output.CodeInternalError,
			err.Error(),
			"check process ownership and permissions",
		).WithDetail("store", dir)
	}
	if !run.Running() {
		return proxyNotRunningError(dir, run)
	}

	return opts.writer().Object(proxyStopResult{Stopped: true, PID: run.PID()})
}

// proxyNotRunningError is the one mapping from a classified not-running state
// to the command error. status and stop share it, so the same store condition
// cannot produce two different codes or exit codes.
func proxyNotRunningError(store string, run proxy.RunState) *output.AppError {
	appErr := output.NewError(
		output.CodeNotRunning,
		"proxy is not running",
		run.Hint(),
	).WithDetail("store", store).WithDetail("reason", run.Reason())
	if pid := run.PID(); pid > 0 {
		appErr.WithDetail("pid", pid)
	}
	return appErr
}

func failedListenAddr(err error, fallback string) string {
	var addrErr *proxy.AddrError
	if errors.As(err, &addrErr) && addrErr.Addr != "" {
		return addrErr.Addr
	}
	return fallback
}

func proxyLockedError(store string, err error) *output.AppError {
	appErr := output.NewError(
		output.CodeLocked,
		"another proxy is already running for this data directory",
		"run `mitt proxy status` to inspect it, or `mitt proxy stop` to stop it",
	).WithDetail("store", store)
	var locked *proxy.LockedError
	if errors.As(err, &locked) {
		if pid := locked.State.PID(); pid > 0 {
			appErr.WithDetail("pid", pid)
		}
		if addr := locked.State.Addr(); addr != "" {
			appErr.WithDetail("addr", addr)
		}
	}
	return appErr
}

func proxyCAError(dir string, err error) *output.AppError {
	reason := "invalid"
	if errors.Is(err, ca.ErrNotFound) {
		reason = "missing"
	}
	return output.NewError(
		output.CodeCAMissing,
		"CA is missing or unusable",
		"run `mitt ca init` before `mitt proxy start`, or pass --ca <dir>",
	).WithDetail("ca_path", dir).WithDetail("reason", reason)
}
