package cli_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// captureDiffEntries captures one GET response per supplied fixture through a
// single proxy session and returns the history entry ids in capture order.
func captureDiffEntries(t *testing.T, env *testEnv, responses ...string) []string {
	t.Helper()
	targets := make([]string, len(responses))
	requests := make([]<-chan observedRequest, len(responses))
	for i, response := range responses {
		targets[i], requests[i] = startRawTarget(t, response)
	}

	proxyAddr, proxy := env.startCaptureProxy(t)
	for i, target := range targets {
		sendRaw(t, proxyAddr, "GET http://"+target+"/compare HTTP/1.1\r\n"+
			"Host: "+target+"\r\n"+
			"Connection: close\r\n"+
			"\r\n")
		waitObserved(t, requests[i])
	}
	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	history := env.runWithStore(t, "history")
	requireExit(t, history, 0)
	lines := historyLines(history.stdout)
	if len(lines) != len(responses) {
		t.Fatalf("history returned %d lines, want %d:\n%s", len(lines), len(responses), history.stdout)
	}
	ids := make([]string, len(lines))
	for i, line := range lines {
		summary := decodeJSONObject(t, line)
		id, _ := summary["id"].(string)
		if id == "" {
			t.Fatalf("history line %d has no id: %v", i, summary)
		}
		ids[i] = id
	}
	return ids
}

func jsonPairs(value any) [][2]string {
	pairs := [][2]string{}
	items, _ := value.([]any)
	for _, item := range items {
		pair, _ := item.([]any)
		if len(pair) != 2 {
			continue
		}
		name, _ := pair[0].(string)
		value, _ := pair[1].(string)
		pairs = append(pairs, [2]string{name, value})
	}
	return pairs
}

func hasPair(pairs [][2]string, want [2]string) bool {
	for _, pair := range pairs {
		if pair == want {
			return true
		}
	}
	return false
}

func jsonStrings(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, _ := item.(string)
		out = append(out, text)
	}
	return out
}

func TestDiffReportsStatusHeaderAndTextBodyChanges(t *testing.T) {
	env := newTestEnv(t)
	bodyA := "caf\u00e9\nalpha\nbeta\n"
	bodyB := "caf\u00e9\nalpha\nBETA\ngamma\n"
	responseA := "HTTP/1.1 200 OK\r\n" +
		"X-Same: yes\r\n" +
		"X-Removed: old\r\n" +
		"X-Changed: one\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyA)) +
		"Connection: close\r\n" +
		"\r\n" + bodyA
	responseB := "HTTP/1.1 201 Created\r\n" +
		"X-Same: yes\r\n" +
		"X-Added: new\r\n" +
		"X-Changed: two\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyB)) +
		"Connection: close\r\n" +
		"\r\n" + bodyB

	ids := captureDiffEntries(t, env, responseA, responseB)
	idA, idB := ids[0], ids[1]

	got := env.runWithStore(t, "diff", idA, idB)
	requireExit(t, got, 0)
	result := decodeJSONObject(t, got.stdout)

	entryA, ok := result["a"].(map[string]any)
	if !ok {
		t.Fatalf("diff.a = %T, want an object", result["a"])
	}
	entryB, ok := result["b"].(map[string]any)
	if !ok {
		t.Fatalf("diff.b = %T, want an object", result["b"])
	}
	if entryA["id"] != idA || entryA["status"] != float64(200) {
		t.Errorf("diff.a = %v, want id %s status 200", entryA, idA)
	}
	if entryB["id"] != idB || entryB["status"] != float64(201) {
		t.Errorf("diff.b = %v, want id %s status 201", entryB, idB)
	}

	statusDiff, ok := result["status"].(map[string]any)
	if !ok {
		t.Fatalf("diff.status = %T, want an object", result["status"])
	}
	if statusDiff["same"] != false || statusDiff["a"] != float64(200) || statusDiff["b"] != float64(201) {
		t.Errorf("diff.status = %v, want same=false a=200 b=201", statusDiff)
	}

	headers, ok := result["headers"].(map[string]any)
	if !ok {
		t.Fatalf("diff.headers = %T, want an object", result["headers"])
	}
	if headers["same"] != false {
		t.Errorf("diff.headers.same = %v, want false", headers["same"])
	}
	added := jsonPairs(headers["added"])
	removed := jsonPairs(headers["removed"])
	if !hasPair(added, [2]string{"X-Added", "new"}) {
		t.Errorf("headers.added = %v, want X-Added: new", added)
	}
	if !hasPair(removed, [2]string{"X-Removed", "old"}) {
		t.Errorf("headers.removed = %v, want X-Removed: old", removed)
	}
	if hasPair(added, [2]string{"X-Same", "yes"}) || hasPair(removed, [2]string{"X-Same", "yes"}) {
		t.Errorf("headers for the unchanged X-Same must not appear in added/removed: %v / %v", added, removed)
	}
	changedFound := false
	changed, _ := headers["changed"].([]any)
	for _, item := range changed {
		change, _ := item.(map[string]any)
		if change["name"] != "X-Changed" {
			continue
		}
		changedFound = true
		if got := jsonStrings(change["a"]); len(got) != 1 || got[0] != "one" {
			t.Errorf("X-Changed a = %v, want [one]", change["a"])
		}
		if got := jsonStrings(change["b"]); len(got) != 1 || got[0] != "two" {
			t.Errorf("X-Changed b = %v, want [two]", change["b"])
		}
	}
	if !changedFound {
		t.Errorf("headers.changed = %v, want an X-Changed entry", changed)
	}

	body, ok := result["body"].(map[string]any)
	if !ok {
		t.Fatalf("diff.body = %T, want an object", result["body"])
	}
	if body["type"] != "text" {
		t.Errorf("body.type = %v, want text", body["type"])
	}
	if body["same"] != false || body["truncated"] != false {
		t.Errorf("body same/truncated = %v/%v, want false/false", body["same"], body["truncated"])
	}
	if body["size_a"] != float64(len(bodyA)) || body["size_b"] != float64(len(bodyB)) {
		t.Errorf("body sizes = %v/%v, want %d/%d", body["size_a"], body["size_b"], len(bodyA), len(bodyB))
	}
	if body["sha256_a"] != sha256Hex(bodyA) || body["sha256_b"] != sha256Hex(bodyB) {
		t.Errorf("body hashes = %v/%v, want sha256 of each body", body["sha256_a"], body["sha256_b"])
	}
	unified, _ := body["unified_diff"].(string)
	for _, want := range []string{"--- a", "+++ b", "@@", "-beta", "+BETA", "+gamma", " alpha"} {
		if !strings.Contains(unified, want) {
			t.Errorf("unified diff missing %q:\n%s", want, unified)
		}
	}
}

