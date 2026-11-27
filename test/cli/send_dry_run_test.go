package cli_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func requireBase64Value(t *testing.T, value any) []byte {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("value = %T, want a base64 string", value)
	}
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		t.Fatalf("decode base64 value: %v", err)
	}
	return data
}

// requirePreview asserts the full dry-run preview contract: base64 of the
// first 8 KiB (or every byte when not truncated), the total request size, and
// the truncation flag.
func requirePreview(t *testing.T, out map[string]any, full []byte, truncated bool) []byte {
	t.Helper()
	want := full
	if truncated {
		want = full[:8<<10]
	}
	preview := requireBase64Value(t, out["request_bytes_preview"])
	if !bytes.Equal(preview, want) {
		t.Errorf("preview bytes = %q, want %q", preview, want)
	}
	if out["preview_total_bytes"] != float64(len(full)) {
		t.Errorf("preview_total_bytes = %v, want %d", out["preview_total_bytes"], len(full))
	}
	if out["preview_truncated"] != truncated {
		t.Errorf("preview_truncated = %v, want %v", out["preview_truncated"], truncated)
	}
	return preview
}

func TestDryRunURLRendersFinalRequestWithoutSending(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "send", "--url", "http://127.0.0.1:1/api/items?q=1", "--dry-run")
	requireExit(t, got, 0)
	out := decodeJSONObject(t, got.stdout)

	document, ok := out["request_document"].(map[string]any)
	if !ok {
		t.Fatalf("request_document = %T, want an object", out["request_document"])
	}
	if document["method"] != "GET" {
		t.Errorf("document method = %v, want GET", document["method"])
	}
	if document["url"] != "http://127.0.0.1:1/api/items?q=1" {
		t.Errorf("document url = %v", document["url"])
	}
	requireHeaderPairs(t, document["headers"], [][2]string{{"Host", "127.0.0.1:1"}})
	requireHeaderPairs(t, document["query"], [][2]string{{"q", "1"}})
	for _, field := range []string{"body", "body_base64", "body_file"} {
		if _, ok := document[field]; ok {
			t.Errorf("document has %q for a bodyless GET: %v", field, document)
		}
	}

	wantBytes := "GET /api/items?q=1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n"
	requirePreview(t, out, []byte(wantBytes), false)

	if _, err := os.Stat(env.store); !os.IsNotExist(err) {
		t.Errorf("send --dry-run touched the data dir: %v", err)
	}
}

func TestDryRunPatchRendersFinalDocumentAndBytes(t *testing.T) {
	env := newTestEnv(t)
	patch := `{"method":"POST","url":"http://127.0.0.1:1/final",` +
		`"headers":[["X-Dup","one"],["X-Dup","two"]],` +
		`"query":[["q","a b"]],"body":"hello"}`

	got := env.runWithStore(t, "send", "--url", "http://127.0.0.1:1/base?x=1", "--patch", patch, "--dry-run")
	requireExit(t, got, 0)
	out := decodeJSONObject(t, got.stdout)

	document, _ := out["request_document"].(map[string]any)
	if document["method"] != "POST" {
		t.Errorf("document method = %v, want POST", document["method"])
	}
	if document["url"] != "http://127.0.0.1:1/final?q=a+b" {
		t.Errorf("document url = %v", document["url"])
	}
	requireHeaderPairs(t, document["headers"], [][2]string{
		{"X-Dup", "one"},
		{"X-Dup", "two"},
		{"Content-Length", "5"},
		{"Host", "127.0.0.1:1"},
	})
	requireHeaderPairs(t, document["query"], [][2]string{{"q", "a b"}})
	if document["body"] != "hello" {
		t.Errorf("document body = %v, want hello", document["body"])
	}
	if _, ok := document["body_file"]; ok {
		t.Errorf("dry-run document must inline the body: %v", document)
	}

	wantBytes := "POST /final?q=a+b HTTP/1.1\r\n" +
		"X-Dup: one\r\n" +
		"X-Dup: two\r\n" +
		"Content-Length: 5\r\n" +
		"Host: 127.0.0.1:1\r\n" +
		"\r\n" +
		"hello"
	requirePreview(t, out, []byte(wantBytes), false)
}

func TestDryRunFromFilePreservesRawBytes(t *testing.T) {
	env := newTestEnv(t)
	raw := "POST http://127.0.0.1:1/raw?x=1 HTTP/1.1\r\n" +
		"Host: 127.0.0.1:1\r\n" +
		"X-A: one\r\n" +
		"Content-Length: 5\r\n" +
		"\r\n" +
		"hello"
	path := filepath.Join(t.TempDir(), "request.txt")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw request: %v", err)
	}

	got := env.runWithStore(t, "send", "--from-file", path, "--dry-run")
	requireExit(t, got, 0)
	out := decodeJSONObject(t, got.stdout)

	document, _ := out["request_document"].(map[string]any)
	if document["method"] != "POST" || document["url"] != "http://127.0.0.1:1/raw?x=1" {
		t.Errorf("document = %v", document)
	}
	requireHeaderPairs(t, document["headers"], [][2]string{
		{"Host", "127.0.0.1:1"},
		{"X-A", "one"},
		{"Content-Length", "5"},
	})
	requireHeaderPairs(t, document["query"], [][2]string{{"x", "1"}})
	if document["body"] != "hello" {
		t.Errorf("document body = %v, want hello", document["body"])
	}

	requirePreview(t, out, []byte(raw), false)
}

