package cli_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type observedRequest struct {
	line    string
	headers [][2]string
	body    []byte
}

// startRawTarget runs a one-shot-per-connection HTTP/1.1 target and publishes
// every request it sees. A raw target keeps header order observable, which a
// net/http server would lose.
func startRawTarget(t *testing.T, response string) (string, <-chan observedRequest) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	requests := make(chan observedRequest, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				request, err := readObservedRequest(conn)
				if err != nil {
					return
				}
				requests <- request
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.WriteString(conn, response)
			}(conn)
		}
	}()
	return listener.Addr().String(), requests
}

func readObservedRequest(conn net.Conn) (observedRequest, error) {
	return readObservedRequestFrom(bufio.NewReader(conn))
}

func readObservedRequestFrom(reader *bufio.Reader) (observedRequest, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return observedRequest{}, err
	}
	request := observedRequest{line: strings.TrimRight(line, "\r\n")}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return observedRequest{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return observedRequest{}, fmt.Errorf("malformed target header %q", line)
		}
		request.headers = append(request.headers, [2]string{strings.TrimSpace(name), strings.TrimSpace(value)})
	}
	length := int64(0)
	chunked := false
	for _, header := range request.headers {
		switch {
		case strings.EqualFold(header[0], "Content-Length"):
			parsed, err := strconv.ParseInt(header[1], 10, 64)
			if err != nil {
				return observedRequest{}, err
			}
			length = parsed
		case strings.EqualFold(header[0], "Transfer-Encoding") && strings.Contains(strings.ToLower(header[1]), "chunked"):
			chunked = true
		}
	}
	if chunked {
		body, err := readObservedChunked(reader)
		if err != nil {
			return observedRequest{}, err
		}
		request.body = body
	} else if length > 0 {
		request.body = make([]byte, length)
		if _, err := io.ReadFull(reader, request.body); err != nil {
			return observedRequest{}, err
		}
	}
	return request, nil
}

func readObservedChunked(reader *bufio.Reader) ([]byte, error) {
	body := []byte{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeText := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			for {
				trailer, err := reader.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(trailer, "\r\n") == "" {
					return body, nil
				}
			}
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return nil, err
		}
		body = append(body, chunk...)
		if _, err := reader.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

func waitObserved(t *testing.T, requests <-chan observedRequest) observedRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("target did not receive a request")
		return observedRequest{}
	}
}

func sendRaw(t *testing.T, addr, request string) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	response, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(response)
}

func (e *testEnv) startCaptureProxy(t *testing.T, args ...string) (string, *runningProxy) {
	t.Helper()
	e.mustInitCA(t)
	full := append([]string{"--addr", "127.0.0.1:0"}, args...)
	proxy := e.startProxy(t, full...)
	status := proxy.waitReady(t, e)
	addr, _ := status["addr"].(string)
	if addr == "" {
		t.Fatalf("proxy status has no addr: %v", status)
	}
	return addr, proxy
}

