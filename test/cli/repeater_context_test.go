package cli_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRepeaterContextIsCaseInsensitiveAndKeepsFirstNonEmptyValue(t *testing.T) {
	env := newTestEnv(t)

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	rawRequest := "GET http://" + targetAddr + "/tagged HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"x-repeater-context:   \r\n" +
		"X-REPEATER-CONTEXT: flow-primary\r\n" +
		"X-Repeater-Context: flow-override\r\n" +
		"X-Keep: yes\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	if response := sendRaw(t, proxyAddr, rawRequest); response != targetResponse {
		t.Fatalf("client response = %q, want %q", response, targetResponse)
	}

	observed := waitObserved(t, requests)
	wantHeaders := [][2]string{
		{"Host", targetAddr},
		{"X-Keep", "yes"},
		{"Connection", "close"},
	}
	if !reflect.DeepEqual(observed.headers, wantHeaders) {
		t.Errorf("target headers = %#v, want every context field stripped in place", observed.headers)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	history := env.runWithStore(t, "history", "--context", "flow-primary")
	requireExit(t, history, 0)
	lines := historyLines(history.stdout)
	if len(lines) != 1 {
		t.Fatalf("history --context flow-primary returned %d lines, want 1:\n%s", len(lines), history.stdout)
	}
	summary := decodeJSONObject(t, lines[0])
	if summary["context"] != "flow-primary" {
		t.Errorf("history context = %v, want the first non-empty value flow-primary", summary["context"])
	}
	id, _ := summary["id"].(string)
	if id == "" {
		t.Fatalf("history summary has no id: %v", summary)
	}

	override := env.runWithStore(t, "history", "--context", "flow-override")
	requireExit(t, override, 66)
	if code := errorEnvelope(t, override)["code"]; code != "NO_MATCH" {
		t.Errorf("later duplicate context code = %v, want NO_MATCH", code)
	}

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	if strings.Contains(strings.ToLower(string(rawHistory)), "x-repeater-context") {
		t.Errorf("history.jsonl must not retain the context header:\n%s", rawHistory)
	}
	event := decodeJSONObject(t, string(rawHistory))
	if event["context"] != "flow-primary" {
		t.Errorf("event context = %v, want flow-primary", event["context"])
	}
	requireHeaderPairs(t, event["request_headers"], wantHeaders)

	raw := env.runWithStore(t, "show", id, "--raw")
	requireExit(t, raw, 0)
	wantRaw := "GET /tagged HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"X-Keep: yes\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		targetResponse
	if raw.stdout != wantRaw {
		t.Fatalf("show --raw bytes differ:\n got %q\nwant %q", raw.stdout, wantRaw)
	}
	if strings.Contains(strings.ToLower(raw.stdout), "x-repeater-context") {
		t.Error("show --raw must not reintroduce X-Repeater-Context")
	}

	detail := env.runWithStore(t, "show", id, "--detail")
	requireExit(t, detail, 0)
	show := decodeJSONObject(t, detail.stdout)
	document, ok := show["request_document"].(map[string]any)
	if !ok {
		t.Fatalf("request_document = %T, want an object", show["request_document"])
	}
	requireHeaderPairs(t, document["headers"], wantHeaders)
}

func TestHistoryContextFilterIsExactAndIsolatesEntries(t *testing.T) {
	env := newTestEnv(t)

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	requestsToSend := []struct {
		path    string
		context string
	}{
		{path: "/a", context: "X-Repeater-Context: flow-a\r\n"},
		{path: "/b", context: "X-Repeater-Context: flow-b\r\n"},
		{path: "/none"},
		{path: "/empty", context: "X-Repeater-Context:   \r\n"},
	}
	for _, item := range requestsToSend {
		rawRequest := "GET http://" + targetAddr + item.path + " HTTP/1.1\r\n" +
			"Host: " + targetAddr + "\r\n" +
			item.context +
			"Connection: close\r\n" +
			"\r\n"
		if response := sendRaw(t, proxyAddr, rawRequest); response != targetResponse {
			t.Fatalf("client response for %s = %q, want %q", item.path, response, targetResponse)
		}
		waitObserved(t, requests)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	all := env.runWithStore(t, "history")
	requireExit(t, all, 0)
	lines := historyLines(all.stdout)
	if len(lines) != len(requestsToSend) {
		t.Fatalf("history returned %d lines, want %d:\n%s", len(lines), len(requestsToSend), all.stdout)
	}
	contexts := map[string]int{}
	urlByContext := map[string]string{}
	for _, line := range lines {
		summary := decodeJSONObject(t, line)
		context, _ := summary["context"].(string)
		contexts[context]++
		urlByContext[context], _ = summary["url"].(string)
	}
	if contexts["flow-a"] != 1 || contexts["flow-b"] != 1 || contexts[""] != 2 {
		t.Errorf("history contexts = %v, want one flow-a, one flow-b, and two without a marker", contexts)
	}
	if !strings.HasSuffix(urlByContext["flow-a"], "/a") {
		t.Errorf("flow-a url = %q, want the /a request", urlByContext["flow-a"])
	}
	if !strings.HasSuffix(urlByContext["flow-b"], "/b") {
		t.Errorf("flow-b url = %q, want the /b request", urlByContext["flow-b"])
	}

	for _, want := range []struct {
		context string
		path    string
	}{
		{context: "flow-a", path: "/a"},
		{context: "flow-b", path: "/b"},
	} {
		got := env.runWithStore(t, "history", "--context", want.context)
		requireExit(t, got, 0)
		matched := historyLines(got.stdout)
		if len(matched) != 1 {
			t.Fatalf("history --context %s returned %d lines, want 1:\n%s", want.context, len(matched), got.stdout)
		}
		summary := decodeJSONObject(t, matched[0])
		matchedURL, _ := summary["url"].(string)
		if !strings.HasSuffix(matchedURL, want.path) {
			t.Errorf("history --context %s returned url %v, want %s", want.context, summary["url"], want.path)
		}
	}

	for _, wrong := range []string{"Flow-A", "flow", "flow-a ", "empty"} {
		got := env.runWithStore(t, "history", "--context", wrong)
		requireExit(t, got, 66)
		if code := errorEnvelope(t, got)["code"]; code != "NO_MATCH" {
			t.Errorf("history --context %q code = %v, want NO_MATCH", wrong, code)
		}
	}
}
