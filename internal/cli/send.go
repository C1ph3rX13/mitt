package cli

import (
	"encoding/base64"
	"io"
	"os"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/diff"
	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/replay"
	"github.com/C1ph3rX13/mitt/internal/session"
	"github.com/C1ph3rX13/mitt/internal/spec"
	"github.com/spf13/cobra"
)

// sendOutput is the single-object result of `mitt send`: the entry that was
// just written, in the same shape `show` uses, plus both body locations and —
// with --diff-against — the parent comparison. Deriving it from entry.View
// keeps a send result and a shown entry from disagreeing about status, body
// sizes, or the request line.
type sendOutput struct {
	entry.View

	RequestBodyPath  string `json:"request_body_path"`
	ResponseBodyPath string `json:"response_body_path"`

	// DiffAgainst is present only with --diff-against on the <id> base.
	DiffAgainst *diff.Summary `json:"diff_against,omitempty" desc:"Present only with --diff-against on the <id> base."`
}

// dryRunOutput is the single-object result of `mitt send --dry-run`. It shows
// the final request document and a bounded preview of the exact bytes that
// would be written to the connection.
type dryRunOutput struct {
	RequestDocument     *entry.Document `json:"request_document" desc:"At most one of body, body_base64, and body_file may be present and non-null."`
	RequestBytesPreview string          `json:"request_bytes_preview" schema:"encoding=base64" desc:"Base64 of the first request_bytes_preview_max_bytes rendered request bytes."`
	PreviewTotalBytes   int             `json:"preview_total_bytes" schema:"min=0"`
	PreviewTruncated    bool            `json:"preview_truncated"`
}

func newSendCmd(opts *options, stdout io.Writer) *cobra.Command {
	var rawURL string
	var fromFile string
	var patchValue string
	var patchVars []string
	var tlsProfile string
	var schemaOnly bool
	var dryRun bool
	var diffAgainst bool
	cmd := &cobra.Command{
		Use:   "send [<id>]",
		Short: "Send a request directly without the capture proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			if schemaOnly {
				return opts.writer().Object(spec.RequestDocumentSchema())
			}
			return runSend(opts, stdout, cmd, args, rawURL, fromFile, patchValue, patchVars, tlsProfile, dryRun, diffAgainst)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&rawURL, "url", "", "base: construct a request from an absolute http(s) URL")
	flags.StringVar(&fromFile, "from-file", "", "base: read a raw HTTP/1.x request from a file")
	flags.StringVar(&patchValue, "patch", "", "apply an RFC 7386 JSON Merge Patch to the request document (inline, @path, or - for stdin)")
	flags.StringArrayVar(&patchVars, "var", nil, "substitute {{K}} in --patch: K=V, or NAME to read the environment")
	flags.StringVar(&tlsProfile, "tls-profile", string(replay.TLSProfileStandard), "outbound TLS ClientHello fingerprint: standard, chrome, firefox")
	flags.BoolVar(&schemaOnly, "schema", false, "print the request document JSON Schema and exit")
	flags.BoolVar(&dryRun, "dry-run", false, "render the final request document and byte preview without sending")
	flags.BoolVar(&diffAgainst, "diff-against", false, "with the <id> base, also summarize the response difference from the parent entry")
	spec.Declare(cmd, spec.Contract{
		Args: []spec.Positional{{Name: "id", Required: false}},
		Flags: []spec.FlagRule{
			{Name: "tls-profile", Enum: replay.TLSProfiles},
		},
		Constraints: []spec.Constraint{
			// --schema is a pure self-description request, so nothing else may
			// accompany it, including flags added later.
			spec.Alone("schema-alone", output.CodeInvalidRequest, "--schema"),
			// Every other mode needs exactly one base.
			spec.OneOf("base", output.CodeBaseConflict, "<id>", "--url", "--from-file").Unless("--schema"),
			// --diff-against compares a replay with its parent entry, so it
			// needs the <id> base and has nothing to compare under --url,
			// --from-file, or --dry-run.
			spec.Requires("diff-against-needs-id", output.CodeInvalidRequest, "--diff-against", "<id>"),
			spec.Forbids("diff-against-bases", output.CodeInvalidRequest, "--diff-against", "--url", "--from-file", "--dry-run"),
			// --var only exists to feed --patch.
			spec.Requires("var-needs-patch", output.CodeInvalidRequest, "--var", "--patch"),
		},
		Errors: []string{
			output.CodeBaseConflict,
			output.CodeEntryNotFound,
			output.CodeInvalidRequest,
			output.CodeInvalidPatch,
			output.CodeUpstreamTLS,
			output.CodeTempFailure,
			output.CodeInternalError,
		},
		Outputs: []spec.Output{
			sendSchema(),
			spec.Schema[dryRunOutput]("send-dry-run", "send --dry-run").
				Named("dry-run").
				WhenFlag("--dry-run").
				PublishAs("send_dry_run"),
			spec.MetaSchema("schema").WhenFlag("--schema"),
		},
	})
	return cmd
}

// sendSchema publishes the entry view a send result carries, narrowed to what a
// send can actually produce: the two kinds it writes, and a complete exchange —
// a send that returns has a status and both body sizes. The kind vocabulary
// lives in session; the rest is the shared view, tightened rather than rebuilt.
func sendSchema() spec.Output {
	out := spec.Schema[sendOutput]("send", "send")
	spec.RestrictEnum(out.Schema, "kind", []string{string(session.KindSent), string(session.KindReplayed)})
	spec.RequireNonNull(out.Schema, "status")
	spec.RestrictRange(out.Schema, "status", 100, 999)
	spec.RequireProperties(out.Schema,
		"method", "url", "request_line", "response_line",
		"req_body_size", "resp_body_size", "timings", "request_body_path", "response_body_path",
	)
	return out
}