func historyLines(stdout string) []string {
	lines := []string{}
	for _, line := range strings.Split(stdout, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func requireHeaderPairs(t *testing.T, got any, want [][2]string) {
	t.Helper()
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal expected headers: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode expected headers: %v", err)
	}
	if !reflect.DeepEqual(got, decoded) {
		t.Fatalf("headers = %#v, want %#v", got, decoded)
	}
}

func TestHTTPCaptureForwardsAndPersists(t *testing.T) {
	env := newTestEnv(t)

	targetResponse := "HTTP/1.1 201 Created\r\n" +
		"X-Server: raw-target\r\n" +
		"X-Repeat: first\r\n" +
		"X-Repeat: second\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"world"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	requestLine := "GET http://" + targetAddr + "/api/items?q=1 HTTP/1.1"
	rawRequest := requestLine + "\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"X-Beta: two\r\n" +
		"X-Alpha: one\r\n" +
		"X-Beta: three\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Length: 11\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello world"

	response := sendRaw(t, proxyAddr, rawRequest)
	if response != targetResponse {
		t.Fatalf("client response = %q, want %q", response, targetResponse)
	}

	observed := waitObserved(t, requests)
	originLine := "GET /api/items?q=1 HTTP/1.1"
	if observed.line != originLine {
		t.Errorf("target request line = %q, want origin-form %q", observed.line, originLine)
	}
	wantHeaders := [][2]string{
		{"Host", targetAddr},
		{"X-Beta", "two"},
		{"X-Alpha", "one"},
		{"X-Beta", "three"},
		{"Content-Type", "text/plain"},
		{"Content-Length", "11"},
		{"Connection", "close"},
	}
	if !reflect.DeepEqual(observed.headers, wantHeaders) {
		t.Errorf("target headers = %#v, want %#v", observed.headers, wantHeaders)
	}
	if string(observed.body) != "hello world" {
		t.Errorf("target body = %q, want %q", observed.body, "hello world")
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
	if proxy.stdout.Len() != 0 {
		t.Errorf("proxy start must stay silent on stdout without --events, got:\n%s", proxy.stdout.String())
	}

	got := env.runWithStore(t, "history")
	requireExit(t, got, 0)
	lines := historyLines(got.stdout)
	if len(lines) != 1 {
		t.Fatalf("history returned %d lines, want 1:\n%s\nstderr:\n%s", len(lines), got.stdout, got.stderr)
	}
	summary := decodeJSONObject(t, lines[0])
	for _, key := range []string{
		"id", "kind", "ts", "method", "url", "status", "context", "req_body_size", "resp_body_size",
	} {
		if _, ok := summary[key]; !ok {
			t.Errorf("history summary missing %q: %v", key, summary)
		}
	}
	if summary["kind"] != "captured" {
		t.Errorf("history kind = %v, want captured", summary["kind"])
	}
	if summary["method"] != "GET" {
		t.Errorf("history method = %v, want GET", summary["method"])
	}
	if summary["url"] != requestLine[len("GET "):len(requestLine)-len(" HTTP/1.1")] {
		t.Errorf("history url = %v, want the absolute request target", summary["url"])
	}
	if summary["status"] != float64(201) {
		t.Errorf("history status = %v, want 201", summary["status"])
	}
	if summary["context"] != "" {
		t.Errorf("history context = %v, want an empty context", summary["context"])
	}
	if summary["req_body_size"] != float64(len("hello world")) {
		t.Errorf("history req_body_size = %v, want %d", summary["req_body_size"], len("hello world"))
	}
	if summary["resp_body_size"] != float64(len("world")) {
		t.Errorf("history resp_body_size = %v, want %d", summary["resp_body_size"], len("world"))
	}
	id, _ := summary["id"].(string)
	if !uuidV7Pattern.MatchString(id) {
		t.Errorf("history id = %q, want a UUIDv7", id)
	}
	ts, _ := summary["ts"].(string)
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("history ts = %q, want RFC3339 nano: %v", ts, err)
	}

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	event := decodeJSONObject(t, string(rawHistory))
	if event["http_version"] != "http/1.1" {
		t.Errorf("event http_version = %v, want http/1.1", event["http_version"])
	}
	if event["client_addr"] == "" {
		t.Error("event client_addr is empty")
	}
	timings, _ := event["timings"].(map[string]any)
	if timings == nil {
		t.Fatalf("event timings missing: %v", event["timings"])
	}
	if _, ok := timings["total_ms"].(float64); !ok {
		t.Errorf("event timings.total_ms = %v, want a number", timings["total_ms"])
	}
	if event["request_line"] != requestLine {
		t.Errorf("event request_line = %q, want the received absolute-form %q", event["request_line"], requestLine)
	}
	if event["forwarded_request_line"] != originLine {
		t.Errorf("event forwarded_request_line = %q, want the forwarded origin-form %q", event["forwarded_request_line"], originLine)
	}
	if event["response_line"] != "HTTP/1.1 201 Created" {
		t.Errorf("event response_line = %v, want the raw status line", event["response_line"])
	}
	requireHeaderPairs(t, event["request_headers"], wantHeaders)
	requireHeaderPairs(t, event["response_headers"], [][2]string{
		{"X-Server", "raw-target"},
		{"X-Repeat", "first"},
		{"X-Repeat", "second"},
		{"Content-Length", "5"},
		{"Connection", "close"},
	})
	if event["req_body_id"] != id || event["resp_body_id"] != id {
		t.Errorf("body ids = (%v, %v), want both %s", event["req_body_id"], event["resp_body_id"], id)
	}

	requireMode(t, env.store, 0o700)
	requireMode(t, filepath.Join(env.store, "bodies"), 0o700)
	requireMode(t, filepath.Join(env.store, "history.jsonl"), 0o600)
	requestBody := filepath.Join(env.store, "bodies", id+"-req.bin")
	responseBody := filepath.Join(env.store, "bodies", id+"-resp.bin")
	requireMode(t, requestBody, 0o600)
	requireMode(t, responseBody, 0o600)
	if body, err := os.ReadFile(requestBody); err != nil || string(body) != "hello world" {
		t.Errorf("request body file = %q, err %v", body, err)
	}
	if body, err := os.ReadFile(responseBody); err != nil || string(body) != "world" {
		t.Errorf("response body file = %q, err %v", body, err)
	}

	requireExit(t, env.runWithStore(t, "history"), 0)
	requireExit(t, env.runWithStore(t, "history", "--filter", "captured"), 0)
	sent := env.runWithStore(t, "history", "--filter", "sent")
	requireExit(t, sent, 66)
	if code := errorEnvelope(t, sent)["code"]; code != "NO_MATCH" {
		t.Errorf("empty filter code = %v, want NO_MATCH", code)
	}
	noContext := env.runWithStore(t, "history", "--context", "missing")
	requireExit(t, noContext, 66)
	if code := errorEnvelope(t, noContext)["code"]; code != "NO_MATCH" {
		t.Errorf("empty context code = %v, want NO_MATCH", code)
	}
}

func TestHTTPCaptureCreatesZeroLengthBodies(t *testing.T) {
	env := newTestEnv(t)

	targetResponse := "HTTP/1.1 204 No Content\r\n" +
		"X-Empty: yes\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	rawRequest := "GET http://" + targetAddr + "/empty HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Connection: close\r\n\r\n"
	if response := sendRaw(t, proxyAddr, rawRequest); response != targetResponse {
		t.Fatalf("client response = %q, want %q", response, targetResponse)
	}
	waitObserved(t, requests)

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	got := env.runWithStore(t, "history")
	requireExit(t, got, 0)
	summary := decodeJSONObject(t, historyLines(got.stdout)[0])
	if summary["req_body_size"] != float64(0) || summary["resp_body_size"] != float64(0) {
		t.Errorf("body sizes = (%v, %v), want (0, 0)", summary["req_body_size"], summary["resp_body_size"])
	}
	id, _ := summary["id"].(string)
	for _, direction := range []string{"req", "resp"} {
		path := filepath.Join(env.store, "bodies", id+"-"+direction+".bin")
		requireMode(t, path, 0o600)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s body: %v", direction, err)
		}
		if info.Size() != 0 {
			t.Errorf("%s body size = %d, want a zero-length file", direction, info.Size())
		}
	}
}

