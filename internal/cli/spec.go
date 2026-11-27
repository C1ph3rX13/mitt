package cli

import (
	"io"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/spec"
	"github.com/spf13/cobra"
)

// specSummaryRow is the one row per command in `mitt spec -f table|md`. It is a
// declared shape, so the human view has a schema like every other mode and the
// contract test can reject a row that no longer matches what the command says.
type specSummaryRow struct {
	Path         string `json:"path"`
	Flags        string `json:"flags"`
	ErrorCodes   string `json:"error_codes"`
	OutputSchema string `json:"output_schema"`
}

func newSpecCmd(opts *options, stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spec",
		Short: "Print the machine-readable command contract",
		RunE: func(cmd *cobra.Command, args []string) error {
			document := spec.Build(cmd.Root())
			writer := opts.writer()
			if opts.modeIs("summary") {
				rows, err := summarizeDocument(document)
				if err != nil {
					return output.InternalErrorf("summarize command contract: %v", err)
				}
				return writer.Stream(rows)
			}
			return writer.Object(document)
		},
	}
	spec.Declare(cmd, spec.Contract{
		Args:   []spec.Positional{},
		Errors: []string{output.CodeInternalError},
		Outputs: []spec.Output{
			spec.Schema[spec.Document]("spec-document", "mitt command contract"),
			// The human view is a declared shape, not a separate renderer: it
			// derives from the same document and is selected only by the row
			// formats, which cannot display the full contract readably.
			spec.Schema[specSummaryRow]("spec-summary", "mitt command contract summary").
				Named("summary").
				ForTabularFormats().
				Streamed().
				PublishAs("spec_summary"),
		},
	})
	return cmd
}

// summarizeDocument projects the contract into one row per command.
func summarizeDocument(document spec.Document) (output.Stream, error) {
	rows := make([]specSummaryRow, 0)
	var walk func(spec.Command)
	walk = func(command spec.Command) {
		rows = append(rows, specSummaryRow{
			Path:         command.Path,
			Flags:        summarizeFlags(command.Flags),
			ErrorCodes:   joinOrDash(command.ErrorCodes),
			OutputSchema: yesNo(command.OutputSchema != nil || len(command.OutputSchemas) > 0),
		})
		for _, child := range command.Children {
			walk(child)
		}
	}
	walk(document.Command)
	return output.StreamOf(rows)
}

func summarizeFlags(flags []spec.Flag) string {
	parts := make([]string, 0, len(flags))
	for _, flag := range flags {
		name := "--" + flag.Name
		if flag.Shorthand != "" {
			name = "-" + flag.Shorthand + ", " + name
		}
		switch flag.Type {
		case "bool":
		case "string":
			name += " <string>"
		default:
			name += " <" + flag.Type + ">"
		}
		if len(flag.Enum) > 0 {
			name += " {" + strings.Join(flag.Enum, "|") + "}"
		}
		parts = append(parts, name)
	}
	return joinOrDash(parts)
}

func joinOrDash(values []string) string {
	switch len(values) {
	case 0:
		return "-"
	case 1:
		return values[0]
	default:
		return strings.Join(values, ", ")
	}
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "-"
}
