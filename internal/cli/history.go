package cli

import (
	"io"

	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/session"
	"github.com/C1ph3rX13/mitt/internal/spec"
	"github.com/spf13/cobra"
)

func newHistoryCmd(opts *options, stdout io.Writer) *cobra.Command {
	var filter string
	var contextName string
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List captured and replayed history entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistory(opts, stdout, filter, contextName)
		},
	}
	cmd.Flags().StringVar(&filter, "filter", "", "only show one event kind")
	cmd.Flags().StringVar(&contextName, "context", "", "only show entries whose context exactly matches this X-Repeater-Context")
	spec.Declare(cmd, spec.Contract{
		Args:   []spec.Positional{},
		Flags:  []spec.FlagRule{{Name: "filter", Enum: session.Kinds}},
		Errors: []string{output.CodeNoMatch, output.CodeInternalError},
		Outputs: []spec.Output{
			spec.Schema[entry.ListRow]("history-summary", "history summary").
				Streamed().
				Described("One record per matching history entry: NDJSON in JSON, a list in YAML, and rows in table/md."),
		},
	})
	return cmd
}

func runHistory(opts *options, stdout io.Writer, filter, contextName string) error {
	dir, err := opts.dataDir()
	if err != nil {
		return err
	}
	store := session.Open(dir)
	rows, err := entry.List(store, entry.Filter{Kind: filter, Context: contextName}, opts.mode.Diag)
	if err != nil {
		return entry.Translate(err, dir)
	}

	if len(rows) == 0 {
		return output.NewError(
			output.CodeNoMatch,
			"no history entries matched",
			"capture traffic with `mitt proxy start`, or adjust --filter/--context",
		).WithDetail("store", dir).
			WithDetail("filter", filter).
			WithDetail("context", contextName)
	}

	stream, err := output.StreamOf(rows)
	if err != nil {
		return output.NewError(
			output.CodeInternalError,
			err.Error(),
			"report this as a mitt bug",
		).WithDetail("store", dir)
	}
	return opts.writer().Stream(stream)
}