func TestHTTPCaptureHandlesChunkedBodies(t *testing.T) {
	env := newTestEnv(t)

	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"3\r\nabc\r\n" +
		"2\r\nde\r\n" +
		"0\r\n\r\n"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	rawRequest := "POST http://" + targetAddr + "/chunked HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n" +
		"6\r\n world\r\n" +
		"0\r\n\r\n"

	response := sendRaw(t, proxyAddr, rawRequest)
	wantResponse := "HTTP/1.1 200 OK\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nabcde\r\n" +
		"0\r\n\r\n"
	if response != wantResponse {
		t.Fatalf("client response = %q, want %q", response, wantResponse)
	}

	observed := waitObserved(t, requests)
	if string(observed.body) != "hello world" {
		t.Errorf("target body = %q, want the decoded body", observed.body)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	got := env.runWithStore(t, "history")
	requireExit(t, got, 0)
	summary := decodeJSONObject(t, historyLines(got.stdout)[0])
	if summary["req_body_size"] != float64(len("hello world")) {
		t.Errorf("req_body_size = %v, want decoded size %d", summary["req_body_size"], len("hello world"))
	}
	if summary["resp_body_size"] != float64(len("abcde")) {
		t.Errorf("resp_body_size = %v, want decoded size %d", summary["resp_body_size"], len("abcde"))
	}
	id, _ := summary["id"].(string)
	if body, err := os.ReadFile(filepath.Join(env.store, "bodies", id+"-req.bin")); err != nil || string(body) != "hello world" {
		t.Errorf("stored request body = %q, err %v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(env.store, "bodies", id+"-resp.bin")); err != nil || string(body) != "abcde" {
		t.Errorf("stored response body = %q, err %v", body, err)
	}
}

func TestHTTPCaptureRewritesHostToRequestTarget(t *testing.T) {
	env := newTestEnv(t)

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	rawRequest := "GET http://" + targetAddr + "/host HTTP/1.1\r\n" +
		"Host: wrong.example\r\n" +
		"Connection: close\r\n\r\n"
	_ = sendRaw(t, proxyAddr, rawRequest)

	observed := waitObserved(t, requests)
	if !reflect.DeepEqual(observed.headers, [][2]string{{"Host", targetAddr}, {"Connection", "close"}}) {
		t.Errorf("target headers = %#v, want Host rewritten to the request-target authority", observed.headers)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	event := decodeJSONObject(t, string(rawHistory))
	requireHeaderPairs(t, event["request_headers"], [][2]string{{"Host", targetAddr}, {"Connection", "close"}})
}

func TestHTTPCaptureRecordsAndStripsRepeaterContext(t *testing.T) {
	env := newTestEnv(t)

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	rawRequest := "GET http://" + targetAddr + "/context HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"X-Before: one\r\n" +
		"X-Repeater-Context: flow-a\r\n" +
		"X-After: two\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	_ = sendRaw(t, proxyAddr, rawRequest)

	observed := waitObserved(t, requests)
	want := [][2]string{
		{"Host", targetAddr},
		{"X-Before", "one"},
		{"X-After", "two"},
		{"Connection", "close"},
	}
	if !reflect.DeepEqual(observed.headers, want) {
		t.Errorf("target headers = %#v, want the context header stripped in order", observed.headers)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	got := env.runWithStore(t, "history", "--context", "flow-a")
	requireExit(t, got, 0)
	summary := decodeJSONObject(t, historyLines(got.stdout)[0])
	if summary["context"] != "flow-a" {
		t.Errorf("history context = %v, want flow-a", summary["context"])
	}
	requireExit(t, env.runWithStore(t, "history", "--context", "flow-b"), 66)

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	event := decodeJSONObject(t, string(rawHistory))
	requireHeaderPairs(t, event["request_headers"], want)
}

func TestProxyJSONEmitsCapturedEventNDJSON(t *testing.T) {
	env := newTestEnv(t)

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t, "--events")

	rawRequest := "GET http://" + targetAddr + "/json HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Connection: close\r\n\r\n"
	_ = sendRaw(t, proxyAddr, rawRequest)
	waitObserved(t, requests)

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	lines := historyLines(proxy.stdout.String())
	if len(lines) != 1 {
		t.Fatalf("proxy stdout has %d NDJSON lines, want 1:\n%s", len(lines), proxy.stdout.String())
	}
	event := decodeJSONObject(t, lines[0])
	if event["kind"] != "captured" {
		t.Errorf("event kind = %v, want captured", event["kind"])
	}
	if event["url"] != "http://"+targetAddr+"/json" {
		t.Errorf("event url = %v, want the request target", event["url"])
	}
	if strings.Contains(strings.ToLower(proxy.stderr.String()), "warning") {
		t.Errorf("proxy emitted a warning for a normal capture:\n%s", proxy.stderr.String())
	}
}

func TestHistorySkipsCorruptLinesAndBackfillsSizes(t *testing.T) {
	env := newTestEnv(t)
	bodies := filepath.Join(env.store, "bodies")
	if err := os.MkdirAll(bodies, 0o700); err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bodies, "event-a-req.bin"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write request body: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bodies, "event-a-resp.bin"), []byte("world!!"), 0o600); err != nil {
		t.Fatalf("write response body: %v", err)
	}

	lines := []string{
		`{"id":"event-a","ts":"2025-01-02T03:04:05.123456789Z","kind":"captured","method":"GET","url":"http://example.test/a","status":200,"context":"alpha"}`,
		`{"id":"corrupt","ts":`,
		`{"id":"event-b","ts":"2025-01-02T03:04:06.123456789Z","kind":"mitm_error","target_host":"example.test","reason":"scope_denied","req_body_id":"event-b","resp_body_id":"event-b"}`,
	}
	history := strings.Join(lines, "\n") + "\n" + `{"id":"partial","kind":"captured"`
	if err := os.WriteFile(filepath.Join(env.store, "history.jsonl"), []byte(history), 0o600); err != nil {
		t.Fatalf("write history: %v", err)
	}

	got := env.runWithStore(t, "history")
	requireExit(t, got, 0)
	if !strings.Contains(got.stderr, "warning") || !strings.Contains(got.stderr, "line 2") {
		t.Errorf("stderr = %q, want a warning naming corrupt line 2", got.stderr)
	}
	if strings.Contains(got.stderr, "line 4") {
		t.Errorf("truncated trailing line must be ignored silently, stderr:\n%s", got.stderr)
	}

	items := historyLines(got.stdout)
	if len(items) != 2 {
		t.Fatalf("history returned %d lines, want 2:\n%s", len(items), got.stdout)
	}

	first := decodeJSONObject(t, items[0])
	if first["id"] != "event-a" || first["context"] != "alpha" {
		t.Errorf("first summary = %v, want event-a/alpha", first)
	}
	if first["req_body_size"] != float64(5) {
		t.Errorf("first req_body_size = %v, want 5 from the body file", first["req_body_size"])
	}
	if first["resp_body_size"] != float64(7) {
		t.Errorf("first resp_body_size = %v, want 7 from the body file", first["resp_body_size"])
	}

	second := decodeJSONObject(t, items[1])
	if second["kind"] != "mitm_error" {
		t.Errorf("second kind = %v, want mitm_error", second["kind"])
	}
	if second["status"] != nil {
		t.Errorf("second status = %v, want null for mitm_error", second["status"])
	}
	if second["req_body_size"] != float64(0) || second["resp_body_size"] != float64(0) {
		t.Errorf("missing bodies must read as empty, got sizes %v/%v", second["req_body_size"], second["resp_body_size"])
	}

	requireExit(t, env.runWithStore(t, "history", "--filter", "mitm_error"), 0)
	captured := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, captured, 0)
	if len(historyLines(captured.stdout)) != 1 {
		t.Errorf("--filter captured returned:\n%s", captured.stdout)
	}
	requireExit(t, env.runWithStore(t, "history", "--context", "alpha"), 0)
	noMatch := env.runWithStore(t, "history", "--context", "beta")
	requireExit(t, noMatch, 66)
	if code := errorEnvelopeAfterWarnings(t, noMatch)["code"]; code != "NO_MATCH" {
		t.Errorf("no-match code = %v, want NO_MATCH", code)
	}
}