func runSend(opts *options, stdout io.Writer, cmd *cobra.Command, args []string, rawURL, fromFile, patchValue string, patchVars []string, tlsProfile string, dryRun, diffAgainst bool) error {
	dir, err := opts.dataDir()
	if err != nil {
		return err
	}
	store := session.Open(dir)

	var parent *entry.Detail
	if diffAgainst {
		detail, err := diffAgainstParent(store, dir, args[0], opts.mode.Diag)
		if err != nil {
			return err
		}
		parent = detail
	}

	request := replay.SendRequest{
		Store:      store,
		TLSProfile: tlsProfile,
		Diag:       opts.mode.Diag,
	}
	switch {
	case len(args) == 1:
		request.Base = replay.EntryBase{ID: args[0]}
	case cmd.Flags().Changed("url"):
		request.Base = replay.URLBase{URL: rawURL}
	case cmd.Flags().Changed("from-file"):
		request.Base = replay.FileBase{Path: fromFile}
	}
	if cmd.Flags().Changed("patch") {
		vars, err := resolvePatchVariables(patchVars)
		if err != nil {
			return err
		}
		switch {
		case patchValue == "-":
			request.Patch = replay.StdinPatch{Reader: cmd.InOrStdin(), Vars: vars}
		case strings.HasPrefix(patchValue, "@"):
			request.Patch = replay.FilePatch{Path: strings.TrimPrefix(patchValue, "@"), Vars: vars}
		default:
			request.Patch = replay.InlinePatch{Text: patchValue, Vars: vars}
		}
	}

	if dryRun {
		result, err := replay.DryRun(request)
		if err != nil {
			return err
		}
		return opts.writer().Object(dryRunOutput{
			RequestDocument:     result.Document,
			RequestBytesPreview: base64.StdEncoding.EncodeToString(result.Preview),
			PreviewTotalBytes:   result.RequestTotalBytes,
			PreviewTruncated:    result.Truncated,
		})
	}

	// `send` writes into the same data directory as a running proxy, so it is a
	// second writer that can clear residue from an earlier crash. The per-file
	// writer lock keeps a body the proxy is currently staging intact.
	store.SweepStaleTemps(opts.mode.Diag)

	result, sendErr := replay.Send(request)
	if result != nil {
		out := buildSendOutput(store, result.Event)
		if parent != nil {
			child := entry.DetailOf(store, result.Event)
			summary, err := diff.Summarize(parent.Response(), child.Response())
			if err != nil {
				// The comparison reads both stored bodies; a store that cannot
				// deliver the bytes it records gets the same diagnostic here as
				// in `show` and `diff`.
				return entry.Translate(err, dir)
			}
			out.DiffAgainst = summary
		}
		if err := opts.writer().Object(out); err != nil {
			return err
		}
	}
	return sendErr
}

// resolvePatchVariables turns repeated --var values into a name-to-value map.
// A value without "=" reads the same-named environment variable. This is flag
// syntax, so it stays in the CLI; replay only receives resolved values.
func resolvePatchVariables(specs []string) (map[string]string, error) {
	variables := map[string]string{}
	for _, spec := range specs {
		name, value, hasValue := strings.Cut(spec, "=")
		if name == "" {
			return nil, output.NewError(
				output.CodeInvalidPatch,
				"--var requires a non-empty variable name",
				"pass --var K=V or --var NAME",
			)
		}
		if !hasValue {
			envValue, exists := os.LookupEnv(name)
			if !exists {
				return nil, output.NewError(
					output.CodeInvalidPatch,
					"--var "+name+" is not set in the environment",
					"export "+name+" or pass --var "+name+"=<value>",
				).WithDetail("var", name)
			}
			value = envValue
		}
		variables[name] = value
	}
	return variables, nil
}

// diffAgainstParent loads the parent entry before sending so a parent without
// a response fails cleanly without creating a replay entry.
func diffAgainstParent(store *session.Store, dir, id string, diag io.Writer) (*entry.Detail, error) {
	detail, err := entry.Load(store, id, diag)
	if err != nil {
		return nil, entry.Translate(err, dir)
	}
	if !detail.HasResponse() {
		appErr := output.NewError(
			output.CodeInvalidRequest,
			"history entry "+id+" has no response to diff against",
			"choose a base entry whose upstream response was captured, or drop --diff-against",
		).WithDetail("id", id).
			WithDetail("kind", string(detail.Kind))
		if detail.Status != nil {
			appErr.WithDetail("status", *detail.Status)
		}
		if detail.Reason != "" {
			appErr.WithDetail("reason", detail.Reason)
		}
		if detail.TargetHost != "" {
			appErr.WithDetail("target_host", detail.TargetHost)
		}
		return nil, appErr
	}
	return detail, nil
}

// buildSendOutput projects the entry just written through the same view every
// read command uses, then adds the two body locations.
func buildSendOutput(store *session.Store, event *session.Event) sendOutput {
	return sendOutput{
		View:             entry.Summarize(event),
		RequestBodyPath:  store.RequestBodyPath(event),
		ResponseBodyPath: store.ResponseBodyPath(event),
	}
}
