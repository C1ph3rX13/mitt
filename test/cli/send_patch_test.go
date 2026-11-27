package cli_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mustJSON encodes one value as an inline --patch argument.
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}
	return string(data)
}

func TestSendPatchRendersEditedDocument(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	patch := `{"method":"POST","url":"http://` + targetAddr + `/new?y=2",` +
		`"headers":[["X-Dup","one"],["X-Other","value"],["X-Dup","two"]],` +
		`"body":"payload"}`
	got := env.runWithStore(t, "send", "--url", "http://"+targetAddr+"/old?x=1", "--patch", patch)
	requireExit(t, got, 0)

	observed := waitObserved(t, requests)
	if observed.line != "POST /new?y=2 HTTP/1.1" {
		t.Errorf("target request line = %q, want POST /new?y=2 HTTP/1.1", observed.line)
	}
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"X-Dup", "one"},
		{"X-Other", "value"},
		{"X-Dup", "two"},
		{"Content-Length", "7"},
		{"Host", targetAddr},
	})
	if string(observed.body) != "payload" {
		t.Errorf("target body = %q, want payload", observed.body)
	}

	out := decodeJSONObject(t, got.stdout)
	if out["kind"] != "sent" || out["method"] != "POST" {
		t.Errorf("kind/method = %v/%v, want sent/POST", out["kind"], out["method"])
	}
	if out["url"] != "http://"+targetAddr+"/new?y=2" {
		t.Errorf("url = %v, want the patched URL", out["url"])
	}
	if out["request_line"] != "POST /new?y=2 HTTP/1.1" {
		t.Errorf("request_line = %v", out["request_line"])
	}
	requireHeaderPairs(t, out["request_headers"], [][2]string{
		{"X-Dup", "one"},
		{"X-Other", "value"},
		{"X-Dup", "two"},
		{"Content-Length", "7"},
		{"Host", targetAddr},
	})
	if out["req_body_size"] != float64(7) || out["status"] != float64(200) {
		t.Errorf("req_body_size/status = %v/%v, want 7/200", out["req_body_size"], out["status"])
	}
	if body, err := os.ReadFile(out["request_body_path"].(string)); err != nil || string(body) != "payload" {
		t.Errorf("stored request body = %q (%v), want payload", body, err)
	}
}

func TestSendPatchReplacesHeadersWholesale(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	got := env.runWithStore(t, "send", "--url", "http://"+targetAddr+"/headers",
		"--patch", `{"headers":[["X-A","1"],["X-A","2"]]}`)
	requireExit(t, got, 0)
	observed := waitObserved(t, requests)
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"X-A", "1"},
		{"X-A", "2"},
		{"Host", targetAddr},
	})

	cleared := env.runWithStore(t, "send", "--url", "http://"+targetAddr+"/cleared",
		"--patch", `{"headers":null}`)
	requireExit(t, cleared, 0)
	observed = waitObserved(t, requests)
	requireRawHeaderPairs(t, observed.headers, [][2]string{{"Host", targetAddr}})
}

func TestSendPatchBodyRepresentations(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	base := "http://" + targetAddr + "/body"

	binaryBody := []byte{0x00, 0x01, 0xfe, 0xff, 'A'}
	filePath := filepath.Join(t.TempDir(), "body.bin")
	if err := os.WriteFile(filePath, []byte("from file"), 0o600); err != nil {
		t.Fatalf("write body file: %v", err)
	}

	cases := []struct {
		name  string
		patch string
		want  []byte
	}{
		{"text", `{"body":"hello"}`, []byte("hello")},
		{"base64", `{"body_base64":"` + base64.StdEncoding.EncodeToString(binaryBody) + `"}`, binaryBody},
		{"file", mustJSON(t, map[string]any{"body_file": filePath}), []byte("from file")},
	}
	for _, tc := range cases {
		got := env.runWithStore(t, "send", "--url", base, "--patch", tc.patch)
		requireExit(t, got, 0)
		observed := waitObserved(t, requests)
		if string(observed.body) != string(tc.want) {
			t.Errorf("%s target body = %q, want %q", tc.name, observed.body, tc.want)
		}
		out := decodeJSONObject(t, got.stdout)
		if out["req_body_size"] != float64(len(tc.want)) {
			t.Errorf("%s req_body_size = %v, want %d", tc.name, out["req_body_size"], len(tc.want))
		}
		requireRawHeaderPairs(t, observed.headers, [][2]string{
			{"Host", targetAddr},
			{"Content-Length", strconv.Itoa(len(tc.want))},
		})
	}
}

