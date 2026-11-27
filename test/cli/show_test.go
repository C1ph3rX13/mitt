package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureEntry starts a capture proxy, sends one raw request through it, stops
// the proxy, and returns the id of the single captured history entry plus the
// target address and the exact response the proxy returned.
func captureEntry(t *testing.T, env *testEnv, targetResponse, rawRequest string) (string, string, string) {
	t.Helper()
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)
	request := strings.ReplaceAll(rawRequest, "{{target}}", targetAddr)
	response := sendRaw(t, proxyAddr, request)
	waitObserved(t, requests)
	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	got := env.runWithStore(t, "history")
	requireExit(t, got, 0)
	lines := historyLines(got.stdout)
	if len(lines) != 1 {
		t.Fatalf("history returned %d lines, want 1:\n%s", len(lines), got.stdout)
	}
	summary := decodeJSONObject(t, lines[0])
	id, _ := summary["id"].(string)
	if id == "" {
		t.Fatalf("history summary has no id: %v", summary)
	}
	return id, targetAddr, response
}

func TestShowSummaryIncludesRequestAndResponse(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 201 Created\r\n" +
		"X-Server: raw-target\r\n" +
		"X-Repeat: first\r\n" +
		"X-Repeat: second\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"world"
	rawRequest := "POST http://{{target}}/api/items?q=1 HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"X-Beta: two\r\n" +
		"X-Repeater-Context: flow-a\r\n" +
		"X-Alpha: one\r\n" +
		"X-Beta: three\r\n" +
		"Content-Length: 11\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello world"

	id, targetAddr, _ := captureEntry(t, env, targetResponse, rawRequest)

	got := env.runWithStore(t, "show", id)
	requireExit(t, got, 0)
	summary := decodeJSONObject(t, got.stdout)

	if summary["id"] != id || summary["kind"] != "captured" {
		t.Errorf("id/kind = %v/%v, want %s/captured", summary["id"], summary["kind"], id)
	}
	if summary["method"] != "POST" {
		t.Errorf("method = %v, want POST", summary["method"])
	}
	if summary["url"] != "http://"+targetAddr+"/api/items?q=1" {
		t.Errorf("url = %v, want the absolute request target", summary["url"])
	}
	if summary["status"] != float64(201) {
		t.Errorf("status = %v, want 201", summary["status"])
	}
	if summary["context"] != "flow-a" {
		t.Errorf("context = %v, want flow-a", summary["context"])
	}
	if summary["http_version"] != "http/1.1" {
		t.Errorf("http_version = %v, want http/1.1", summary["http_version"])
	}
	if summary["req_body_size"] != float64(len("hello world")) {
		t.Errorf("req_body_size = %v, want %d", summary["req_body_size"], len("hello world"))
	}
	if summary["resp_body_size"] != float64(len("world")) {
		t.Errorf("resp_body_size = %v, want %d", summary["resp_body_size"], len("world"))
	}
	requireHeaderPairs(t, summary["request_headers"], [][2]string{
		{"Host", targetAddr},
		{"X-Beta", "two"},
		{"X-Alpha", "one"},
		{"X-Beta", "three"},
		{"Content-Length", "11"},
		{"Connection", "close"},
	})
	requireHeaderPairs(t, summary["response_headers"], [][2]string{
		{"X-Server", "raw-target"},
		{"X-Repeat", "first"},
		{"X-Repeat", "second"},
		{"Content-Length", "5"},
		{"Connection", "close"},
	})
	if _, ok := summary["request_document"]; ok {
		t.Error("default summary must not inline request_document; use --detail")
	}
	if _, ok := summary["request_body_path"]; ok {
		t.Error("default summary must not expose body paths; use --detail")
	}
}