func TestDiffReportsBinaryBodiesWithHashes(t *testing.T) {
	env := newTestEnv(t)
	bodyA := []byte{0x00, 0x01, 0xfe, 0xff}
	bodyB := []byte{0x00, 0x01, 0xfe, 0x7f}
	responseA := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyA)) +
		"Connection: close\r\n" +
		"\r\n" + string(bodyA)
	responseB := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyB)) +
		"Connection: close\r\n" +
		"\r\n" + string(bodyB)

	ids := captureDiffEntries(t, env, responseA, responseB)

	got := env.runWithStore(t, "diff", ids[0], ids[1])
	requireExit(t, got, 0)
	result := decodeJSONObject(t, got.stdout)
	body, ok := result["body"].(map[string]any)
	if !ok {
		t.Fatalf("diff.body = %T, want an object", result["body"])
	}
	if body["type"] != "binary" {
		t.Errorf("body.type = %v, want binary", body["type"])
	}
	if body["same"] != false || body["truncated"] != false {
		t.Errorf("body same/truncated = %v/%v, want false/false", body["same"], body["truncated"])
	}
	if body["size_a"] != float64(len(bodyA)) || body["size_b"] != float64(len(bodyB)) {
		t.Errorf("body sizes = %v/%v, want %d/%d", body["size_a"], body["size_b"], len(bodyA), len(bodyB))
	}
	if body["sha256_a"] != sha256Hex(string(bodyA)) || body["sha256_b"] != sha256Hex(string(bodyB)) {
		t.Errorf("body hashes = %v/%v, want independent sha256 values", body["sha256_a"], body["sha256_b"])
	}
	if _, present := body["unified_diff"]; present {
		t.Errorf("binary body must not carry a unified_diff: %v", body)
	}
}