func TestSendPatchRejectsInvalidPatch(t *testing.T) {
	env := newTestEnv(t)
	base := "http://127.0.0.1:1/base"

	cases := map[string]string{
		"malformed json":    "not-json",
		"non-object patch":  `[]`,
		"empty method":      `{"method":""}`,
		"method with space": `{"method":"BAD METHOD"}`,
		"null method":       `{"method":null}`,
		"non-http url":      `{"url":"ftp://example.test/"}`,
		"null url":          `{"url":null}`,
		"headers not array": `{"headers":"nope"}`,
		"short header pair": `{"headers":[["X"]]}`,
		"header injection":  `{"headers":[["X","a\r\nInjected: 1"]]}`,
		"query not array":   `{"query":"nope"}`,
		"short query pair":  `{"query":[["a"]]}`,
		"body not string":   `{"body":5}`,
		"multiple bodies":   `{"body":"x","body_base64":"eA=="}`,
		"invalid base64":    `{"body_base64":"!!!"}`,
		"missing body file": mustJSON(t, map[string]any{
			"body_file": filepath.Join(t.TempDir(), "missing.bin"),
		}),
		"unknown field": `{"unknown":1}`,
	}
	for name, patch := range cases {
		got := env.runWithStore(t, "send", "--url", base, "--patch", patch)
		requireExit(t, got, 2)
		if code := errorEnvelope(t, got)["code"]; code != "INVALID_PATCH" {
			t.Errorf("%s code = %v, want INVALID_PATCH", name, code)
		}
	}
}

func TestSendPatchHostAndQueryRules(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	base := "http://" + targetAddr + "/old?base=1"

	cases := []struct {
		name     string
		patch    string
		wantLine string
		wantHost string
	}{
		{
			name:     "url only follows new query",
			patch:    `{"url":"http://` + targetAddr + `/new?from=url"}`,
			wantLine: "GET /new?from=url HTTP/1.1",
			wantHost: targetAddr,
		},
		{
			name:     "query overrides url",
			patch:    `{"url":"http://` + targetAddr + `/new?from=url","query":[["q","1"],["q","2"]]}`,
			wantLine: "GET /new?q=1&q=2 HTTP/1.1",
			wantHost: targetAddr,
		},
		{
			name:     "null query derives from url",
			patch:    `{"query":null}`,
			wantLine: "GET /old?base=1 HTTP/1.1",
			wantHost: targetAddr,
		},
		{
			name:     "empty query clears",
			patch:    `{"query":[]}`,
			wantLine: "GET /old HTTP/1.1",
			wantHost: targetAddr,
		},
		{
			name:     "empty query wins over url query",
			patch:    `{"url":"http://` + targetAddr + `/new?from=url","query":[]}`,
			wantLine: "GET /new HTTP/1.1",
			wantHost: targetAddr,
		},
		{
			name:     "patch host wins",
			patch:    `{"url":"http://` + targetAddr + `/new","headers":[["Host","custom.example"]]}`,
			wantLine: "GET /new HTTP/1.1",
			wantHost: "custom.example",
		},
	}
	for _, tc := range cases {
		got := env.runWithStore(t, "send", "--url", base, "--patch", tc.patch)
		requireExit(t, got, 0)
		observed := waitObserved(t, requests)
		if observed.line != tc.wantLine {
			t.Errorf("%s request line = %q, want %q", tc.name, observed.line, tc.wantLine)
		}
		if host := headerValue(observed.headers, "Host"); host != tc.wantHost {
			t.Errorf("%s Host = %q, want %q", tc.name, host, tc.wantHost)
		}
	}
}

func TestSendPatchRecalculatesContentLength(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	lfChunked := "POST /lf HTTP/1.1\n" +
		"Host: " + targetAddr + "\n" +
		"Transfer-Encoding: chunked\n" +
		"Connection: close\n" +
		"\n" +
		"5\nhello\n" +
		"6\n world\n" +
		"0\n\n"
	path := filepath.Join(t.TempDir(), "chunked.txt")
	if err := os.WriteFile(path, []byte(lfChunked), 0o600); err != nil {
		t.Fatalf("write raw request: %v", err)
	}

	got := env.runWithStore(t, "send", "--from-file", path, "--patch", `{"body":"replaced"}`)
	requireExit(t, got, 0)
	observed := waitObserved(t, requests)
	if observed.line != "POST /lf HTTP/1.1" {
		t.Errorf("request line = %q", observed.line)
	}
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"Host", targetAddr},
		{"Content-Length", "8"},
		{"Connection", "close"},
	})
	if string(observed.body) != "replaced" {
		t.Errorf("target body = %q, want replaced", observed.body)
	}
	out := decodeJSONObject(t, got.stdout)
	if out["req_body_size"] != float64(8) {
		t.Errorf("req_body_size = %v, want 8", out["req_body_size"])
	}
	stored, err := os.ReadFile(out["request_body_path"].(string))
	if err != nil || string(stored) != "replaced" {
		t.Errorf("stored request body = %q (%v), want replaced", stored, err)
	}
}

