package cli

import (
	"io"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/diff"
	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/session"
	"github.com/C1ph3rX13/mitt/internal/spec"
	"github.com/spf13/cobra"
)

func newDiffCmd(opts *options, stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff <idA> <idB>",
		Short: "Compare the responses of two history entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(opts, stdout, args[0], args[1])
		},
	}
	spec.Declare(cmd, spec.Contract{
		Args: []spec.Positional{
			{Name: "idA", Required: true},
			{Name: "idB", Required: true},
		},
		Errors: []string{
			output.CodeEntryNotFound,
			output.CodeInvalidRequest,
			output.CodeInternalError,
		},
		Outputs: []spec.Output{
			diffSchema(),
		},
	})
	return cmd
}

// diffSchema publishes the comparison result. Its entry kinds are the exchange
// kinds only: a compared side always has a response, so a mitm_error can never
// appear there, and the schema says so instead of listing it.
func diffSchema() spec.Output {
	out := spec.Schema[diff.Result]("diff", "diff")
	for _, side := range []string{"a", "b"} {
		properties, _ := out.Schema["properties"].(map[string]any)
		entrySchema, ok := properties[side].(map[string]any)
		if !ok {
			panic("spec: diff result is missing its " + side + " entry")
		}
		spec.RestrictEnum(entrySchema, "kind", []string{
			string(session.KindCaptured),
			string(session.KindReplayed),
			string(session.KindSent),
		})
	}
	return out
}

func runDiff(opts *options, stdout io.Writer, idA, idB string) error {
	dir, err := opts.dataDir()
	if err != nil {
		return err
	}
	store := session.Open(dir)

	// Both entries come from one scan and one projection, so a bad id produces
	// one not-found answer and both sides agree about status and body paths.
	details, err := entry.LoadMany(store, []string{idA, idB}, opts.mode.Diag)
	if err != nil {
		return entry.Translate(err, dir)
	}
	detailA, detailB := details[0], details[1]
	if !detailA.HasResponse() || !detailB.HasResponse() {
		return noResponseError(detailA, detailB)
	}

	result, err := diff.Compare(detailA.Response(), detailB.Response())
	if err != nil {
		// A body the record claims but the disk cannot deliver is the entry
		// package's diagnostic; translating it here gives the same code and
		// details every other stored-body reader reports.
		return entry.Translate(err, dir)
	}
	return opts.writer().Object(result)
}

// noResponseError explains which side cannot be compared and why. mitm_error
// entries carry a reason and target host; incomplete exchanges carry no status.
func noResponseError(details ...*entry.Detail) error {
	entries := map[string]any{}
	ids := make([]string, 0, len(details))
	for index, detail := range details {
		if detail.HasResponse() {
			continue
		}
		side := "a"
		if index == 1 {
			side = "b"
		}
		status := 0
		if detail.Status != nil {
			status = *detail.Status
		}
		entryDetails := map[string]any{
			"id":     detail.ID,
			"kind":   string(detail.Kind),
			"status": status,
		}
		if detail.Reason != "" {
			entryDetails["reason"] = detail.Reason
		}
		if detail.TargetHost != "" {
			entryDetails["target_host"] = detail.TargetHost
		}
		entries[side] = entryDetails
		ids = append(ids, detail.ID)
	}
	return output.NewError(
		output.CodeInvalidRequest,
		"cannot diff entries without a response: "+strings.Join(ids, ", "),
		"diff compares upstream responses; mitm_error and failed exchanges carry none",
	).WithDetail("ids", ids).WithDetail("entries", entries)
}
