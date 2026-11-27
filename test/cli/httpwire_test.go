package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProxyNormalizesAmbiguousFraming pins the single TE+CL rule: the proxy
// reads the message as chunked, strips Content-Length before forwarding, but
// keeps the received response headers in history.
func TestProxyNormalizesAmbiguousFraming(t *testing.T) {
	env := newTestEnv(t)

	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Content-Length: 999\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n0\r\n\r\n"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	rawRequest := "POST http://" + targetAddr + "/ambiguous HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Content-Length: 999\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n0\r\n\r\n"

	response := sendRaw(t, proxyAddr, rawRequest)
	wantResponse := "HTTP/1.1 200 OK\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n0\r\n\r\n"
	if response != wantResponse {
		t.Fatalf("client response = %q, want %q", response, wantResponse)
	}

	observed := waitObserved(t, requests)
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"Host", targetAddr},
		{"Transfer-Encoding", "chunked"},
		{"Connection", "close"},
	})
	if string(observed.body) != "hello" {
		t.Errorf("target body = %q, want hello", observed.body)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	history := env.runWithStore(t, "history")
	requireExit(t, history, 0)
	lines := historyLines(history.stdout)
	if len(lines) != 1 {
		t.Fatalf("history returned %d lines, want 1:\n%s", len(lines), history.stdout)
	}
	summary := decodeJSONObject(t, lines[0])
	id, _ := summary["id"].(string)
	if id == "" {
		t.Fatalf("history summary has no id: %v", summary)
	}

	show := env.runWithStore(t, "show", id, "--detail")
	requireExit(t, show, 0)
	detail := decodeJSONObject(t, show.stdout)
	requireHeaderPairs(t, detail["request_headers"], [][2]string{
		{"Host", targetAddr},
		{"Transfer-Encoding", "chunked"},
		{"Connection", "close"},
	})
	// History keeps the headers as received, even though the client-facing
	// response had Content-Length removed.
	requireHeaderPairs(t, detail["response_headers"], [][2]string{
		{"Transfer-Encoding", "chunked"},
		{"Content-Length", "999"},
		{"Connection", "close"},
	})
}

// TestSendFromFilePreservesAmbiguousFraming pins the other half of the same
// rule: a raw --from-file request is sent verbatim, so both framing headers
// reach the target, while the stored body is the decoded chunked entity.
func TestSendFromFilePreservesAmbiguousFraming(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	request := "POST /ambiguous HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Content-Length: 999\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n0\r\n\r\n"
	path := filepath.Join(t.TempDir(), "ambiguous.txt")
	if err := os.WriteFile(path, []byte(request), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}

	got := env.runWithStore(t, "send", "--from-file", path)
	requireExit(t, got, 0)

	observed := waitObserved(t, requests)
	requireRawHeaderPairs(t, observed.headers, [][2]string{
		{"Host", targetAddr},
		{"Transfer-Encoding", "chunked"},
		{"Content-Length", "999"},
		{"Connection", "close"},
	})
	if string(observed.body) != "hello" {
		t.Errorf("target body = %q, want the decoded chunked body", observed.body)
	}

	out := decodeJSONObject(t, got.stdout)
	if out["req_body_size"] != float64(len("hello")) {
		t.Errorf("req_body_size = %v, want %d", out["req_body_size"], len("hello"))
	}
	requireHeaderPairs(t, out["request_headers"], [][2]string{
		{"Host", targetAddr},
		{"Transfer-Encoding", "chunked"},
		{"Content-Length", "999"},
		{"Connection", "close"},
	})
}

func TestShowRawKeepsReceivedResponseFraming(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Content-Length: 999\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n0\r\n\r\n"
	rawRequest := "POST http://{{target}}/ambiguous HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Content-Length: 999\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n0\r\n\r\n"

	id, targetAddr, _ := captureEntry(t, env, targetResponse, rawRequest)
	got := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, got, 0)

	// show --raw displays the request after proxy normalization (Content-Length
	// removed) and the response as received (both headers retained); the
	// chunked bodies are re-encoded as one chunk.
	want := "POST /ambiguous HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n0\r\n\r\n" +
		"HTTP/1.1 200 OK\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Content-Length: 999\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"5\r\nhello\r\n0\r\n\r\n"
	if got.stdout != want {
		t.Fatalf("show --raw bytes differ:\n got %q\nwant %q", got.stdout, want)
	}
	requestPart, responsePart, ok := strings.Cut(got.stdout, "HTTP/1.1 200 OK")
	if !ok {
		t.Fatal("show --raw output has no response status line")
	}
	if strings.Contains(requestPart, "Content-Length") {
		t.Error("show --raw reintroduced Content-Length on the normalized request")
	}
	if !strings.Contains(responsePart, "Content-Length: 999") {
		t.Error("show --raw dropped the received response Content-Length")
	}
}