func TestSendPatchFromFileRendersDocument(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	crlf := "POST /crlf?x=1 HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"X-Old: value\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello"
	path := filepath.Join(t.TempDir(), "crlf.txt")
	if err := os.WriteFile(path, []byte(crlf), 0o600); err != nil {
		t.Fatalf("write raw request: %v", err)
	}

	patch := `{"method":"PATCH","headers":[["X-New","one"],["X-New","two"]]}`
	got := env.runWithStore(t, "send", "--from-file", path, "--patch", patch)
	requireExit(t, got, 0)
	observed := waitObserved(t, requests)
	if observed.line != "PATCH /crlf?x=1 HTTP/1.1" {
		t.Errorf("request line = %q, want PATCH /crlf?x=1 HTTP/1.1", observed.line)
	}
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"X-New", "one"},
		{"X-New", "two"},
		{"Content-Length", "5"},
		{"Host", targetAddr},
	})
	if string(observed.body) != "hello" {
		t.Errorf("target body = %q, want hello", observed.body)
	}
	out := decodeJSONObject(t, got.stdout)
	if out["method"] != "PATCH" || out["kind"] != "sent" {
		t.Errorf("method/kind = %v/%v, want PATCH/sent", out["method"], out["kind"])
	}
	if out["url"] != "http://"+targetAddr+"/crlf?x=1" {
		t.Errorf("url = %v", out["url"])
	}
}

func TestSendPatchVariables(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	base := "http://" + targetAddr + "/vars"

	patch := `{"headers":[["X-Token","{{TOKEN}}"]],"body":"{{BODY}}"}`
	got := env.runWithStore(t, "send", "--url", base, "--patch", patch,
		"--var", "TOKEN=abc", "--var", "BODY=hello world")
	requireExit(t, got, 0)
	observed := waitObserved(t, requests)
	if host := headerValue(observed.headers, "X-Token"); host != "abc" {
		t.Errorf("X-Token = %q, want abc", host)
	}
	if string(observed.body) != "hello world" {
		t.Errorf("target body = %q, want the variable value", observed.body)
	}

	envToken := env.runWithEnv(t, []string{"MITT_TEST_TOKEN=env-secret"},
		"--store", env.store, "send", "--url", base, "--patch", `{"headers":[["X-Token","{{MITT_TEST_TOKEN}}"]]}`, "--var", "MITT_TEST_TOKEN")
	requireExit(t, envToken, 0)
	observed = waitObserved(t, requests)
	if host := headerValue(observed.headers, "X-Token"); host != "env-secret" {
		t.Errorf("env X-Token = %q, want env-secret", host)
	}

	missingEnv := env.runWithStore(t, "send", "--url", base, "--patch", `{"headers":[["X-Token","{{TOKEN}}"]]}`, "--var", "TOKEN")
	requireExit(t, missingEnv, 2)
	if code := errorEnvelope(t, missingEnv)["code"]; code != "INVALID_PATCH" {
		t.Errorf("missing env var code = %v, want INVALID_PATCH", code)
	}

	unresolved := env.runWithStore(t, "send", "--url", base, "--patch", `{"body":"{{MISSING}}"}`)
	requireExit(t, unresolved, 2)
	if code := errorEnvelope(t, unresolved)["code"]; code != "INVALID_PATCH" {
		t.Errorf("unresolved variable code = %v, want INVALID_PATCH", code)
	}

	withoutPatch := env.runWithStore(t, "send", "--url", base, "--var", "TOKEN=abc")
	requireExit(t, withoutPatch, 2)
	if code := errorEnvelope(t, withoutPatch)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("--var without --patch code = %v, want INVALID_REQUEST", code)
	}
}

func TestSendPatchInputSources(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	base := "http://" + targetAddr + "/source"

	patchPath := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(patchPath, []byte(`{"method":"PUT"}`), 0o600); err != nil {
		t.Fatalf("write patch file: %v", err)
	}
	fromFile := env.runWithStore(t, "send", "--url", base, "--patch", "@"+patchPath)
	requireExit(t, fromFile, 0)
	if observed := waitObserved(t, requests); observed.line != "PUT /source HTTP/1.1" {
		t.Errorf("@path request line = %q", observed.line)
	}

	fromStdin := env.runWithStdin(t, `{"method":"DELETE"}`, "send", "--url", base, "--patch", "-")
	requireExit(t, fromStdin, 0)
	if observed := waitObserved(t, requests); observed.line != "DELETE /source HTTP/1.1" {
		t.Errorf("stdin request line = %q", observed.line)
	}

	missing := env.runWithStore(t, "send", "--url", base, "--patch", "@"+filepath.Join(t.TempDir(), "missing.json"))
	requireExit(t, missing, 2)
	if code := errorEnvelope(t, missing)["code"]; code != "INVALID_PATCH" {
		t.Errorf("missing @path code = %v, want INVALID_PATCH", code)
	}
}

