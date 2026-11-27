package cli_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSendDiffAgainstSummarizesBinaryBodies(t *testing.T) {
	env := newTestEnv(t)
	bodyA := []byte{0x00, 0x01, 0xfe, 0xff}
	bodyB := []byte{0x00, 0x02, 0x03}
	responseA := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyA)) +
		"Connection: close\r\n" +
		"\r\n" + string(bodyA)
	rawRequest := "GET http://{{target}}/binary HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	id, _, _ := captureEntry(t, env, responseA, rawRequest)

	responseB := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyB)) +
		"Connection: close\r\n" +
		"\r\n" + string(bodyB)
	liveAddr, requests := startRawTarget(t, responseB)
	rewriteHistoryURL(t, env.store, id, "http://"+liveAddr+"/binary")

	got := env.runWithStore(t, "send", id, "--diff-against")
	requireExit(t, got, 0)
	waitObserved(t, requests)
	summary, _ := decodeJSONObject(t, got.stdout)["diff_against"].(map[string]any)
	body, _ := summary["body"].(map[string]any)
	if body["type"] != "binary" || body["same"] != false {
		t.Errorf("body type/same = %v/%v, want binary/false", body["type"], body["same"])
	}
	if body["similarity"] != float64(0) {
		t.Errorf("body.similarity = %v, want 0 for different binary bodies", body["similarity"])
	}
	if body["size_delta"] != float64(-1) {
		t.Errorf("body.size_delta = %v, want -1", body["size_delta"])
	}
	if _, present := body["unified_diff"]; present {
		t.Errorf("summary must not carry a unified_diff: %v", body)
	}
}

func TestSendDiffAgainstSurvivesUpstream5xx(t *testing.T) {
	env := newTestEnv(t)
	bodyA := "healthy\n"
	responseA := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyA)) +
		"Connection: close\r\n" +
		"\r\n" + bodyA
	rawRequest := "GET http://{{target}}/down HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	id, _, _ := captureEntry(t, env, responseA, rawRequest)

	bodyB := "down\n"
	responseB := "HTTP/1.1 503 Service Unavailable\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyB)) +
		"Connection: close\r\n" +
		"\r\n" + bodyB
	liveAddr, requests := startRawTarget(t, responseB)
	rewriteHistoryURL(t, env.store, id, "http://"+liveAddr+"/down")

	got := env.runWithStore(t, "send", id, "--diff-against")
	requireExit(t, got, 75)
	waitObserved(t, requests)
	out := decodeJSONObject(t, got.stdout)
	if out["status"] != float64(503) {
		t.Errorf("send status = %v, want the 503 response", out["status"])
	}
	summary, ok := out["diff_against"].(map[string]any)
	if !ok {
		t.Fatalf("5xx send must still carry diff_against: %s", got.stdout)
	}
	status, _ := summary["status"].(map[string]any)
	if status["a"] != float64(200) || status["b"] != float64(503) {
		t.Errorf("diff_against.status = %v, want 200 -> 503", status)
	}
	if code := errorEnvelopeWithStdoutData(t, got)["code"]; code != "TEMP_FAILURE" {
		t.Errorf("stderr code = %v, want TEMP_FAILURE", code)
	}
}

func TestSendDiffAgainstRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	send, ok := doc.Command.find("mitt send")
	if !ok {
		t.Fatal("spec missing mitt send")
	}
	flag, ok := flagByName(send.Flags, "diff-against")
	if !ok || flag.Type != "bool" {
		t.Errorf("send --diff-against flag = %+v, want a bool flag", flag)
	}
	if send.OutputSchema == nil {
		t.Fatal("send must register a success output schema")
	}
	properties, _ := send.OutputSchema["properties"].(map[string]any)
	if _, ok := properties["diff_against"]; !ok {
		t.Errorf("send output schema properties missing diff_against: %v", properties)
	}
	required, _ := send.OutputSchema["required"].([]any)
	for _, field := range required {
		if field == "diff_against" {
			t.Errorf("diff_against must be optional in the send output schema: %v", required)
		}
	}
	summarySchema, ok := doc.Schemas["send_diff_against"].(map[string]any)
	if !ok {
		t.Fatalf("spec schemas missing send_diff_against: %v", doc.Schemas)
	}
	if got := summarySchema["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("send_diff_against $schema = %v", got)
	}
	requireSchemaFields(t, summarySchema, "status", "headers", "body", "changed")

	bodyA := "before\n"
	bodyB := "after\n"
	responseA := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyA)) +
		"Connection: close\r\n" +
		"\r\n" + bodyA
	rawRequest := "GET http://{{target}}/spec HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	id, _, _ := captureEntry(t, env, responseA, rawRequest)
	responseB := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyB)) +
		"Connection: close\r\n" +
		"\r\n" + bodyB
	liveAddr, requests := startRawTarget(t, responseB)
	rewriteHistoryURL(t, env.store, id, "http://"+liveAddr+"/spec")

	got := env.runWithStore(t, "send", id, "--diff-against")
	requireExit(t, got, 0)
	waitObserved(t, requests)
	out := decodeJSONObject(t, got.stdout)
	requireSchemaValid(t, send.OutputSchema, out, "send --diff-against output")
	requireSchemaValid(t, summarySchema, out["diff_against"], "send_diff_against summary")
}

func TestSendDiffAgainstRejectsParentWithoutResponse(t *testing.T) {
	env := newTestEnv(t)
	proxyAddr, proxy := env.startCaptureProxy(t, "--deny-targets", "blocked.example")
	response := sendRaw(t, proxyAddr,
		"GET http://blocked.example/private HTTP/1.1\r\n"+
			"Host: blocked.example\r\n"+
			"Connection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 403 ") {
		t.Fatalf("blocked request response = %q, want 403", firstLine(response))
	}
	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	history := env.runWithStore(t, "history", "--filter", "mitm_error")
	requireExit(t, history, 0)
	lines := historyLines(history.stdout)
	if len(lines) != 1 {
		t.Fatalf("history --filter mitm_error returned %d lines, want 1", len(lines))
	}
	id, _ := decodeJSONObject(t, lines[0])["id"].(string)

	got := env.runWithStore(t, "send", id, "--diff-against")
	requireExit(t, got, 2)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("error.code = %v, want INVALID_REQUEST", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["id"] != id || details["kind"] != "mitm_error" {
		t.Errorf("error.details = %v, want the parent id and mitm_error kind", details)
	}
	if details["reason"] != "scope_denied" {
		t.Errorf("error.details.reason = %v, want scope_denied", details["reason"])
	}

	unexpected := env.runWithStore(t, "history")
	requireExit(t, unexpected, 0)
	if lines := historyLines(unexpected.stdout); len(lines) != 1 {
		t.Errorf("a failed --diff-against parent check must not send: %v", lines)
	}
}

func TestSendDiffAgainstMissingParentReturnsEntryNotFound(t *testing.T) {
	env := newTestEnv(t)
	got := env.runWithStore(t, "send", "missing-entry", "--diff-against")
	requireExit(t, got, 66)
	if code := errorEnvelope(t, got)["code"]; code != "ENTRY_NOT_FOUND" {
		t.Errorf("error.code = %v, want ENTRY_NOT_FOUND", code)
	}
}

func TestSendDiffAgainstRejectsNonEntryBases(t *testing.T) {
	env := newTestEnv(t)
	requestPath := filepath.Join(t.TempDir(), "request.txt")
	raw := "GET http://127.0.0.1:1/from-file HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n"
	if err := os.WriteFile(requestPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw request: %v", err)
	}

	cases := []struct {
		name string
		args []string
		code string
	}{
		{name: "url", args: []string{"send", "--diff-against", "--url", "http://127.0.0.1:1/"}, code: "INVALID_REQUEST"},
		{name: "from-file", args: []string{"send", "--diff-against", "--from-file", requestPath}, code: "INVALID_REQUEST"},
		{name: "no base", args: []string{"send", "--diff-against"}, code: "BASE_CONFLICT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := env.runWithStore(t, tc.args...)
			requireExit(t, got, 2)
			if code := errorEnvelope(t, got)["code"]; code != tc.code {
				t.Errorf("%v code = %v, want %s", tc.args, code, tc.code)
			}
		})
	}
	if _, err := os.Stat(env.store); !os.IsNotExist(err) {
		t.Errorf("rejected --diff-against combinations touched the data dir: %v", err)
	}
}

