package cli_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSendSchemaOutputsRequestDocumentSchemaAlone(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "send", "--schema")
	requireExit(t, got, 0)

	schema := decodeJSONObject(t, got.stdout)
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("schema $schema = %v", schema["$schema"])
	}
	if id, _ := schema["$id"].(string); !strings.Contains(id, "request-document") {
		t.Errorf("schema $id = %v, want a request-document id", schema["$id"])
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, field := range []string{"method", "url", "headers", "query", "body", "body_base64", "body_file"} {
		if _, ok := properties[field]; !ok {
			t.Errorf("schema properties missing %q: %v", field, properties)
		}
	}
	if _, err := os.Stat(env.store); !os.IsNotExist(err) {
		t.Errorf("send --schema touched the data dir: %v", err)
	}
}

func TestSendSchemaRejectsCompanions(t *testing.T) {
	env := newTestEnv(t)
	cases := [][]string{
		{"send", "--schema", "entry-id"},
		{"send", "--schema", "--url", "http://example.test/"},
		{"send", "--schema", "--from-file", "request.txt"},
		{"send", "--schema", "--patch", `{}`},
		{"send", "--schema", "--dry-run"},
		{"send", "--schema", "--var", "A=1"},
	}
	for _, args := range cases {
		got := env.runWithStore(t, args...)
		requireExit(t, got, 2)
		if code := errorEnvelope(t, got)["code"]; code != "INVALID_REQUEST" {
			t.Errorf("%v code = %v, want INVALID_REQUEST", args, code)
		}
	}
}

func TestSendDryRunAndSchemaRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	send, ok := doc.Command.find("mitt send")
	if !ok {
		t.Fatal("spec missing mitt send")
	}
	for _, name := range []string{"dry-run", "schema"} {
		flag, ok := flagByName(send.Flags, name)
		if !ok || flag.Type != "bool" {
			t.Errorf("send --%s flag = %+v, want a bool flag", name, flag)
		}
	}
	if got := doc.Limits["request_bytes_preview_max_bytes"]; got != 8<<10 {
		t.Errorf("spec limits request_bytes_preview_max_bytes = %v, want %d", got, 8<<10)
	}

	dryRunSchema, ok := doc.Schemas["send_dry_run"].(map[string]any)
	if !ok {
		t.Fatalf("spec schemas missing send_dry_run: %v", doc.Schemas)
	}
	if dryRunSchema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("send_dry_run schema $schema = %v", dryRunSchema["$schema"])
	}
	requireSchemaFields(t, dryRunSchema,
		"request_document", "request_bytes_preview", "preview_total_bytes", "preview_truncated")
}

func TestSendSchemaValidatesShowAndDryRunDocuments(t *testing.T) {
	env := newTestEnv(t)
	specDoc := loadSpec(t, env.run(t, "spec"))

	emit := env.runWithStore(t, "send", "--schema")
	requireExit(t, emit, 0)
	requestSchema := decodeJSONObject(t, emit.stdout)
	registered, ok := specDoc.Schemas["request_document"].(map[string]any)
	if !ok {
		t.Fatalf("spec schemas missing request_document: %v", specDoc.Schemas)
	}
	if !reflect.DeepEqual(requestSchema, registered) {
		t.Errorf("send --schema differs from spec schemas.request_document")
	}
	dryRunSchema, ok := specDoc.Schemas["send_dry_run"].(map[string]any)
	if !ok {
		t.Fatalf("spec schemas missing send_dry_run: %v", specDoc.Schemas)
	}

	text := env.runWithStore(t, "send", "--url", "http://127.0.0.1:1/schema",
		"--patch", `{"method":"PUT","headers":[["X-A","1"]],"body":"hello"}`, "--dry-run")
	requireExit(t, text, 0)
	textOut := decodeJSONObject(t, text.stdout)
	requireSchemaValid(t, dryRunSchema, textOut, "send --dry-run output")
	requireSchemaValid(t, requestSchema, textOut["request_document"], "dry-run request_document")

	binary := []byte{0x00, 0x01, 0xfe, 0xff}
	path := filepath.Join(t.TempDir(), "request.bin")
	raw := "POST http://127.0.0.1:1/bin HTTP/1.1\r\nHost: 127.0.0.1:1\r\nContent-Length: 4\r\n\r\n" + string(binary)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw request: %v", err)
	}
	binaryOut := env.runWithStore(t, "send", "--from-file", path, "--dry-run")
	requireExit(t, binaryOut, 0)
	binaryObject := decodeJSONObject(t, binaryOut.stdout)
	requireSchemaValid(t, requestSchema, binaryObject["request_document"], "binary dry-run request_document")

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	rawRequest := "POST http://{{target}}/docs?x=1 HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Content-Length: 11\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello world"
	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)
	show := env.runWithStore(t, "show", id, "--detail")
	requireExit(t, show, 0)
	showDocument, ok := decodeJSONObject(t, show.stdout)["request_document"].(map[string]any)
	if !ok {
		t.Fatal("show --detail has no request_document")
	}
	requireSchemaValid(t, requestSchema, showDocument, "show request_document")
}