func TestHistoryRejectsUnknownFilter(t *testing.T) {
	env := newTestEnv(t)
	got := env.runWithStore(t, "history", "--filter", "bogus")
	requireExit(t, got, 2)
	if code := errorEnvelope(t, got)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("error.code = %v, want INVALID_REQUEST", code)
	}
}

func TestHTTPCaptureRejectsNonProxyForms(t *testing.T) {
	env := newTestEnv(t)
	proxyAddr, proxy := env.startCaptureProxy(t)

	cases := []string{
		"GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n",
		"GET /origin-form HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n",
	}
	for _, request := range cases {
		response := sendRaw(t, proxyAddr, request)
		if !strings.HasPrefix(response, "HTTP/1.1 400 ") {
			t.Errorf("response = %q, want 400 for %q", firstLine(response), firstLine(request))
		}
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	history := env.runWithStore(t, "history")
	requireExit(t, history, 66)
	if code := errorEnvelope(t, history)["code"]; code != "NO_MATCH" {
		t.Errorf("history code = %v, want NO_MATCH", code)
	}
}

func TestHistoryRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	history, ok := doc.Command.find("mitt history")
	if !ok {
		t.Fatal("spec missing mitt history")
	}
	if !containsString(history.ErrorCodes, "NO_MATCH") {
		t.Errorf("history error_codes = %v, want NO_MATCH", history.ErrorCodes)
	}
	filter, ok := flagByName(history.Flags, "filter")
	if !ok {
		t.Fatal("history missing --filter")
	}
	wantKinds := []string{"captured", "replayed", "sent", "mitm_error"}
	if !reflect.DeepEqual(filter.Enum, wantKinds) {
		t.Errorf("--filter enum = %v, want %v", filter.Enum, wantKinds)
	}
	contextFlag, ok := flagByName(history.Flags, "context")
	if !ok {
		t.Fatal("history missing --context")
	}
	if contextFlag.Type != "string" {
		t.Errorf("--context type = %q, want string", contextFlag.Type)
	}
	if !strings.Contains(contextFlag.Description, "exactly matches") {
		t.Errorf("--context description = %q, want the exact-match semantics", contextFlag.Description)
	}
	if history.OutputSchema == nil {
		t.Fatal("history must register a success output schema")
	}
	properties, _ := history.OutputSchema["properties"].(map[string]any)
	contextProperty, _ := properties["context"].(map[string]any)
	if contextProperty["type"] != "string" {
		t.Errorf("history output schema context = %v, want a string property", contextProperty)
	}
}

func errorEnvelopeAfterWarnings(t *testing.T, got result) map[string]any {
	t.Helper()
	if strings.TrimSpace(got.stdout) != "" {
		t.Fatalf("stdout must not contain data on error, got:\n%s", got.stdout)
	}
	lines := historyLines(got.stderr)
	if len(lines) == 0 {
		t.Fatal("stderr has no error object")
	}
	envelope := decodeJSONObject(t, lines[len(lines)-1])
	errObj, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("last stderr line is not an error envelope: %s", lines[len(lines)-1])
	}
	return errObj
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\r\n")
	return line
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}