func TestSendDiffAgainstRejectsDryRunAndSchema(t *testing.T) {
	env := newTestEnv(t)
	rawRequest := "GET http://{{target}}/dry HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	id, _, _ := captureEntry(t, env, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok", rawRequest)

	for _, args := range [][]string{
		{"send", id, "--diff-against", "--dry-run"},
		{"send", "--schema", "--diff-against"},
	} {
		got := env.runWithStore(t, args...)
		requireExit(t, got, 2)
		if code := errorEnvelope(t, got)["code"]; code != "INVALID_REQUEST" {
			t.Errorf("%v code = %v, want INVALID_REQUEST", args, code)
		}
	}

	history := env.runWithStore(t, "history")
	requireExit(t, history, 0)
	if lines := historyLines(history.stdout); len(lines) != 1 {
		t.Errorf("rejected --diff-against combinations sent or recorded a request: %v", lines)
	}
}

func TestSendDiffAgainstReportsUnchangedResponse(t *testing.T) {
	env := newTestEnv(t)
	body := "same body\n"
	response := "HTTP/1.1 200 OK\r\n" +
		"X-Stable: yes\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"Connection: close\r\n" +
		"\r\n" + body
	rawRequest := "GET http://{{target}}/same HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	id, _, _ := captureEntry(t, env, response, rawRequest)

	// The capture target stays up for the entire test and serves the same
	// response again, so the replay has nothing to report.
	got := env.runWithStore(t, "send", id, "--diff-against")
	requireExit(t, got, 0)

	summary, ok := decodeJSONObject(t, got.stdout)["diff_against"].(map[string]any)
	if !ok {
		t.Fatalf("diff_against missing from output: %s", got.stdout)
	}
	if summary["changed"] != false {
		t.Errorf("diff_against.changed = %v, want false", summary["changed"])
	}
	status, _ := summary["status"].(map[string]any)
	if status["same"] != true || status["a"] != float64(200) || status["b"] != float64(200) {
		t.Errorf("diff_against.status = %v, want same 200/200", status)
	}
	headers, _ := summary["headers"].(map[string]any)
	if headers["same"] != true {
		t.Errorf("diff_against.headers.same = %v, want true", headers["same"])
	}
	bodySummary, _ := summary["body"].(map[string]any)
	if bodySummary["same"] != true || bodySummary["similarity"] != float64(1) {
		t.Errorf("body same/similarity = %v/%v, want true/1", bodySummary["same"], bodySummary["similarity"])
	}
	if bodySummary["size_delta"] != float64(0) {
		t.Errorf("body.size_delta = %v, want 0", bodySummary["size_delta"])
	}
	if bodySummary["sha256_a"] != bodySummary["sha256_b"] {
		t.Errorf("body hashes differ for identical responses: %v vs %v", bodySummary["sha256_a"], bodySummary["sha256_b"])
	}
}

func TestSendWithoutDiffAgainstOmitsSummary(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	got := env.runWithStore(t, "send", "--url", "http://"+targetAddr+"/plain")
	requireExit(t, got, 0)
	waitObserved(t, requests)
	if _, present := decodeJSONObject(t, got.stdout)["diff_against"]; present {
		t.Errorf("send without --diff-against must not emit diff_against: %s", got.stdout)
	}
}

