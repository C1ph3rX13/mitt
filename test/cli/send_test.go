package cli_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSendURLConstructsAndSendsGET(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"X-Target: raw\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello"
	targetAddr, requests := startRawTarget(t, targetResponse)

	got := env.runWithStore(t, "send", "--url", "http://"+targetAddr+"/api/items?q=1")
	requireExit(t, got, 0)

	observed := waitObserved(t, requests)
	if observed.line != "GET /api/items?q=1 HTTP/1.1" {
		t.Errorf("target request line = %q, want GET origin-form", observed.line)
	}
	requireRawHeaderPairs(t, observed.headers, [][2]string{{"Host", targetAddr}})
	if len(observed.body) != 0 {
		t.Errorf("target body = %q, want none", observed.body)
	}

	out := decodeJSONObject(t, got.stdout)
	if out["kind"] != "sent" {
		t.Errorf("kind = %v, want sent", out["kind"])
	}
	if _, ok := out["parent_id"]; ok {
		t.Errorf("--url entry must not have parent_id: %v", out)
	}
	if out["method"] != "GET" {
		t.Errorf("method = %v, want GET", out["method"])
	}
	if out["url"] != "http://"+targetAddr+"/api/items?q=1" {
		t.Errorf("url = %v, want the supplied URL", out["url"])
	}
	if out["status"] != float64(200) {
		t.Errorf("status = %v, want 200", out["status"])
	}
	if out["resp_body_size"] != float64(len("hello")) {
		t.Errorf("resp_body_size = %v, want %d", out["resp_body_size"], len("hello"))
	}
	for _, key := range []string{"id", "request_line", "request_headers", "request_body_path", "response_line", "response_headers", "response_body_path", "timings"} {
		if _, ok := out[key]; !ok {
			t.Errorf("send output missing %q: %v", key, out)
		}
	}
	if _, ok := out["timings"].(map[string]any); !ok {
		t.Errorf("timings = %T, want an object", out["timings"])
	}

	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("send output has no id")
	}
	history := env.runWithStore(t, "history")
	requireExit(t, history, 0)
	lines := historyLines(history.stdout)
	if len(lines) != 1 {
		t.Fatalf("history returned %d lines, want 1:\n%s", len(lines), history.stdout)
	}
	summary := decodeJSONObject(t, lines[0])
	if summary["id"] != id || summary["kind"] != "sent" || summary["status"] != float64(200) {
		t.Errorf("history summary = %v, want sent 200 entry %s", summary, id)
	}

	responseBody, err := os.ReadFile(filepath.Join(env.store, "bodies", id+"-resp.bin"))
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(responseBody) != "hello" {
		t.Errorf("stored response body = %q, want hello", responseBody)
	}
}

func TestSendRequiresExactlyOneBase(t *testing.T) {
	env := newTestEnv(t)
	cases := [][]string{
		{"send"},
		{"send", "--url", "http://example.test/", "--from-file", "request.txt"},
		{"send", "entry-id", "--url", "http://example.test/"},
		{"send", "entry-id", "--from-file", "request.txt"},
	}
	for _, args := range cases {
		got := env.runWithStore(t, args...)
		requireExit(t, got, 2)
		errObj := errorEnvelope(t, got)
		if errObj["code"] != "BASE_CONFLICT" {
			t.Errorf("%v code = %v, want BASE_CONFLICT", args, errObj["code"])
		}
	}
}

func TestSendRejectsInvalidURLs(t *testing.T) {
	env := newTestEnv(t)
	for _, rawURL := range []string{"ftp://example.test/", "/relative", "example.test/path", "http://", "https:///path"} {
		got := env.runWithStore(t, "send", "--url", rawURL)
		requireExit(t, got, 2)
		errObj := errorEnvelope(t, got)
		if errObj["code"] != "INVALID_REQUEST" {
			t.Errorf("--url %q code = %v, want INVALID_REQUEST", rawURL, errObj["code"])
		}
	}
}

