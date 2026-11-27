package cli

import (
	"io"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/proxy"
	"github.com/C1ph3rX13/mitt/internal/session"
	"github.com/C1ph3rX13/mitt/internal/spec"
	"github.com/spf13/cobra"
)

func newShowCmd(opts *options, stdout io.Writer) *cobra.Command {
	var raw bool
	var detail bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one history entry's request and response",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(opts, stdout, args[0], raw)
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&raw, "raw", false, "emit the captured request and response bytes")
	flags.BoolVar(&detail, "detail", false, "include body paths and the request_document view")
	spec.Declare(cmd, spec.Contract{
		Args: []spec.Positional{{Name: "id", Required: true}},
		Constraints: []spec.Constraint{
			// --raw writes the stored bytes verbatim, so it cannot be combined
			// with the structured detail shape. Both flags are declared here,
			// so the rule and the two modes share one declaration. The
			// format-vs-opaque mismatch is a property of the resolved output
			// mode, so prepare owns it instead.
			spec.Forbids("raw-excludes-detail", output.CodeInvalidRequest, "--raw", "--detail"),
		},
		Errors: []string{output.CodeEntryNotFound, output.CodeInvalidRequest, output.CodeInternalError},
		Outputs: []spec.Output{
			showSummarySchema(),
			showDetailSchema(),
			spec.Opaque("raw", "The captured request and response bytes, written verbatim: the forwarded request line, stored header order, and stored body bytes.").
				WhenFlag("--raw").
				PublishAs("show_raw"),
		},
	})
	return cmd
}

// showSummarySchema derives the default show output schema and documents the
// reason field from the proxy's reason vocabulary. The vocabulary lives with
// the failure table, so a new failure reason reaches `mitt spec` without
// a second hand-maintained list here.
func showSummarySchema() spec.Output {
	out := spec.Schema[entry.View]("show", "show")
	spec.DescribeProperty(out.Schema, "reason", "Lowercase snake_case failure reason; one of: "+strings.Join(proxy.MITMReasons, ", ")+".")
	return out
}

// showDetailSchema publishes the detailed shape under its own name. Both
// schemas derive from the two types the command emits, so a field cannot exist
// in one shape and silently disappear from the other.
func showDetailSchema() spec.Output {
	out := spec.Schema[entry.Detail]("show-detail", "show --detail").
		Named("detail").
		WhenFlag("--detail")
	spec.DescribeProperty(out.Schema, "reason", "Lowercase snake_case failure reason; one of: "+strings.Join(proxy.MITMReasons, ", ")+".")
	return out
}

func runShow(opts *options, stdout io.Writer, id string, raw bool) error {
	dir, err := opts.dataDir()
	if err != nil {
		return err
	}
	store := session.Open(dir)

	// One read serves all three shapes: the summary, the detail, and the raw
	// bytes come from the same Detail, so they cannot disagree about which
	// body file or which request line describes the entry.
	detail, err := entry.Load(store, id, opts.mode.Diag)
	if err != nil {
		return entry.Translate(err, dir)
	}

	if raw {
		if err := entry.WriteRaw(detail, stdout); err != nil {
			return entry.Translate(err, dir)
		}
		return nil
	}
	if opts.modeIs("detail") {
		return opts.writer().Object(detail)
	}
	return opts.writer().Object(&detail.View)
}