func hasChangedHeader(changes []any, name string, a, b []string) bool {
	for _, raw := range changes {
		change, _ := raw.(map[string]any)
		if change["name"] != name {
			continue
		}
		return equalStrings(jsonStrings(change["a"]), a) && equalStrings(jsonStrings(change["b"]), b)
	}
	return false
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestSendDiffAgainstSummarizesParentResponse(t *testing.T) {
	env := newTestEnv(t)
	bodyA := "hello"
	bodyB := "hello\nworld\n"
	responseA := "HTTP/1.1 200 OK\r\n" +
		"X-Same: keep\r\n" +
		"X-Removed: old\r\n" +
		"X-Changed: one\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyA)) +
		"Connection: close\r\n" +
		"\r\n" + bodyA
	rawRequest := "GET http://{{target}}/before HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	id, _, _ := captureEntry(t, env, responseA, rawRequest)

	responseB := "HTTP/1.1 403 Forbidden\r\n" +
		"X-Same: keep\r\n" +
		"X-Added: new\r\n" +
		"X-Changed: two\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyB)) +
		"Connection: close\r\n" +
		"\r\n" + bodyB
	liveAddr, requests := startRawTarget(t, responseB)
	rewriteHistoryURL(t, env.store, id, "http://"+liveAddr+"/after")

	got := env.runWithStore(t, "send", id, "--diff-against")
	requireExit(t, got, 0)
	waitObserved(t, requests)

	out := decodeJSONObject(t, got.stdout)
	if out["parent_id"] != id {
		t.Errorf("parent_id = %v, want %s", out["parent_id"], id)
	}
	summary, ok := out["diff_against"].(map[string]any)
	if !ok {
		t.Fatalf("diff_against = %T, want an object: %v", out["diff_against"], out)
	}
	if summary["changed"] != true {
		t.Errorf("diff_against.changed = %v, want true", summary["changed"])
	}

	status, ok := summary["status"].(map[string]any)
	if !ok {
		t.Fatalf("diff_against.status = %T, want an object", summary["status"])
	}
	if status["same"] != false || status["a"] != float64(200) || status["b"] != float64(403) {
		t.Errorf("diff_against.status = %v, want same=false a=200 b=403", status)
	}

	headers, ok := summary["headers"].(map[string]any)
	if !ok {
		t.Fatalf("diff_against.headers = %T, want an object", summary["headers"])
	}
	if headers["same"] != false {
		t.Errorf("diff_against.headers.same = %v, want false", headers["same"])
	}
	if added := jsonPairs(headers["added"]); !hasPair(added, [2]string{"X-Added", "new"}) {
		t.Errorf("headers.added = %v, want X-Added: new", added)
	}
	if removed := jsonPairs(headers["removed"]); !hasPair(removed, [2]string{"X-Removed", "old"}) {
		t.Errorf("headers.removed = %v, want X-Removed: old", removed)
	}
	changedHeaders, _ := headers["changed"].([]any)
	if !hasChangedHeader(changedHeaders, "X-Changed", []string{"one"}, []string{"two"}) {
		t.Errorf("headers.changed = %v, want X-Changed one -> two", headers["changed"])
	}

	body, ok := summary["body"].(map[string]any)
	if !ok {
		t.Fatalf("diff_against.body = %T, want an object", summary["body"])
	}
	if body["type"] != "text" || body["same"] != false || body["truncated"] != false {
		t.Errorf("body type/same/truncated = %v/%v/%v, want text/false/false", body["type"], body["same"], body["truncated"])
	}
	if body["size_a"] != float64(len(bodyA)) || body["size_b"] != float64(len(bodyB)) {
		t.Errorf("body sizes = %v/%v, want %d/%d", body["size_a"], body["size_b"], len(bodyA), len(bodyB))
	}
	if body["size_delta"] != float64(len(bodyB)-len(bodyA)) {
		t.Errorf("body.size_delta = %v, want %d", body["size_delta"], len(bodyB)-len(bodyA))
	}
	if body["sha256_a"] != sha256Hex(bodyA) || body["sha256_b"] != sha256Hex(bodyB) {
		t.Errorf("body hashes = %v/%v, want sha256 of each body", body["sha256_a"], body["sha256_b"])
	}
	similarity, ok := body["similarity"].(float64)
	if !ok {
		t.Fatalf("body.similarity = %T, want a number", body["similarity"])
	}
	if math.Abs(similarity-2.0/3.0) > 1e-9 {
		t.Errorf("body.similarity = %v, want 2/3 for one shared line of three", similarity)
	}
	if _, present := body["unified_diff"]; present {
		t.Errorf("diff_against is a summary and must not carry a unified_diff: %v", body)
	}
}