func TestSendMissingEntryAndFile(t *testing.T) {
	env := newTestEnv(t)

	missing := env.runWithStore(t, "send", "does-not-exist")
	requireExit(t, missing, 66)
	if code := errorEnvelope(t, missing)["code"]; code != "ENTRY_NOT_FOUND" {
		t.Errorf("missing entry code = %v, want ENTRY_NOT_FOUND", code)
	}

	missingFile := env.runWithStore(t, "send", "--from-file", filepath.Join(t.TempDir(), "missing.txt"))
	requireExit(t, missingFile, 2)
	if code := errorEnvelope(t, missingFile)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("missing file code = %v, want INVALID_REQUEST", code)
	}

	malformed := filepath.Join(t.TempDir(), "malformed.txt")
	if err := os.WriteFile(malformed, []byte("not an HTTP request\n"), 0o600); err != nil {
		t.Fatalf("write malformed request: %v", err)
	}
	malformedResult := env.runWithStore(t, "send", "--from-file", malformed)
	requireExit(t, malformedResult, 2)
	if code := errorEnvelope(t, malformedResult)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("malformed file code = %v, want INVALID_REQUEST", code)
	}
}

func TestSendFromFileSendsBytesVerbatim(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 204 No Content\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	targetAddr, requests := startRawTarget(t, targetResponse)

	request := "POST /raw?x=1 HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"X-Order: first\r\n" +
		"X-Other: value\r\n" +
		"X-Order: second\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello"
	path := filepath.Join(t.TempDir(), "request.txt")
	if err := os.WriteFile(path, []byte(request), 0o600); err != nil {
		t.Fatalf("write raw request: %v", err)
	}

	got := env.runWithStore(t, "send", "--from-file", path)
	requireExit(t, got, 0)

	observed := waitObserved(t, requests)
	if observed.line != "POST /raw?x=1 HTTP/1.1" {
		t.Errorf("target request line = %q, want verbatim origin-form", observed.line)
	}
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"Host", targetAddr},
		{"X-Order", "first"},
		{"X-Other", "value"},
		{"X-Order", "second"},
		{"Content-Length", "5"},
		{"Connection", "close"},
	})
	if string(observed.body) != "hello" {
		t.Errorf("target body = %q, want hello", observed.body)
	}

	out := decodeJSONObject(t, got.stdout)
	if out["kind"] != "sent" {
		t.Errorf("kind = %v, want sent", out["kind"])
	}
	if _, ok := out["parent_id"]; ok {
		t.Errorf("--from-file entry must not have parent_id: %v", out)
	}
	if out["method"] != "POST" || out["url"] != "http://"+targetAddr+"/raw?x=1" {
		t.Errorf("method/url = %v/%v, want POST and derived URL", out["method"], out["url"])
	}
	if out["req_body_size"] != float64(5) || out["status"] != float64(204) {
		t.Errorf("req_body_size/status = %v/%v, want 5/204", out["req_body_size"], out["status"])
	}
	requireHeaderPairs(t, out["request_headers"], [][2]string{
		{"Host", targetAddr},
		{"X-Order", "first"},
		{"X-Other", "value"},
		{"X-Order", "second"},
		{"Content-Length", "5"},
		{"Connection", "close"},
	})
}

