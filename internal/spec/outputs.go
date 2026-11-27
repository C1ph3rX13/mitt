package spec

import (
	"github.com/C1ph3rX13/mitt/internal/diff"
	"github.com/C1ph3rX13/mitt/internal/entry"
)

const requestDocumentNote = "At most one of body, body_base64, and body_file may be present and non-null; allOf enforces this rule and null counts as absent."

// RequestDocumentSchema is the draft 2020-12 JSON Schema for the shared
// request_document shape used by `show --detail` and `send --schema`.
func RequestDocumentSchema() map[string]any {
	return Describe(
		SchemaOf[entry.Document]("request-document", "request document"),
		requestDocumentNote,
	)
}

// SendDiffAgainstSchema is the draft 2020-12 JSON Schema for the compact
// comparison attached to `send --diff-against` output.
func SendDiffAgainstSchema() map[string]any {
	return Describe(
		SchemaOf[diff.Summary]("send-diff-against", "send --diff-against summary"),
		"Compact response comparison attached by `mitt send --diff-against`.",
	)
}

// MetaSchema registers the output shape of a command that prints a JSON
// Schema: a document that conforms to the JSON Schema meta-schema.
func MetaSchema(mode string) Output {
	return Output{
		Mode:   mode,
		Schema: map[string]any{"$ref": draft202012},
	}
}

// sharedSchemas seeds the named registry that predates per-output
// registration. Both entries derive from the same types that the commands
// embed, so the registry cannot drift from the command output schemas.
func sharedSchemas() map[string]any {
	return map[string]any{
		"request_document":  RequestDocumentSchema(),
		"send_diff_against": SendDiffAgainstSchema(),
	}
}
