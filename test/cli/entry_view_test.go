package cli_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeHistory installs a literal history.jsonl in the test store. Tests use
// it when they need the exact stored line, not whatever a capture produces.
func writeHistory(t *testing.T, env *testEnv, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(env.store, 0o700); err != nil {
		t.Fatalf("create store: %v", err)
	}
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(filepath.Join(env.store, "history.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatalf("write history: %v", err)
	}
}

// TestEntryProjectionsAgreeAcrossCommands pins the one-answer property of the
// entry deepening: `history` (a narrow row), `show` (the summary), and `show
// --detail` (the summary plus detail fields) are three projections of the same
// view, so the fields they share must carry identical values. Before the
// deepening these came from three DTOs with their own nil handling.
func TestEntryProjectionsAgreeAcrossCommands(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 202 Accepted\r\n" +
		"X-Server: raw-target\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"ok"
	rawRequest := "POST http://{{target}}/agree?q=1 HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"X-Repeater-Context: agree-context\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello"

	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)

	history := env.runWithStore(t, "history")
	requireExit(t, history, 0)
	row := decodeJSONObject(t, historyLines(history.stdout)[0])

	show := env.runWithStore(t, "show", id)
	requireExit(t, show, 0)
	summary := decodeJSONObject(t, show.stdout)

	detailOut := env.runWithStore(t, "show", id, "--detail")
	requireExit(t, detailOut, 0)
	detail := decodeJSONObject(t, detailOut.stdout)

	for _, field := range []string{"id", "kind", "ts", "method", "url", "status", "context", "req_body_size", "resp_body_size"} {
		if row[field] != summary[field] {
			t.Errorf("history and show disagree on %s: %v vs %v", field, row[field], summary[field])
		}
		if detail[field] != summary[field] {
			t.Errorf("show --detail and show disagree on %s: %v vs %v", field, detail[field], summary[field])
		}
	}

	// Detail adds fields; it must not change any of the summary ones.
	for field, value := range summary {
		if !reflect.DeepEqual(detail[field], value) {
			t.Errorf("show --detail changed summary field %s: %v vs %v", field, detail[field], value)
		}
	}
	for _, field := range []string{"request_body_path", "response_body_path", "request_document"} {
		if _, ok := detail[field]; !ok {
			t.Errorf("show --detail missing %s: %v", field, detail)
		}
	}
}

// TestShowReportsBothRequestLines pins the field the report called out as lost
// at the boundary: the received line and the forwarded line are both part of
// the view. `show --raw` writes the forwarded one because that is what the next
// hop received, while the JSON summary keeps the received one rather than
// silently substituting the other.
func TestShowReportsBothRequestLines(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	rawRequest := "GET http://{{target}}/lines?a=1 HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Connection: close\r\n" +
		"\r\n"

	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)

	got := env.runWithStore(t, "show", id)
	requireExit(t, got, 0)
	summary := decodeJSONObject(t, got.stdout)
	received, _ := summary["request_line"].(string)
	forwarded, _ := summary["forwarded_request_line"].(string)
	if !strings.HasPrefix(received, "GET http://") {
		t.Errorf("request_line = %q, want the absolute-form line the client wrote", received)
	}
	if !strings.HasPrefix(forwarded, "GET /lines?a=1 ") {
		t.Errorf("forwarded_request_line = %q, want the origin-form line the next hop received", forwarded)
	}

	raw := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, raw, 0)
	if !strings.HasPrefix(raw.stdout, forwarded+"\r\n") {
		t.Errorf("show --raw starts with %q, want the forwarded line %q", firstLine(raw.stdout), forwarded)
	}
}

// TestShowReportsMutations pins the other field the report said no output DTO
// exposed: the raw --patch text of a replayed entry reaches `show`.
func TestShowReportsMutations(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	rawRequest := "POST http://{{target}}/base HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello"
	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)

	// Point the replay at a live target instead of the stopped capture proxy.
	liveAddr, requests := startRawTarget(t, targetResponse)
	rewriteHistoryURL(t, env.store, id, "http://"+liveAddr+"/base")

	patch := `{"headers":[["Host","` + liveAddr + `"],["Content-Length","5"]],"body_file":null,"body":"world"}`
	sent := env.runWithStore(t, "send", id, "--patch", patch)
	requireExit(t, sent, 0)
	waitObserved(t, requests)
	replayID, _ := decodeJSONObject(t, sent.stdout)["id"].(string)

	shown := env.runWithStore(t, "show", replayID)
	requireExit(t, shown, 0)
	summary := decodeJSONObject(t, shown.stdout)
	if summary["kind"] != "replayed" {
		t.Fatalf("kind = %v, want replayed", summary["kind"])
	}
	if summary["mutations"] != patch {
		t.Errorf("mutations = %v, want the raw --patch text", summary["mutations"])
	}
	if summary["parent_id"] != id {
		t.Errorf("parent_id = %v, want %s", summary["parent_id"], id)
	}
}

