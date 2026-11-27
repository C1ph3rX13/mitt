package cli_test

import (
	"reflect"
	"testing"
)

// TestSpecKindEnumsMatchWhatEachCommandEmits pins the schema side of the entry
// deepening: the kind vocabulary exists once (session.Kinds), and each command
// publishes exactly the subset it can write. A `send` result cannot be a
// mitm_error, and a compared side in `diff` always has a response.
func TestSpecKindEnumsMatchWhatEachCommandEmits(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	enumOf := func(schema map[string]any, property string) []string {
		t.Helper()
		properties, _ := schema["properties"].(map[string]any)
		field, _ := properties[property].(map[string]any)
		values, _ := field["enum"].([]any)
		out := make([]string, 0, len(values))
		for _, value := range values {
			text, _ := value.(string)
			out = append(out, text)
		}
		return out
	}

	show, ok := doc.Command.find("mitt show")
	if !ok || show.OutputSchema == nil {
		t.Fatal("spec missing mitt show output schema")
	}
	wantAll := []string{"captured", "replayed", "sent", "mitm_error"}
	if got := enumOf(show.OutputSchema, "kind"); !reflect.DeepEqual(got, wantAll) {
		t.Errorf("show kind enum = %v, want every event kind %v", got, wantAll)
	}

	// The report's complaint was that Event.Mutations and
	// Event.ForwardedRequestLine never reached any output DTO. Both are now
	// published, so an Agent can see them from `spec` before running anything.
	properties, _ := show.OutputSchema["properties"].(map[string]any)
	for _, field := range []string{"mutations", "forwarded_request_line"} {
		if _, ok := properties[field]; !ok {
			t.Errorf("show output schema missing %q: %v", field, properties)
		}
	}

	send, ok := doc.Command.find("mitt send")
	if !ok || send.OutputSchema == nil {
		t.Fatal("spec missing mitt send output schema")
	}
	if got := enumOf(send.OutputSchema, "kind"); !reflect.DeepEqual(got, []string{"sent", "replayed"}) {
		t.Errorf("send kind enum = %v, want only the kinds send writes", got)
	}

	diffCmd, ok := doc.Command.find("mitt diff")
	if !ok || diffCmd.OutputSchema == nil {
		t.Fatal("spec missing mitt diff output schema")
	}
	for _, side := range []string{"a", "b"} {
		properties, _ := diffCmd.OutputSchema["properties"].(map[string]any)
		entrySchema, ok := properties[side].(map[string]any)
		if !ok {
			t.Fatalf("diff output schema missing %q", side)
		}
		if got := enumOf(entrySchema, "kind"); !reflect.DeepEqual(got, []string{"captured", "replayed", "sent"}) {
			t.Errorf("diff %s.kind enum = %v, want the exchange kinds only", side, got)
		}
	}
}

// TestSpecDetailAndDryRunShareTheRequestDocumentShape pins the one
// materialization policy: the request document is the same type everywhere, so
// the two published schemas must be structurally identical.
func TestSpecDetailAndDryRunShareTheRequestDocumentShape(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	show, ok := doc.Command.find("mitt show")
	if !ok {
		t.Fatal("spec missing mitt show")
	}
	detail, ok := show.OutputSchemas["detail"]
	if !ok {
		t.Fatalf("show output_schemas = %v, want detail", show.OutputSchemas)
	}
	send, ok := doc.Command.find("mitt send")
	if !ok {
		t.Fatal("spec missing mitt send")
	}
	dryRun, ok := send.OutputSchemas["dry-run"]
	if !ok {
		t.Fatalf("send output_schemas = %v, want dry-run", send.OutputSchemas)
	}

	detailProperties, _ := detail.Schema["properties"].(map[string]any)
	dryRunProperties, _ := dryRun.Schema["properties"].(map[string]any)
	detailDocument, _ := detailProperties["request_document"].(map[string]any)
	dryRunDocument, _ := dryRunProperties["request_document"].(map[string]any)
	if detailDocument == nil || dryRunDocument == nil {
		t.Fatalf("request_document schema missing: detail=%v dry-run=%v", detailDocument, dryRunDocument)
	}

	// Nullability is the one legitimate difference: a mitm_error detail has no
	// request at all, while a dry-run always renders one. Everything else —
	// the fields, their order-insensitive shape, and the one-body rule — comes
	// from the same type.
	detailTypes, _ := detailDocument["type"].([]any)
	if len(detailTypes) != 2 || detailTypes[0] != "object" || detailTypes[1] != "null" {
		t.Errorf("show --detail request_document type = %v, want [object null]", detailDocument["type"])
	}
	if dryRunDocument["type"] != "object" {
		t.Errorf("send --dry-run request_document type = %v, want object", dryRunDocument["type"])
	}
	delete(detailDocument, "type")
	delete(dryRunDocument, "type")
	if !reflect.DeepEqual(detailDocument, dryRunDocument) {
		t.Errorf("request_document differs between show --detail and send --dry-run beyond nullability:\n detail: %v\n dry-run: %v", detailDocument, dryRunDocument)
	}
}