func TestShowDetailIncludesBodyPathsAndRequestDocument(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"X-Server: raw-target\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"ok"
	rawRequest := "POST http://{{target}}/api/items?q=1&q=2&empty= HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"X-Beta: two\r\n" +
		"X-Repeater-Context: flow-a\r\n" +
		"X-Beta: three\r\n" +
		"Content-Length: 11\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello world"

	id, targetAddr, _ := captureEntry(t, env, targetResponse, rawRequest)

	got := env.runWithStore(t, "show", id, "--detail")
	requireExit(t, got, 0)
	detail := decodeJSONObject(t, got.stdout)

	requestPath := filepath.Join(env.store, "bodies", id+"-req.bin")
	responsePath := filepath.Join(env.store, "bodies", id+"-resp.bin")
	if detail["request_body_path"] != requestPath {
		t.Errorf("request_body_path = %v, want %s", detail["request_body_path"], requestPath)
	}
	if detail["response_body_path"] != responsePath {
		t.Errorf("response_body_path = %v, want %s", detail["response_body_path"], responsePath)
	}
	for path, want := range map[string]string{
		requestPath:  "hello world",
		responsePath: "ok",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", path, body, want)
		}
	}

	document, ok := detail["request_document"].(map[string]any)
	if !ok {
		t.Fatalf("request_document = %T, want an object", detail["request_document"])
	}
	if document["method"] != "POST" {
		t.Errorf("request_document.method = %v, want POST", document["method"])
	}
	if document["url"] != "http://"+targetAddr+"/api/items?q=1&q=2&empty=" {
		t.Errorf("request_document.url = %v, want the absolute URL", document["url"])
	}
	requireHeaderPairs(t, document["headers"], [][2]string{
		{"Host", targetAddr},
		{"X-Beta", "two"},
		{"X-Beta", "three"},
		{"Content-Length", "11"},
		{"Connection", "close"},
	})
	requireHeaderPairs(t, document["query"], [][2]string{
		{"q", "1"},
		{"q", "2"},
		{"empty", ""},
	})
	if document["body_file"] != requestPath {
		t.Errorf("request_document.body_file = %v, want %s", document["body_file"], requestPath)
	}
	if _, ok := document["body"]; ok {
		t.Error("request_document must not inline a textual body")
	}
	if _, ok := document["body_base64"]; ok {
		t.Error("request_document must not inline a base64 body")
	}
}

func TestShowRawReconstructsForwardedBytes(t *testing.T) {
	env := newTestEnv(t)
	requestBody := []byte{0x00, 0x01, 0xff, 'a', 'b'}
	responseBody := []byte{0xde, 0xad, 0x00, 0x7f}
	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"X-R: one\r\n" +
		"X-Other: value\r\n" +
		"X-R: two\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(responseBody)) +
		"Connection: close\r\n" +
		"\r\n" +
		string(responseBody)
	rawRequest := "POST http://{{target}}/raw?x=%2F HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"X-Dup: one\r\n" +
		"X-Other: value\r\n" +
		"X-Dup: two\r\n" +
		"X-Repeater-Context: flow-a\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(requestBody)) +
		"Connection: close\r\n" +
		"\r\n" +
		string(requestBody)

	id, targetAddr, response := captureEntry(t, env, targetResponse, rawRequest)
	if response != targetResponse {
		t.Fatalf("captured client response differs from fixture:\n got %q\nwant %q", response, targetResponse)
	}

	got := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, got, 0)

	wantRequest := "POST /raw?x=%2F HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"X-Dup: one\r\n" +
		"X-Other: value\r\n" +
		"X-Dup: two\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(requestBody)) +
		"Connection: close\r\n" +
		"\r\n" +
		string(requestBody)
	want := wantRequest + targetResponse
	if got.stdout != want {
		t.Fatalf("show --raw bytes differ:\n got %q\nwant %q", got.stdout, want)
	}
	if strings.Contains(got.stdout, "X-Repeater-Context") {
		t.Error("show --raw must not reintroduce X-Repeater-Context")
	}
}

func TestShowRawReencodesChunkedMessages(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"3\r\nabc\r\n" +
		"2\r\nde\r\n" +
		"0\r\n\r\n"
	rawRequest := "POST http://{{target}}/chunked HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n" +
		"6\r\n world\r\n" +
		"0\r\n\r\n"

	id, targetAddr, _ := captureEntry(t, env, targetResponse, rawRequest)

	got := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, got, 0)

	want := "POST /chunked HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"b\r\nhello world\r\n" +
		"0\r\n\r\n" +
		"HTTP/1.1 200 OK\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nabcde\r\n" +
		"0\r\n\r\n"
	if got.stdout != want {
		t.Fatalf("chunked show --raw bytes differ:\n got %q\nwant %q", got.stdout, want)
	}
}

