package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/session"
	"github.com/C1ph3rX13/mitt/internal/spec"
	"github.com/spf13/cobra"
)

type options struct {
	store  string
	format *formatFlag
	stdout io.Writer
	stderr io.Writer
	mode   output.Mode
}

// Execute runs the CLI and returns the process exit code.
func Execute(stdout, stderr io.Writer) int {
	root := newRootCmd(stdout, stderr)
	err := root.Execute()
	if err == nil {
		return 0
	}
	if errors.Is(err, errInterrupted) {
		// SIGINT already cleaned up the proxy; 130 is the sysexits interrupt
		// contract, so do not emit a command error object.
		return 130
	}
	appErr := asAppError(err)
	if writeErr := output.WriteError(stderr, appErr); writeErr != nil {
		_, _ = io.WriteString(stderr, "mitt: failed to write error object: "+writeErr.Error()+"\n")
	}
	return appErr.ExitCode()
}

func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := &options{format: newFormatFlag(), stdout: stdout, stderr: stderr}
	root := &cobra.Command{
		Use:           "mitt [command]",
		Short:         "Capture, inspect, modify and replay HTTP/HTTPS traffic",
		Long:          "mitt is an agent-first HTTP/HTTPS capture proxy and Repeater-style replay CLI.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return opts.prepare(cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return output.UsageError(err.Error())
	})

	flags := root.PersistentFlags()
	flags.StringVar(&opts.store, "store", "~/.mitt", "data directory")
	flags.VarP(opts.format, "format", "f", "output format: json, table, yaml, md")
	spec.Declare(root, spec.Contract{
		// Declaring the empty positional list also replaces cobra's legacy
		// argument check, so an unknown subcommand is a typed usage error.
		Args:  []spec.Positional{},
		Flags: []spec.FlagRule{{Name: "format", Enum: output.FormatNames}},
	})

	root.AddCommand(newCACmd(opts, stdout))
	root.AddCommand(newProxyCmd(opts, stdout))
	root.AddCommand(newHistoryCmd(opts, stdout))
	root.AddCommand(newShowCmd(opts, stdout))
	root.AddCommand(newSendCmd(opts, stdout))
	root.AddCommand(newDiffCmd(opts, stdout))
	root.AddCommand(newSkillsCmd(opts, stdout))
	root.AddCommand(newSpecCmd(opts, stdout))
	return root
}

// prepare resolves the one output mode for this invocation. The format is
// parsed here so every command sees the same value; the shape key comes from
// the command's own declaration, so "which shape does this command emit" is
// answered by spec.SelectOutput rather than by ad-hoc flag tests in RunE.
func (o *options) prepare(cmd *cobra.Command) error {
	format, err := o.format.Format()
	if err != nil {
		return output.UsageError(err.Error())
	}
	selected, ok := spec.SelectOutput(cmd, format)
	if ok && selected.EventStream && format != output.FormatJSON {
		return output.NewError(
			output.CodeInvalidRequest,
			fmt.Sprintf("%s emits a live NDJSON stream and cannot be rendered as %s", cmd.CommandPath(), format),
			"drop --format and use the default JSON output",
		).WithDetail("command", cmd.CommandPath()).
			WithDetail("format", string(format)).
			WithDetail("mode", selected.Mode)
	}
	if ok && selected.Opaque && format != output.FormatJSON {
		// Opaque modes write bytes that no -f renderer can format; accepting
		// --format here would silently ignore it.
		return output.NewError(
			output.CodeInvalidRequest,
			fmt.Sprintf("%s mode %s writes raw output and cannot be rendered as %s", cmd.CommandPath(), selected.Mode, format),
			"drop --format when asking for raw output",
		).WithDetail("command", cmd.CommandPath()).
			WithDetail("format", string(format)).
			WithDetail("mode", selected.Mode)
	}
	o.mode = output.Mode{
		Format: format,
		Key:    selected.Mode,
		Data:   o.stdout,
		Diag:   o.stderr,
	}
	return nil
}

func (o *options) dataDir() (string, error) {
	raw := o.store
	if raw == "" {
		raw = "~/.mitt"
	}
	dir, err := expandPath(raw)
	if err != nil {
		return "", output.InternalErrorf("resolve data directory %q: %v", raw, err)
	}
	return dir, nil
}

// caDir resolves --dir when explicitly set; otherwise the CA lives under
// <store>/ca so an overridden store never falls back to the real ~/.mitt/ca.
func (o *options) caDir(explicit string) (string, error) {
	if explicit != "" {
		dir, err := expandPath(explicit)
		if err != nil {
			return "", output.InternalErrorf("resolve CA directory %q: %v", explicit, err)
		}
		return dir, nil
	}
	store, err := o.dataDir()
	if err != nil {
		return "", err
	}
	return session.Open(store).CADir(), nil
}

func expandPath(raw string) (string, error) {
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if raw == "~" {
			raw = home
		} else {
			raw = filepath.Join(home, raw[2:])
		}
	}
	return filepath.Abs(raw)
}

// writer is the command's data sink. It reads the resolved mode, so no command
// constructs a writer from a format or a flag of its own.
func (o *options) writer() *output.Writer {
	return o.mode.Writer()
}

// modeIs reports whether the invocation selected the declared shape mode.
func (o *options) modeIs(mode string) bool {
	return o.mode.Key == mode
}

// asAppError returns err as an AppError. Argument and usage mistakes are
// already typed at their source (spec's positional validator, the flag error
// hook, and prepare), so anything untyped here is an internal failure.
func asAppError(err error) *output.AppError {
	var appErr *output.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return output.NewError(output.CodeInternalError, err.Error(), "re-run the command; if the problem persists, report it")
}