func TestSendEntryCreatesReplayedEntryAndLeavesBaseUntouched(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"ok"
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
	baseRequestPath := filepath.Join(env.store, "bodies", id+"-req.bin")
	baseBodyBefore, err := os.ReadFile(baseRequestPath)
	if err != nil {
		t.Fatalf("read base request body: %v", err)
	}

	// The capture proxy consumed the one response from the single-request
	// target, so start a fresh target for the replay.
	replayResponse := "HTTP/1.1 201 Created\r\n" +
		"X-Replayed: yes\r\n" +
		"Content-Length: 7\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"created"

	// Replaying the captured entry points at the original target address, so
	// replace the history URL with a live target for this test.
	liveAddr, requests := startRawTarget(t, replayResponse)
	rewriteHistoryURL(t, env.store, id, "http://"+liveAddr+"/base?q=1")

	got := env.runWithStore(t, "send", id)
	requireExit(t, got, 0)

	observed := waitObserved(t, requests)
	if observed.line != "POST /base?q=1 HTTP/1.1" {
		t.Errorf("target request line = %q, want POST origin-form", observed.line)
	}
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"Host", liveAddr},
		{"X-Trace", "one"},
		{"Content-Length", "5"},
		{"Connection", "close"},
	})
	if string(observed.body) != "hello" {
		t.Errorf("target body = %q, want hello", observed.body)
	}

	out := decodeJSONObject(t, got.stdout)
	if out["kind"] != "replayed" {
		t.Errorf("kind = %v, want replayed", out["kind"])
	}
	if out["parent_id"] != id {
		t.Errorf("parent_id = %v, want %s", out["parent_id"], id)
	}
	if out["status"] != float64(201) {
		t.Errorf("status = %v, want 201", out["status"])
	}
	newID, _ := out["id"].(string)
	if newID == "" || newID == id {
		t.Fatalf("new id = %q, want a fresh id", newID)
	}
	if out["request_body_path"] != filepath.Join(env.store, "bodies", newID+"-req.bin") {
		t.Errorf("request_body_path = %v", out["request_body_path"])
	}
	if out["response_body_path"] != filepath.Join(env.store, "bodies", newID+"-resp.bin") {
		t.Errorf("response_body_path = %v", out["response_body_path"])
	}
	replayedBody, err := os.ReadFile(filepath.Join(env.store, "bodies", newID+"-req.bin"))
	if err != nil {
		t.Fatalf("read replayed request body: %v", err)
	}
	if string(replayedBody) != "hello" {
		t.Errorf("replayed request body = %q, want hello", replayedBody)
	}

	baseAfter := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, baseAfter, 0)
	if baseAfter.stdout != baseBefore.stdout {
		t.Errorf("base raw bytes changed after send:\nbefore %q\nafter  %q", baseBefore.stdout, baseAfter.stdout)
	}
	baseBodyAfter, err := os.ReadFile(baseRequestPath)
	if err != nil {
		t.Fatalf("read base request body after send: %v", err)
	}
	if string(baseBodyAfter) != string(baseBodyBefore) {
		t.Errorf("base request body changed: before %q after %q", baseBodyBefore, baseBodyAfter)
	}

	history := env.runWithStore(t, "history")
	requireExit(t, history, 0)
	lines := historyLines(history.stdout)
	if len(lines) != 2 {
		t.Fatalf("history returned %d lines, want 2:\n%s", len(lines), history.stdout)
	}
	replayed := decodeJSONObject(t, lines[1])
	if replayed["id"] != newID || replayed["kind"] != "replayed" {
		t.Errorf("history replayed summary = %v", replayed)
	}
}

func TestSendNormalizesChunkedAndContentLength(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	rawRequest := "POST http://{{target}}/chunked HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n" +
		"6\r\n world\r\n" +
		"0\r\n\r\n"
	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)

	liveAddr, requests := startRawTarget(t, targetResponse)
	rewriteHistoryURL(t, env.store, id, "http://"+liveAddr+"/chunked")

	got := env.runWithStore(t, "send", id)
	requireExit(t, got, 0)

	observed := waitObserved(t, requests)
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"Host", liveAddr},
		{"Content-Length", "11"},
		{"Connection", "close"},
	})
	if string(observed.body) != "hello world" {
		t.Errorf("target body = %q, want the decoded entity", observed.body)
	}
}