// TestRequestDocumentSchemaAndRuntimeAgreeOnBodies is the acceptance check for
// the request-document deepening: one dataset of body shapes is judged twice,
// once by the schema `send --schema` publishes and once by the runtime that
// applies `--patch`. A document the schema rejects must be refused as
// INVALID_PATCH, and a document the schema accepts must be accepted. The two
// cases the schema cannot express are asserted as well, so the gap stays named
// instead of silently growing.
func TestRequestDocumentSchemaAndRuntimeAgreeOnBodies(t *testing.T) {
	env := newTestEnv(t)
	schema := decodeJSONObject(t, env.runWithStore(t, "send", "--schema").stdout)

	bodyFile := filepath.Join(t.TempDir(), "body.bin")
	if err := os.WriteFile(bodyFile, []byte("from file"), 0o600); err != nil {
		t.Fatalf("write body file: %v", err)
	}

	cases := []struct {
		name string
		body map[string]any
		// valid is the verdict both the schema and the runtime must reach.
		valid bool
		// schemaExpressible is false for the body shapes JSON Schema cannot
		// check (file existence, base64 payload): there the schema accepts and
		// only the runtime rejects.
		schemaExpressible bool
	}{
		{name: "no representation", body: map[string]any{}, valid: true, schemaExpressible: true},
		{
			name:              "explicit nulls",
			body:              map[string]any{"body": nil, "body_base64": nil, "body_file": nil},
			valid:             true,
			schemaExpressible: true,
		},
		{name: "text", body: map[string]any{"body": "hello"}, valid: true, schemaExpressible: true},
		{name: "empty string", body: map[string]any{"body": ""}, valid: true, schemaExpressible: true},
		{
			name:              "base64",
			body:              map[string]any{"body_base64": base64.StdEncoding.EncodeToString([]byte{0x00, 0xff})},
			valid:             true,
			schemaExpressible: true,
		},
		{name: "file", body: map[string]any{"body_file": bodyFile}, valid: true, schemaExpressible: true},
		{
			name:              "two representations",
			body:              map[string]any{"body": "x", "body_base64": "eA=="},
			valid:             false,
			schemaExpressible: true,
		},
		{
			name:              "three representations",
			body:              map[string]any{"body": "x", "body_base64": "eA==", "body_file": bodyFile},
			valid:             false,
			schemaExpressible: true,
		},
		{
			name:              "missing file",
			body:              map[string]any{"body_file": filepath.Join(t.TempDir(), "absent.bin")},
			valid:             false,
			schemaExpressible: false,
		},
		{
			name:              "invalid base64",
			body:              map[string]any{"body_base64": "!!!"},
			valid:             false,
			schemaExpressible: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The document is built through JSON so the schema validator sees
			// the wire shapes (arrays and objects), not Go slices and maps.
			document := map[string]any{
				"method":  "POST",
				"url":     "http://127.0.0.1:1/schema",
				"headers": []any{[]any{"X-Test", "1"}},
				"query":   []any{},
			}
			for key, value := range tc.body {
				document[key] = value
			}

			problems := validateJSONSchema(schema, schema, document, "$")
			schemaAccepts := len(problems) == 0
			wantSchema := tc.valid || !tc.schemaExpressible
			if schemaAccepts != wantSchema {
				t.Errorf("schema verdict = %v (problems: %v), want accepts=%v", schemaAccepts, problems, wantSchema)
			}

			patch, err := json.Marshal(document)
			if err != nil {
				t.Fatalf("encode patch: %v", err)
			}
			got := env.runWithStore(t, "send", "--url", "http://127.0.0.1:1/base",
				"--patch", string(patch), "--dry-run")
			if tc.valid {
				requireExit(t, got, 0)
				rendered := decodeJSONObject(t, got.stdout)["request_document"]
				requireSchemaValid(t, schema, rendered, "dry-run request_document")
				return
			}
			requireExit(t, got, 2)
			if code := errorEnvelope(t, got)["code"]; code != "INVALID_PATCH" {
				t.Errorf("runtime code = %v, want INVALID_PATCH", code)
			}
		})
	}
}