func TestSendPatchPersistsMutationsAndLeavesBaseUntouched(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	rawRequest := "POST http://{{target}}/base?q=1 HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"X-Trace: one\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello"
	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)
	baseBefore := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, baseBefore, 0)
	baseBodyPath := filepath.Join(env.store, "bodies", id+"-req.bin")
	baseBodyBefore, err := os.ReadFile(baseBodyPath)
	if err != nil {
		t.Fatalf("read base body: %v", err)
	}

	liveAddr, requests := startRawTarget(t, targetResponse)
	rewriteHistoryURL(t, env.store, id, "http://"+liveAddr+"/base?q=1")

	patch := `{"method":"PUT","headers":[["X-Trace","patched"]],"body_file":null,"body":"new body"}`
	got := env.runWithStore(t, "send", id, "--patch", patch)
	requireExit(t, got, 0)

	observed := waitObserved(t, requests)
	if observed.line != "PUT /base?q=1 HTTP/1.1" {
		t.Errorf("request line = %q", observed.line)
	}
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"X-Trace", "patched"},
		{"Content-Length", "8"},
		{"Host", liveAddr},
	})
	if string(observed.body) != "new body" {
		t.Errorf("target body = %q, want new body", observed.body)
	}

	out := decodeJSONObject(t, got.stdout)
	if out["kind"] != "replayed" || out["parent_id"] != id {
		t.Errorf("kind/parent_id = %v/%v, want replayed/%s", out["kind"], out["parent_id"], id)
	}
	newID, _ := out["id"].(string)
	if newID == "" || newID == id {
		t.Fatalf("new id = %q, want a fresh id", newID)
	}
	stored, err := os.ReadFile(out["request_body_path"].(string))
	if err != nil || string(stored) != "new body" {
		t.Errorf("stored request body = %q (%v), want new body", stored, err)
	}

	historyPath := filepath.Join(env.store, "history.jsonl")
	historyData, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	lines := historyLines(string(historyData))
	if len(lines) != 2 {
		t.Fatalf("history has %d lines, want 2", len(lines))
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &event); err != nil {
		t.Fatalf("decode replayed event: %v", err)
	}
	if event["mutations"] != patch {
		t.Errorf("mutations = %v, want the raw patch", event["mutations"])
	}
	if event["parent_id"] != id || event["kind"] != "replayed" {
		t.Errorf("event = %v, want a replayed child of %s", event, id)
	}

	baseAfter := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, baseAfter, 0)
	if baseAfter.stdout != baseBefore.stdout {
		t.Errorf("base raw bytes changed after a patched send")
	}
	baseBodyAfter, err := os.ReadFile(baseBodyPath)
	if err != nil {
		t.Fatalf("read base body after send: %v", err)
	}
	if string(baseBodyAfter) != string(baseBodyBefore) {
		t.Errorf("base request body changed: before %q after %q", baseBodyBefore, baseBodyAfter)
	}
}

func TestSendPatchRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	send, ok := doc.Command.find("mitt send")
	if !ok {
		t.Fatal("spec missing mitt send")
	}
	patchFlag, ok := flagByName(send.Flags, "patch")
	if !ok || patchFlag.Type != "string" {
		t.Errorf("send --patch flag = %+v, want a string flag", patchFlag)
	}
	varFlag, ok := flagByName(send.Flags, "var")
	if !ok || varFlag.Type != "stringArray" {
		t.Errorf("send --var flag = %+v, want a repeatable string flag", varFlag)
	}
	for _, code := range []string{"INVALID_PATCH", "INVALID_REQUEST", "BASE_CONFLICT"} {
		if !containsString(send.ErrorCodes, code) {
			t.Errorf("send error_codes = %v, want %s", send.ErrorCodes, code)
		}
	}
	if !containsString(doc.CommonErrorCodes, "INVALID_REQUEST") {
		t.Errorf("common_error_codes = %v, want INVALID_REQUEST", doc.CommonErrorCodes)
	}
}

func headerValue(headers [][2]string, name string) string {
	for _, header := range headers {
		if strings.EqualFold(header[0], name) {
			return header[1]
		}
	}
	return ""
}