func TestSendUpstreamStatusExitCodes(t *testing.T) {
	env := newTestEnv(t)

	notFoundAddr, notFoundRequests := startRawTarget(t, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	notFound := env.runWithStore(t, "send", "--url", "http://"+notFoundAddr+"/missing")
	requireExit(t, notFound, 0)
	waitObserved(t, notFoundRequests)
	if out := decodeJSONObject(t, notFound.stdout); out["status"] != float64(404) {
		t.Errorf("404 send status = %v", out["status"])
	}

	tempAddr, tempRequests := startRawTarget(t, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 4\r\nConnection: close\r\n\r\ndown")
	failed := env.runWithStore(t, "send", "--url", "http://"+tempAddr+"/down")
	requireExit(t, failed, 75)
	if strings.TrimSpace(failed.stdout) == "" {
		t.Fatal("upstream 5xx must still write the result object to stdout")
	}
	out := decodeJSONObject(t, failed.stdout)
	if out["status"] != float64(503) || out["kind"] != "sent" {
		t.Errorf("5xx result = %v, want a sent 503 entry", out)
	}
	waitObserved(t, tempRequests)
	errObj := errorEnvelopeWithStdoutData(t, failed)
	if errObj["code"] != "TEMP_FAILURE" {
		t.Errorf("5xx code = %v, want TEMP_FAILURE", errObj["code"])
	}
	if errObj["retryable"] != true {
		t.Errorf("5xx retryable = %v, want true", errObj["retryable"])
	}

	history := env.runWithStore(t, "history")
	requireExit(t, history, 0)
	if len(historyLines(history.stdout)) != 2 {
		t.Errorf("history must retain both the 404 and 503 sends:\n%s", history.stdout)
	}
}

func TestSendConnectionErrors(t *testing.T) {
	env := newTestEnv(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := listener.Addr().String()
	_ = listener.Close()

	refused := env.runWithStore(t, "send", "--url", "http://"+closedAddr+"/")
	requireExit(t, refused, 75)
	if code := errorEnvelope(t, refused)["code"]; code != "TEMP_FAILURE" {
		t.Errorf("connection failure code = %v, want TEMP_FAILURE", code)
	}

	dnsFailure := env.runWithStore(t, "send", "--url", "http://mitt-send-does-not-exist.invalid/")
	requireExit(t, dnsFailure, 75)
	if code := errorEnvelope(t, dnsFailure)["code"]; code != "TEMP_FAILURE" {
		t.Errorf("DNS failure code = %v, want TEMP_FAILURE", code)
	}
}

func TestSendBypassesProxyEnvironment(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	got := env.runWithEnv(t, []string{"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1"},
		"--store", env.store, "send", "--url", "http://"+targetAddr+"/direct")
	requireExit(t, got, 0)
	observed := waitObserved(t, requests)
	if observed.line != "GET /direct HTTP/1.1" {
		t.Errorf("target request line = %q", observed.line)
	}
}

func TestSendRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	send, ok := doc.Command.find("mitt send")
	if !ok {
		t.Fatal("spec missing mitt send")
	}
	if len(send.Args) != 1 || send.Args[0].Name != "id" || send.Args[0].Required {
		t.Errorf("send positionals = %+v, want optional id", send.Args)
	}
	urlFlag, ok := flagByName(send.Flags, "url")
	if !ok || urlFlag.Type != "string" {
		t.Errorf("send --url flag = %+v, want a string flag", urlFlag)
	}
	fileFlag, ok := flagByName(send.Flags, "from-file")
	if !ok || fileFlag.Type != "string" {
		t.Errorf("send --from-file flag = %+v, want a string flag", fileFlag)
	}
	for _, code := range []string{"BASE_CONFLICT", "ENTRY_NOT_FOUND", "INVALID_REQUEST", "TEMP_FAILURE", "UPSTREAM_TLS"} {
		if !containsString(send.ErrorCodes, code) {
			t.Errorf("send error_codes = %v, want %s", send.ErrorCodes, code)
		}
	}
	if send.OutputSchema == nil {
		t.Fatal("send must register a success output schema")
	}
	if send.OutputSchema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("send output schema $schema = %v", send.OutputSchema["$schema"])
	}
}

// rewriteHistoryURL replaces the URL of one history entry. Tests use it to
// point a captured entry at a fresh raw target after the original address is
// no longer serving.
func rewriteHistoryURL(t *testing.T, store, id, rawURL string) {
	t.Helper()
	path := filepath.Join(store, "history.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode history line %d: %v", i+1, err)
		}
		if event["id"] == id {
			event["url"] = rawURL
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("encode history line %d: %v", i+1, err)
			}
			lines[i] = string(encoded)
			found = true
		}
	}
	if !found {
		t.Fatalf("history entry %s not found", id)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write history: %v", err)
	}
}

func requireRawHeaderPairs(t *testing.T, got, want [][2]string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("headers = %#v, want %#v", got, want)
	}
}