func TestDiffIdenticalResponsesAreAllSame(t *testing.T) {
	env := newTestEnv(t)
	body := "stable\nbody\n"
	response := "HTTP/1.1 200 OK\r\n" +
		"X-Same: yes\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"Connection: close\r\n" +
		"\r\n" + body

	ids := captureDiffEntries(t, env, response)

	got := env.runWithStore(t, "diff", ids[0], ids[0])
	requireExit(t, got, 0)
	result := decodeJSONObject(t, got.stdout)
	status, _ := result["status"].(map[string]any)
	headers, _ := result["headers"].(map[string]any)
	bodyDiff, _ := result["body"].(map[string]any)
	if status["same"] != true {
		t.Errorf("status.same = %v, want true", status["same"])
	}
	if headers["same"] != true {
		t.Errorf("headers.same = %v, want true", headers["same"])
	}
	if bodyDiff["same"] != true || bodyDiff["truncated"] != false {
		t.Errorf("body same/truncated = %v/%v, want true/false", bodyDiff["same"], bodyDiff["truncated"])
	}
	if unified, _ := bodyDiff["unified_diff"].(string); unified != "" {
		t.Errorf("unified_diff for identical bodies = %q, want empty", unified)
	}
	if bodyDiff["type"] != "text" {
		t.Errorf("body.type = %v, want text", bodyDiff["type"])
	}
}

func TestDiffTruncatesOversizedBodies(t *testing.T) {
	env := newTestEnv(t)
	const bodyLimit = 1 << 20
	// Each line is 10 bytes; the truncation point splits a 3-byte rune.
	tail := strings.Repeat("\u20ac\u20ac\u20ac\n", bodyLimit/10+10)
	bodyA := "OLD first line\n" + tail
	bodyB := "NEW first line\n" + tail
	if len(bodyA) <= bodyLimit || len(bodyB) <= bodyLimit {
		t.Fatalf("fixture bodies must exceed %d bytes, got %d/%d", bodyLimit, len(bodyA), len(bodyB))
	}
	responseA := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyA)) +
		"Connection: close\r\n" +
		"\r\n" + bodyA
	responseB := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(bodyB)) +
		"Connection: close\r\n" +
		"\r\n" + bodyB

	ids := captureDiffEntries(t, env, responseA, responseB)

	got := env.runWithStore(t, "diff", ids[0], ids[1])
	requireExit(t, got, 0)
	result := decodeJSONObject(t, got.stdout)
	body, ok := result["body"].(map[string]any)
	if !ok {
		t.Fatalf("diff.body = %T, want an object", result["body"])
	}
	if body["truncated"] != true {
		t.Errorf("body.truncated = %v, want true for bodies over %d bytes", body["truncated"], bodyLimit)
	}
	if body["size_a"] != float64(len(bodyA)) || body["size_b"] != float64(len(bodyB)) {
		t.Errorf("body sizes = %v/%v, want full sizes %d/%d",
			body["size_a"], body["size_b"], len(bodyA), len(bodyB))
	}
	if body["same"] != false {
		t.Errorf("body.same = %v, want false", body["same"])
	}
	if body["type"] != "text" {
		t.Errorf("body.type = %v, want text; a split trailing rune must not force binary", body["type"])
	}
	unified, _ := body["unified_diff"].(string)
	if !strings.Contains(unified, "-OLD first line") || !strings.Contains(unified, "+NEW first line") {
		t.Errorf("unified diff does not show the real change:\n%s", unified)
	}
	if len(got.stdout) > 64<<10 {
		t.Errorf("diff output is %d bytes, want bounded by truncation (< 64 KiB)", len(got.stdout))
	}
}