func TestShowMissingEntryAndRawConflicts(t *testing.T) {
	env := newTestEnv(t)

	missing := env.runWithStore(t, "show", "does-not-exist")
	requireExit(t, missing, 66)
	if code := errorEnvelope(t, missing)["code"]; code != "ENTRY_NOT_FOUND" {
		t.Errorf("missing entry code = %v, want ENTRY_NOT_FOUND", code)
	}

	missingDetail := env.runWithStore(t, "show", "does-not-exist", "--detail")
	requireExit(t, missingDetail, 66)
	if code := errorEnvelope(t, missingDetail)["code"]; code != "ENTRY_NOT_FOUND" {
		t.Errorf("missing entry --detail code = %v, want ENTRY_NOT_FOUND", code)
	}

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	id, _, _ := captureEntry(t, env, targetResponse,
		"GET http://{{target}}/conflict HTTP/1.1\r\nHost: {{target}}\r\nConnection: close\r\n\r\n")

	conflict := env.runWithStore(t, "show", id, "--raw", "--detail")
	requireExit(t, conflict, 2)
	if code := errorEnvelope(t, conflict)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("raw+detail code = %v, want INVALID_REQUEST", code)
	}
	// Presence is what a constraint reads, matching pflag's Changed: spelling
	// --detail=false still counts as giving the flag, exactly as send --schema=false
	// still counts as asking for the schema.
	explicitFalse := env.runWithStore(t, "show", id, "--raw", "--detail=false")
	requireExit(t, explicitFalse, 2)
	if code := errorEnvelope(t, explicitFalse)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("raw+--detail=false code = %v, want INVALID_REQUEST", code)
	}

	noID := env.runWithStore(t, "show")
	requireExit(t, noID, 2)
}

func TestShowRendersMITMError(t *testing.T) {
	env := newTestEnv(t)
	if err := os.MkdirAll(env.store, 0o700); err != nil {
		t.Fatalf("create store: %v", err)
	}
	event := `{"id":"error-entry","ts":"2025-01-02T03:04:05.123456789Z","kind":"mitm_error","target_host":"blocked.test","reason":"scope_denied","context":"alpha","details":{"matched":"deny"}}`
	if err := os.WriteFile(filepath.Join(env.store, "history.jsonl"), []byte(event+"\n"), 0o600); err != nil {
		t.Fatalf("write history: %v", err)
	}

	got := env.runWithStore(t, "show", "error-entry")
	requireExit(t, got, 0)
	summary := decodeJSONObject(t, got.stdout)
	if summary["kind"] != "mitm_error" || summary["target_host"] != "blocked.test" || summary["reason"] != "scope_denied" {
		t.Errorf("mitm_error summary = %v", summary)
	}
	if summary["status"] != nil {
		t.Errorf("mitm_error status = %v, want null", summary["status"])
	}
	details, _ := summary["details"].(map[string]any)
	if details["matched"] != "deny" {
		t.Errorf("mitm_error details = %v, want matched=deny", summary["details"])
	}

	raw := env.runWithStore(t, "show", "error-entry", "--raw")
	requireExit(t, raw, 2)
	if code := errorEnvelope(t, raw)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("mitm_error --raw code = %v, want INVALID_REQUEST", code)
	}
}

func TestShowRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	show, ok := doc.Command.find("mitt show")
	if !ok {
		t.Fatal("spec missing mitt show")
	}
	if len(show.Args) != 1 || show.Args[0].Name != "id" || !show.Args[0].Required {
		t.Errorf("show args = %v, want required id positional", show.Args)
	}
	for _, name := range []string{"raw", "detail", "store", "format"} {
		if _, ok := flagByName(show.Flags, name); !ok {
			t.Errorf("show missing --%s", name)
		}
	}
	if !containsString(show.ErrorCodes, "ENTRY_NOT_FOUND") {
		t.Errorf("show error_codes = %v, want ENTRY_NOT_FOUND", show.ErrorCodes)
	}
	if show.OutputSchema == nil {
		t.Fatal("show must register a default success output schema")
	}
	if show.OutputSchema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("show output schema $schema = %v", show.OutputSchema["$schema"])
	}
	if properties, _ := show.OutputSchema["properties"].(map[string]any); properties["request_document"] != nil {
		t.Errorf("the default show summary must not carry detail fields: %v", properties)
	}
	detailMode, ok := show.OutputSchemas["detail"]
	if !ok {
		t.Fatalf("show output_schemas = %v, want a detail mode", show.OutputSchemas)
	}
	if detailMode.Flag != "--detail" {
		t.Errorf("show detail selector = %q, want --detail", detailMode.Flag)
	}
	properties, _ := detailMode.Schema["properties"].(map[string]any)
	for _, field := range []string{"request_document", "request_body_path", "response_body_path"} {
		if _, ok := properties[field]; !ok {
			t.Errorf("show detail schema missing %s: %v", field, properties)
		}
	}
	rawMode, ok := show.OutputSchemas["raw"]
	if !ok {
		t.Fatalf("show output_schemas = %v, want a raw mode", show.OutputSchemas)
	}
	if !rawMode.Opaque || rawMode.Flag != "--raw" {
		t.Errorf("show raw mode = %+v, want an opaque --raw shape", rawMode)
	}

	registered, ok := doc.Schemas["request_document"].(map[string]any)
	if !ok {
		t.Fatalf("spec schemas missing request_document: %v", doc.Schemas)
	}
	if registered["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("request_document schema $schema = %v", registered["$schema"])
	}
	if got := registered["$id"]; got == nil || !strings.Contains(got.(string), "request-document") {
		t.Errorf("request_document schema $id = %v", got)
	}
}

// TestShowSchemaDocumentsEveryMITMReason keeps the published reason list tied
// to the proxy's failure vocabulary. The hand-written description named a
// subset of the reasons and drifted when h2_only_target was added.
func TestShowSchemaDocumentsEveryMITMReason(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	show, ok := doc.Command.find("mitt show")
	if !ok {
		t.Fatal("spec missing mitt show")
	}
	properties, _ := show.OutputSchema["properties"].(map[string]any)
	reason, _ := properties["reason"].(map[string]any)
	description, _ := reason["description"].(string)
	if description == "" {
		t.Fatalf("show schema reason has no description: %v", reason)
	}
	for _, want := range []string{
		"scope_denied",
		"client_cert_rejected",
		"upstream_tls",
		"upstream_connect",
		"not_tls",
		"leaf_certificate",
		"h2_only_target",
		"h2_parse_error",
	} {
		if !strings.Contains(description, want) {
			t.Errorf("show schema reason description is missing %q: %s", want, description)
		}
	}
}

func TestShowRawHandlesCloseDelimitedResponse(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"X-Server: raw-target\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"until-close"
	rawRequest := "GET http://{{target}}/close HTTP/1.1\r\nHost: {{target}}\r\nConnection: close\r\n\r\n"

	id, targetAddr, _ := captureEntry(t, env, targetResponse, rawRequest)
	got := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, got, 0)

	want := "GET /close HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		targetResponse
	if got.stdout != want {
		t.Fatalf("close-delimited show --raw bytes differ:\n got %q\nwant %q", got.stdout, want)
	}
}

func TestShowRawPreservesBinaryBodies(t *testing.T) {
	env := newTestEnv(t)
	binary := string([]byte{0x00, 0x01, 0x02, 0xff, 0xfe})
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\n" + binary
	rawRequest := "POST http://{{target}}/binary HTTP/1.1\r\n" +
		"Host: {{target}}\r\nContent-Length: 5\r\nConnection: close\r\n\r\n" + binary

	id, targetAddr, _ := captureEntry(t, env, targetResponse, rawRequest)
	got := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, got, 0)

	want := "POST /binary HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		binary +
		targetResponse
	if got.stdout != want {
		t.Fatalf("binary show --raw bytes differ:\n got %v\nwant %v", []byte(got.stdout), []byte(want))
	}
}