func TestDryRunBinaryBodyUsesBase64(t *testing.T) {
	env := newTestEnv(t)
	binary := []byte{0x00, 0x01, 0xfe, 0xff}
	raw := "POST http://127.0.0.1:1/bin HTTP/1.1\r\nHost: 127.0.0.1:1\r\nContent-Length: 4\r\n\r\n" + string(binary)
	path := filepath.Join(t.TempDir(), "request.bin")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw request: %v", err)
	}

	got := env.runWithStore(t, "send", "--from-file", path, "--dry-run")
	requireExit(t, got, 0)
	document, _ := decodeJSONObject(t, got.stdout)["request_document"].(map[string]any)
	want := base64.StdEncoding.EncodeToString(binary)
	if document["body_base64"] != want {
		t.Errorf("body_base64 = %v, want %v", document["body_base64"], want)
	}
	if _, ok := document["body"]; ok {
		t.Errorf("binary body must not use body: %v", document)
	}
}

func TestDryRunEntryDoesNotWriteHistoryOrBodies(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	rawRequest := "POST http://{{target}}/base?q=1 HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello"
	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)
	rewriteHistoryURL(t, env.store, id, "http://127.0.0.1:1/never?q=1")

	historyBefore, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	bodiesBefore := bodyFileNames(t, env.store)

	got := env.runWithStore(t, "send", id, "--dry-run")
	requireExit(t, got, 0)
	out := decodeJSONObject(t, got.stdout)
	document, _ := out["request_document"].(map[string]any)
	if document["url"] != "http://127.0.0.1:1/never?q=1" {
		t.Errorf("document url = %v, want the rewritten base URL", document["url"])
	}
	// A stored entry refers to its body by path, exactly as `show --detail`
	// does; only a request rendered in memory inlines its bytes.
	wantBodyPath := filepath.Join(env.store, "bodies", id+"-req.bin")
	if document["body_file"] != wantBodyPath {
		t.Errorf("document body_file = %v, want %s", document["body_file"], wantBodyPath)
	}
	for _, field := range []string{"body", "body_base64"} {
		if _, ok := document[field]; ok {
			t.Errorf("stored entry document inlined %q: %v", field, document)
		}
	}
	if body, err := os.ReadFile(wantBodyPath); err != nil || string(body) != "hello" {
		t.Errorf("stored request body = %q, %v; want hello", body, err)
	}

	historyAfter, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history after dry run: %v", err)
	}
	if string(historyAfter) != string(historyBefore) {
		t.Errorf("dry run changed history:\nbefore %s\nafter %s", historyBefore, historyAfter)
	}
	if got := bodyFileNames(t, env.store); !reflect.DeepEqual(got, bodiesBefore) {
		t.Errorf("dry run changed body files: before %v after %v", bodiesBefore, got)
	}
}

func bodyFileNames(t *testing.T, store string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store, "bodies"))
	if err != nil {
		t.Fatalf("read bodies dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestDryRunPreviewTruncatesAt8KiB(t *testing.T) {
	env := newTestEnv(t)
	const limit = 8 << 10
	head := func(bodyLength int) string {
		return fmt.Sprintf("POST http://127.0.0.1:1/big HTTP/1.1\r\n"+
			"Host: 127.0.0.1:1\r\n"+
			"Content-Length: %d\r\n"+
			"\r\n", bodyLength)
	}

	oversized := head(9000) + strings.Repeat("a", 9000)
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte(oversized), 0o600); err != nil {
		t.Fatalf("write oversized request: %v", err)
	}
	got := env.runWithStore(t, "send", "--from-file", path, "--dry-run")
	requireExit(t, got, 0)
	out := decodeJSONObject(t, got.stdout)
	preview := requirePreview(t, out, []byte(oversized), true)
	if len(preview) != limit {
		t.Errorf("preview length = %d, want %d", len(preview), limit)
	}

	bodyLength := limit - len(head(9000))
	exact := head(bodyLength) + strings.Repeat("b", bodyLength)
	if len(exact) != limit {
		t.Fatalf("fixture length = %d, want %d", len(exact), limit)
	}
	exactPath := filepath.Join(t.TempDir(), "exact.txt")
	if err := os.WriteFile(exactPath, []byte(exact), 0o600); err != nil {
		t.Fatalf("write exact request: %v", err)
	}
	exactOut := env.runWithStore(t, "send", "--from-file", exactPath, "--dry-run")
	requireExit(t, exactOut, 0)
	exactResult := decodeJSONObject(t, exactOut.stdout)
	exactPreview := requirePreview(t, exactResult, []byte(exact), false)
	if len(exactPreview) != limit {
		t.Errorf("exact preview length = %d, want %d", len(exactPreview), limit)
	}
}

func TestDryRunRequiresAValidBaseAndPatch(t *testing.T) {
	env := newTestEnv(t)

	missing := env.runWithStore(t, "send", "--dry-run")
	requireExit(t, missing, 2)
	if code := errorEnvelope(t, missing)["code"]; code != "BASE_CONFLICT" {
		t.Errorf("missing base code = %v, want BASE_CONFLICT", code)
	}

	patchWithoutBase := env.runWithStore(t, "send", "--dry-run", "--patch", `{}`)
	requireExit(t, patchWithoutBase, 2)
	if code := errorEnvelope(t, patchWithoutBase)["code"]; code != "BASE_CONFLICT" {
		t.Errorf("patch without base code = %v, want BASE_CONFLICT", code)
	}

	invalidPatch := env.runWithStore(t, "send", "--url", "http://127.0.0.1:1/", "--patch", `{"method":""}`, "--dry-run")
	requireExit(t, invalidPatch, 2)
	if code := errorEnvelope(t, invalidPatch)["code"]; code != "INVALID_PATCH" {
		t.Errorf("invalid patch code = %v, want INVALID_PATCH", code)
	}
}