func TestDiffMissingEntryReturnsEntryNotFound(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "diff", "missing-a", "missing-b")
	requireExit(t, got, 66)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "ENTRY_NOT_FOUND" {
		t.Errorf("error.code = %v, want ENTRY_NOT_FOUND", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	// One lookup serves both ids, so the error names every id that was
	// missing instead of stopping at the first one.
	ids, _ := details["ids"].([]any)
	if len(ids) != 2 || ids[0] != "missing-a" || ids[1] != "missing-b" {
		t.Errorf("error.details.ids = %v, want both missing ids in argument order", details["ids"])
	}
	message, _ := errObj["message"].(string)
	if !strings.Contains(message, "missing-a") || !strings.Contains(message, "missing-b") {
		t.Errorf("error.message = %q, want both missing ids", message)
	}
}

func TestDiffRejectsEntriesWithoutResponse(t *testing.T) {
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
	summary := decodeJSONObject(t, lines[0])
	id, _ := summary["id"].(string)

	got := env.runWithStore(t, "diff", id, id)
	requireExit(t, got, 2)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("error.code = %v, want INVALID_REQUEST", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	entries, _ := details["entries"].(map[string]any)
	side, _ := entries["a"].(map[string]any)
	if side["id"] != id || side["kind"] != "mitm_error" {
		t.Errorf("error.details.entries.a = %v, want the mitm_error entry", entries["a"])
	}
	if side["reason"] != "scope_denied" {
		t.Errorf("error.details.entries.a.reason = %v, want scope_denied", side["reason"])
	}
}

func TestDiffRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	diffCmd, ok := doc.Command.find("mitt diff")
	if !ok {
		t.Fatal("spec missing mitt diff")
	}
	if len(diffCmd.Args) != 2 ||
		diffCmd.Args[0].Name != "idA" || !diffCmd.Args[0].Required ||
		diffCmd.Args[1].Name != "idB" || !diffCmd.Args[1].Required {
		t.Errorf("mitt diff args = %+v, want required idA and idB", diffCmd.Args)
	}
	for _, code := range []string{"ENTRY_NOT_FOUND", "INVALID_REQUEST"} {
		if !containsString(diffCmd.ErrorCodes, code) {
			t.Errorf("mitt diff error_codes = %v, missing %s", diffCmd.ErrorCodes, code)
		}
	}
	if diffCmd.OutputSchema == nil {
		t.Fatal("mitt diff must register a success output schema")
	}
	if got := diffCmd.OutputSchema["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("diff output schema $schema = %v", got)
	}
	requireSchemaFields(t, diffCmd.OutputSchema, "a", "b", "status", "headers", "body")
	if got := doc.Limits["diff_body_max_bytes"]; got != 1<<20 {
		t.Errorf("spec limits diff_body_max_bytes = %v, want %d", got, 1<<20)
	}

	body := "before\n"
	idA, idB := captureDiffIDs(t, env, body, "after\n")
	got := env.runWithStore(t, "diff", idA, idB)
	requireExit(t, got, 0)
	requireSchemaValid(t, diffCmd.OutputSchema, decodeJSONObject(t, got.stdout), "diff output")
}

func captureDiffIDs(t *testing.T, env *testEnv, bodyA, bodyB string) (string, string) {
	t.Helper()
	response := func(body string) string {
		return "HTTP/1.1 200 OK\r\n" +
			fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
			"Connection: close\r\n" +
			"\r\n" + body
	}
	ids := captureDiffEntries(t, env, response(bodyA), response(bodyB))
	return ids[0], ids[1]
}

func TestDiffRendersHumanFormats(t *testing.T) {
	env := newTestEnv(t)
	idA, idB := captureDiffIDs(t, env, "before\n", "after\n")

	for _, format := range []string{"table", "yaml", "md"} {
		t.Run(format, func(t *testing.T) {
			got := env.runWithStore(t, "diff", idA, idB, "-f", format)
			requireExit(t, got, 0)
			for _, want := range []string{"status", "headers", "body"} {
				if !strings.Contains(got.stdout, want) {
					t.Errorf("%s output missing %q:\n%s", format, want, got.stdout)
				}
			}
		})
	}
}

func TestDiffClassifiesBinaryBodiesByUTF8AndNUL(t *testing.T) {
	env := newTestEnv(t)
	bodies := [][]byte{
		{0xff, 'a', '\n'},
		{0xfe, 'b', '\n'},
		[]byte("text\x00a"),
		[]byte("text\x00b"),
	}
	responses := make([]string, len(bodies))
	for i, body := range bodies {
		responses[i] = "HTTP/1.1 200 OK\r\n" +
			fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
			"Connection: close\r\n" +
			"\r\n" + string(body)
	}
	ids := captureDiffEntries(t, env, responses...)

	cases := []struct {
		name string
		a, b int
	}{
		{name: "invalid utf-8 without NUL", a: 0, b: 1},
		{name: "NUL with otherwise valid utf-8", a: 2, b: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := env.runWithStore(t, "diff", ids[tc.a], ids[tc.b])
			requireExit(t, got, 0)
			body, _ := decodeJSONObject(t, got.stdout)["body"].(map[string]any)
			if body["type"] != "binary" {
				t.Errorf("body.type = %v, want binary", body["type"])
			}
			if _, present := body["unified_diff"]; present {
				t.Errorf("binary body must not carry a unified_diff: %v", body)
			}
		})
	}
}