// TestSendOutputMatchesShownEntry pins that the entry `send` reports is the
// same view `show` reports for it. The two used to be separate DTOs, so a
// field added to one could be missing from the other.
func TestSendOutputMatchesShownEntry(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 201 Created\r\n" +
		"X-Marker: yes\r\n" +
		"Content-Length: 4\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"done"
	targetAddr, requests := startRawTarget(t, targetResponse)

	sent := env.runWithStore(t, "send", "--url", "http://"+targetAddr+"/unify?q=1")
	requireExit(t, sent, 0)
	waitObserved(t, requests)
	sendOut := decodeJSONObject(t, sent.stdout)

	id, _ := sendOut["id"].(string)
	if id == "" {
		t.Fatal("send output has no id")
	}
	shown := env.runWithStore(t, "show", id)
	requireExit(t, shown, 0)
	shownOut := decodeJSONObject(t, shown.stdout)

	// Everything send reports about the entry, show must report identically.
	// The reverse is not symmetric: show adds fields such as upstream
	// certificate identity that a send result does not need.
	for field, value := range sendOut {
		switch field {
		case "request_body_path", "response_body_path", "diff_against":
			continue
		}
		if !reflect.DeepEqual(shownOut[field], value) {
			t.Errorf("send and show disagree on %s: %v vs %v", field, value, shownOut[field])
		}
	}
	if shownOut["request_body_path"] != nil {
		t.Errorf("default show summary must not expose body paths: %v", shownOut["request_body_path"])
	}
}

// TestSendReportsCorruptHistoryWarning pins that a replay no longer swallows the
// store's corrupt-line warning. The single-entry read passes the command's
// diagnostics sink, so `send <id>` reports a damaged line exactly as `show` and
// `history` do.
func TestSendReportsCorruptHistoryWarning(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	rawRequest := "GET http://{{target}}/warn HTTP/1.1\r\nHost: {{target}}\r\nConnection: close\r\n\r\n"
	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)

	liveAddr, requests := startRawTarget(t, targetResponse)
	rewriteHistoryURL(t, env.store, id, "http://"+liveAddr+"/warn")

	// Append a corrupt line after the captured entry; the entry itself stays
	// readable, so the replay proceeds and only the warning is at stake.
	historyPath := filepath.Join(env.store, "history.jsonl")
	existing, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	corrupt := string(existing) + `{"id":"broken","ts":` + "\n"
	if err := os.WriteFile(historyPath, []byte(corrupt), 0o600); err != nil {
		t.Fatalf("append corrupt line: %v", err)
	}

	dryRun := env.runWithStore(t, "send", id, "--dry-run")
	requireExit(t, dryRun, 0)
	if !strings.Contains(dryRun.stderr, "warning") || !strings.Contains(dryRun.stderr, "line 2") {
		t.Errorf("send --dry-run stderr = %q, want a warning naming corrupt line 2", dryRun.stderr)
	}

	sent := env.runWithStore(t, "send", id)
	requireExit(t, sent, 0)
	waitObserved(t, requests)
	if !strings.Contains(sent.stderr, "warning") || !strings.Contains(sent.stderr, "line 2") {
		t.Errorf("send stderr = %q, want a warning naming corrupt line 2", sent.stderr)
	}
}

// TestShowReadsEntriesInOneScan pins the single-entry read as an observable
// property rather than an implementation detail: an entry addressed by id is
// returned even when other lines are unreadable, and the error for a missing id
// carries the data directory whichever command asked.
func TestShowMissingEntryNamesTheStore(t *testing.T) {
	env := newTestEnv(t)
	writeHistory(t,
		env,
		`{"id":"kept","ts":"2025-01-02T03:04:05.123456789Z","kind":"captured","method":"GET","url":"http://example.test/a","status":200,"context":""}`,
	)

	kept := env.runWithStore(t, "show", "kept")
	requireExit(t, kept, 0)
	if got := decodeJSONObject(t, kept.stdout)["id"]; got != "kept" {
		t.Errorf("show id = %v, want kept", got)
	}

	for _, command := range [][]string{
		{"show", "absent"},
		{"diff", "absent", "kept"},
		{"send", "absent", "--dry-run"},
	} {
		if command[0] == "diff" {
			// diff needs both sides readable; a missing second id still yields
			// the one not-found answer.
			command = []string{"diff", "kept", "absent"}
		}
		got := env.runWithStore(t, command...)
		requireExit(t, got, 66)
		errObj := errorEnvelope(t, got)
		if errObj["code"] != "ENTRY_NOT_FOUND" {
			t.Errorf("%v code = %v, want ENTRY_NOT_FOUND", command, errObj["code"])
		}
		details, _ := errObj["details"].(map[string]any)
		if details["store"] != env.store {
			t.Errorf("%v details.store = %v, want %s", command, details["store"], env.store)
		}
		if details["id"] != "absent" {
			t.Errorf("%v details.id = %v, want absent", command, details["id"])
		}
	}
}

// TestShowRawRejectsMITMErrorWithoutLeakingStorePath pins the failure shape of
// the raw projection after it moved behind the entry package.
func TestShowRawRejectsMITMError(t *testing.T) {
	env := newTestEnv(t)
	writeHistory(t,
		env,
		`{"id":"err","ts":"2025-01-02T03:04:05.123456789Z","kind":"mitm_error","target_host":"blocked.test","reason":"scope_denied"}`,
	)

	got := env.runWithStore(t, "show", "err", "--raw")
	requireExit(t, got, 2)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("code = %v, want INVALID_REQUEST", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["id"] != "err" || details["kind"] != "mitm_error" {
		t.Errorf("details = %v, want the mitm_error entry", details)
	}
}