func errorEnvelopeWithStdoutData(t *testing.T, got result) map[string]any {
	t.Helper()
	if strings.TrimSpace(got.stderr) == "" {
		t.Fatal("stderr must contain the error object")
	}
	lines := historyLines(got.stderr)
	envelope := decodeJSONObject(t, lines[len(lines)-1])
	errObj, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("last stderr line is not an error envelope: %s", lines[len(lines)-1])
	}
	return errObj
}

func TestSendTLSAdvertisesOnlyHTTP11(t *testing.T) {
	env := newTestEnv(t)
	ca := newTestUpstreamCA(t)
	cert, _ := ca.issueTargetCert(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	target := startTLSTarget(t, cert, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	caPath := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caPath, ca.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	got := env.runWithEnv(t, []string{"SSL_CERT_FILE=" + caPath},
		"--store", env.store, "send", "--url", "https://"+target.addr+"/tls?q=1")
	requireExit(t, got, 0)

	observed := target.waitRequest(t)
	if observed.line != "GET /tls?q=1 HTTP/1.1" {
		t.Errorf("target request line = %q, want GET origin-form", observed.line)
	}
	if alpn := target.waitALPN(t); alpn != "http/1.1" {
		t.Errorf("ALPN = %q, want http/1.1 only", alpn)
	}
	if out := decodeJSONObject(t, got.stdout); out["status"] != float64(200) {
		t.Errorf("status = %v, want 200", out["status"])
	}
}

func TestSendUpstreamTLSFailureIsNotRetryable(t *testing.T) {
	env := newTestEnv(t)
	trusted := newTestUpstreamCA(t)
	targetCA := newTestUpstreamCA(t)
	cert, _ := targetCA.issueTargetCert(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	target := startTLSTarget(t, cert, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	unrelatedCA := filepath.Join(t.TempDir(), "unrelated-ca.pem")
	if err := os.WriteFile(unrelatedCA, trusted.pem, 0o600); err != nil {
		t.Fatalf("write unrelated CA: %v", err)
	}
	got := env.runWithEnv(t, []string{"SSL_CERT_FILE=" + unrelatedCA},
		"--store", env.store, "send", "--url", "https://"+target.addr+"/tls")
	requireExit(t, got, 1)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "UPSTREAM_TLS" {
		t.Errorf("code = %v, want UPSTREAM_TLS", errObj["code"])
	}
	if errObj["retryable"] != false {
		t.Errorf("retryable = %v, want false", errObj["retryable"])
	}

	history := env.runWithStore(t, "history")
	requireExit(t, history, 66)
	if code := errorEnvelope(t, history)["code"]; code != "NO_MATCH" {
		t.Errorf("history code = %v, want NO_MATCH after a failed handshake", code)
	}
}

func TestSendFromFileAcceptsLFAndChunked(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	request := "POST /lf HTTP/1.1\n" +
		"Host: " + targetAddr + "\n" +
		"Transfer-Encoding: chunked\n" +
		"Connection: close\n" +
		"\n" +
		"5\nhello\n" +
		"6\n world\n" +
		"0\n\n"
	path := filepath.Join(t.TempDir(), "lf-chunked.txt")
	if err := os.WriteFile(path, []byte(request), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}

	got := env.runWithStore(t, "send", "--from-file", path)
	requireExit(t, got, 0)

	observed := waitObserved(t, requests)
	if observed.line != "POST /lf HTTP/1.1" {
		t.Errorf("request line = %q", observed.line)
	}
	if string(observed.body) != "hello world" {
		t.Errorf("target body = %q, want decoded entity", observed.body)
	}
	out := decodeJSONObject(t, got.stdout)
	if out["req_body_size"] != float64(len("hello world")) {
		t.Errorf("req_body_size = %v, want decoded size", out["req_body_size"])
	}
	stored, err := os.ReadFile(out["request_body_path"].(string))
	if err != nil {
		t.Fatalf("read stored request body: %v", err)
	}
	if string(stored) != "hello world" {
		t.Errorf("stored request body = %q, want decoded entity", stored)
	}
}

func TestSendPersistsChunkedAndCloseDelimitedResponses(t *testing.T) {
	env := newTestEnv(t)

	chunkedAddr, chunkedRequests := startRawTarget(t, "HTTP/1.1 200 OK\r\n"+
		"Transfer-Encoding: chunked\r\n"+
		"Connection: close\r\n"+
		"\r\n"+
		"3\r\nabc\r\n"+
		"2\r\nde\r\n"+
		"0\r\n\r\n")
	chunked := env.runWithStore(t, "send", "--url", "http://"+chunkedAddr+"/chunked")
	requireExit(t, chunked, 0)
	waitObserved(t, chunkedRequests)
	chunkedOut := decodeJSONObject(t, chunked.stdout)
	if chunkedOut["resp_body_size"] != float64(5) {
		t.Errorf("chunked resp_body_size = %v, want 5", chunkedOut["resp_body_size"])
	}
	body, err := os.ReadFile(chunkedOut["response_body_path"].(string))
	if err != nil {
		t.Fatalf("read chunked response body: %v", err)
	}
	if string(body) != "abcde" {
		t.Errorf("chunked response body = %q, want abcde", body)
	}
	requireHeaderPairs(t, chunkedOut["response_headers"], [][2]string{
		{"Transfer-Encoding", "chunked"},
		{"Connection", "close"},
	})

	closeAddr, closeRequests := startRawTarget(t, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nclose-body")
	closeResult := env.runWithStore(t, "send", "--url", "http://"+closeAddr+"/close")
	requireExit(t, closeResult, 0)
	waitObserved(t, closeRequests)
	closeOut := decodeJSONObject(t, closeResult.stdout)
	if closeOut["resp_body_size"] != float64(len("close-body")) {
		t.Errorf("close-delimited resp_body_size = %v, want %d", closeOut["resp_body_size"], len("close-body"))
	}
	body, err = os.ReadFile(closeOut["response_body_path"].(string))
	if err != nil {
		t.Fatalf("read close-delimited response body: %v", err)
	}
	if string(body) != "close-body" {
		t.Errorf("close-delimited response body = %q, want close-body", body)
	}
}

func TestSendFromFileAcceptsAbsoluteRequestTarget(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	targetURL := "http://" + targetAddr + "/absolute?x=1"
	request := "PUT " + targetURL + " HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Content-Length: 0\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	path := filepath.Join(t.TempDir(), "absolute.txt")
	if err := os.WriteFile(path, []byte(request), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}

	got := env.runWithStore(t, "send", "--from-file", path)
	requireExit(t, got, 0)
	observed := waitObserved(t, requests)
	if observed.line != "PUT "+targetURL+" HTTP/1.1" {
		t.Errorf("request line = %q, want verbatim absolute-form", observed.line)
	}
	out := decodeJSONObject(t, got.stdout)
	if out["url"] != targetURL {
		t.Errorf("url = %v, want %s", out["url"], targetURL)
	}
	if out["status"] != float64(200) {
		t.Errorf("status = %v, want 200", out["status"])
	}
}

func TestSendFromFileAbsoluteHTTPSUsesTLS(t *testing.T) {
	env := newTestEnv(t)
	ca := newTestUpstreamCA(t)
	cert, _ := ca.issueTargetCert(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	target := startTLSTarget(t, cert, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	targetURL := "https://" + target.addr + "/from-file"
	request := "GET " + targetURL + " HTTP/1.1\r\nHost: " + target.addr + "\r\nConnection: close\r\n\r\n"
	path := filepath.Join(t.TempDir(), "https.txt")
	if err := os.WriteFile(path, []byte(request), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}
	caPath := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caPath, ca.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}

	got := env.runWithEnv(t, []string{"SSL_CERT_FILE=" + caPath},
		"--store", env.store, "send", "--from-file", path)
	requireExit(t, got, 0)
	if observed := target.waitRequest(t); observed.line != "GET "+targetURL+" HTTP/1.1" {
		t.Errorf("request line = %q, want verbatim absolute-form", observed.line)
	}
	if alpn := target.waitALPN(t); alpn != "http/1.1" {
		t.Errorf("ALPN = %q, want http/1.1", alpn)
	}
	out := decodeJSONObject(t, got.stdout)
	if out["url"] != targetURL {
		t.Errorf("url = %v, want %s", out["url"], targetURL)
	}
}
