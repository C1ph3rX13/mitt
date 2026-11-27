package cli_test

import (
	"encoding/json"
	"os"
	"testing"
)

// writeChunkedEmptyBodyEntry installs one captured entry whose request declares
// chunked framing and whose stored body is empty. It is written literally
// because the shape is what matters here, and running a capture proxy to
// produce it would cost a process and two sockets for no extra coverage.
func writeChunkedEmptyBodyEntry(t *testing.T, env *testEnv, id, rawURL string) {
	t.Helper()
	event := map[string]any{
		"id":              id,
		"ts":              "2026-01-02T03:04:05.123456789Z",
		"kind":            "captured",
		"client_addr":     "127.0.0.1:4321",
		"method":          "POST",
		"url":             rawURL,
		"http_version":    "http/1.1",
		"request_line":    "POST /empty HTTP/1.1",
		"request_headers": [][2]string{{"Host", "127.0.0.1"}, {"Transfer-Encoding", "chunked"}, {"Connection", "close"}},
		"req_body_size":   0,
		"status":          200,
		"response_line":   "HTTP/1.1 200 OK",
		"context":         "",
	}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	writeHistory(t, env, string(line))
}

// TestSendDryRunEmptyPatchKeepsTheBytes pins the replay rendering rule through
// the CLI: a no-op patch cannot change the request a replay writes. The chunked
// base with an empty body is the case that used to diverge, because the patched
// path kept Transfer-Encoding while the unpatched path recomputed a length.
//
// Both invocations are pure reads of the store, so the test needs no proxy, no
// target, and no goroutine; the only external process is the bounded `mitt`
// binary itself.
func TestSendDryRunEmptyPatchKeepsTheBytes(t *testing.T) {
	env := newTestEnv(t)
	writeChunkedEmptyBodyEntry(t, env, "chunked-empty", "http://127.0.0.1:1/empty")

	plain := env.runWithStore(t, "send", "chunked-empty", "--dry-run")
	requireExit(t, plain, 0)
	patched := env.runWithStore(t, "send", "chunked-empty", "--patch", `{}`, "--dry-run")
	requireExit(t, patched, 0)

	plainOut := decodeJSONObject(t, plain.stdout)
	patchedOut := decodeJSONObject(t, patched.stdout)
	plainPreview := requireBase64Value(t, plainOut["request_bytes_preview"])
	patchedPreview := requireBase64Value(t, patchedOut["request_bytes_preview"])

	if string(patchedPreview) != string(plainPreview) {
		t.Errorf("empty patch changed the preview bytes:\n plain:   %q\n patched: %q", plainPreview, patchedPreview)
	}
	if patchedOut["preview_total_bytes"] != plainOut["preview_total_bytes"] {
		t.Errorf("empty patch changed preview_total_bytes: plain %v, patched %v",
			plainOut["preview_total_bytes"], patchedOut["preview_total_bytes"])
	}

	want := "POST /empty HTTP/1.1\r\n" +
		"Host: 127.0.0.1:1\r\n" +
		"Content-Length: 0\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	if string(plainPreview) != want {
		t.Errorf("preview bytes = %q, want %q", plainPreview, want)
	}
}

// TestSendEmptyPatchWritesTheSameWireBytes replays the same entry to a live
// target with and without an empty patch and compares what the target actually
// received. A dry-run agreement would still allow the two paths to diverge
// between preview and dispatch; this pins the bytes on the socket.
func TestSendEmptyPatchWritesTheSameWireBytes(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"

	plainTarget, plainRequests := startRawTarget(t, targetResponse)
	writeChunkedEmptyBodyEntry(t, env, "plain", "http://"+plainTarget+"/empty")
	plain := env.runWithStore(t, "send", "plain")
	requireExit(t, plain, 0)
	plainObserved := waitObserved(t, plainRequests)

	patchedTarget, patchedRequests := startRawTarget(t, targetResponse)
	writeChunkedEmptyBodyEntry(t, env, "patched", "http://"+patchedTarget+"/empty")
	patched := env.runWithStore(t, "send", "patched", "--patch", `{}`)
	requireExit(t, patched, 0)
	patchedObserved := waitObserved(t, patchedRequests)

	wantHeaders := [][2]string{
		{"Host", plainTarget},
		{"Content-Length", "0"},
		{"Connection", "close"},
	}
	requireRawHeaderPairs(t, plainObserved.headers, wantHeaders)
	requireRawHeaderPairs(t, patchedObserved.headers, [][2]string{
		{"Host", patchedTarget},
		{"Content-Length", "0"},
		{"Connection", "close"},
	})
	if plainObserved.line != "POST /empty HTTP/1.1" || patchedObserved.line != "POST /empty HTTP/1.1" {
		t.Errorf("request lines = %q / %q, want POST /empty HTTP/1.1", plainObserved.line, patchedObserved.line)
	}
	if len(plainObserved.body) != 0 || len(patchedObserved.body) != 0 {
		t.Errorf("target bodies = %q / %q, want empty", plainObserved.body, patchedObserved.body)
	}

	// The replayed entries report the same request sizes on both paths.
	for name, out := range map[string]map[string]any{
		"plain":   decodeJSONObject(t, plain.stdout),
		"patched": decodeJSONObject(t, patched.stdout),
	} {
		if out["req_body_size"] != float64(0) {
			t.Errorf("%s req_body_size = %v, want 0", name, out["req_body_size"])
		}
		if out["status"] != float64(200) {
			t.Errorf("%s status = %v, want 200", name, out["status"])
		}
	}
}

// TestSendDryRunRejectsUnsupportedTLSProfile pins that a preview accepts the
// same request surface a send does: a profile the declared enum rejects is not
// previewable either, and the rejected preview writes nothing. The library-level
// half of the same rule — DryRun running the precondition Send runs — is covered
// by internal/replay, because the declared flag enum answers first from the CLI.
func TestSendDryRunRejectsUnsupportedTLSProfile(t *testing.T) {
	env := newTestEnv(t)
	got := env.runWithStore(t, "send", "--url", "https://example.test/", "--tls-profile", "safari", "--dry-run")
	requireExit(t, got, 2)
	if code := errorEnvelope(t, got)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("code = %v, want INVALID_REQUEST", code)
	}
	if _, err := os.Stat(env.store); !os.IsNotExist(err) {
		t.Errorf("rejected dry run touched the data dir: %v", err)
	}
}
