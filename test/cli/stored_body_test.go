package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// captureBodies records one captured entry whose request and response bodies
// are both non-empty, so either direction can be damaged afterwards.
func captureBodies(t *testing.T, env *testEnv) string {
	t.Helper()
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	rawRequest := "POST http://{{target}}/body HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello"
	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)
	return id
}

// TestStoredBodyDamageIsOneAnswerForEveryCommand pins the report's acceptance
// case at the command boundary. A record that claims bytes, whose file is gone,
// must not read as an empty body in any command that needs those bytes: each
// reports the same INTERNAL_ERROR naming the entry, direction, and path.
// `show --detail` still describes the record, because a projection does not need
// the bytes.
func TestStoredBodyDamageIsOneAnswerForEveryCommand(t *testing.T) {
	cases := []struct {
		name     string
		suffix   string
		label    string
		commands func(t *testing.T, env *testEnv, id string) map[string]result
	}{
		{
			name:   "request body",
			suffix: session.BodyRequest,
			label:  "request",
			commands: func(t *testing.T, env *testEnv, id string) map[string]result {
				return map[string]result{
					"show --raw": env.runWithStore(t, "show", id, "--raw"),
					"send <id>":  env.runWithStore(t, "send", id),
					"dry-run":    env.runWithStore(t, "send", id, "--dry-run"),
				}
			},
		},
		{
			name:   "response body",
			suffix: session.BodyResponse,
			label:  "response",
			commands: func(t *testing.T, env *testEnv, id string) map[string]result {
				return map[string]result{
					"show --raw": env.runWithStore(t, "show", id, "--raw"),
					"diff":       env.runWithStore(t, "diff", id, id),
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			id := captureBodies(t, env)
			path := filepath.Join(env.store, "bodies", id+"-"+tc.suffix+".bin")
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove stored %s body: %v", tc.label, err)
			}

			// A listing and the detail projection stay metadata-only: the record
			// exists and is describable even when a body cannot be delivered.
			history := env.runWithStore(t, "history")
			requireExit(t, history, 0)
			detail := env.runWithStore(t, "show", id, "--detail")
			requireExit(t, detail, 0)
			detailObject := decodeJSONObject(t, detail.stdout)
			if detailObject["request_body_path"] != filepath.Join(env.store, "bodies", id+"-req.bin") {
				t.Errorf("request_body_path = %v, want the recorded path", detailObject["request_body_path"])
			}
			if detailObject["response_body_path"] != filepath.Join(env.store, "bodies", id+"-resp.bin") {
				t.Errorf("response_body_path = %v, want the recorded path", detailObject["response_body_path"])
			}
			if document, ok := detailObject["request_document"].(map[string]any); ok && tc.label == "request" {
				// Damage must not be papered over by dropping the reference: the
				// document still says which file the record's bytes belong in.
				if document["body_file"] != path {
					t.Errorf("request_document.body_file = %v, want the record's reference %s", document["body_file"], path)
				}
				if _, inlined := document["body"]; inlined {
					t.Errorf("damaged stored body was silently inlined: %v", document)
				}
			}

			for name, got := range tc.commands(t, env, id) {
				requireExit(t, got, 1)
				errObj := errorEnvelope(t, got)
				if errObj["code"] != "INTERNAL_ERROR" {
					t.Errorf("%s code = %v, want INTERNAL_ERROR", name, errObj["code"])
				}
				details, _ := errObj["details"].(map[string]any)
				if details["id"] != id {
					t.Errorf("%s details.id = %v, want %s", name, details["id"], id)
				}
				if details["direction"] != tc.label {
					t.Errorf("%s details.direction = %v, want %s", name, details["direction"], tc.label)
				}
				if details["path"] != path {
					t.Errorf("%s details.path = %v, want %s", name, details["path"], path)
				}
				if details["store"] != env.store {
					t.Errorf("%s details.store = %v, want %s", name, details["store"], env.store)
				}
			}
		})
	}
}

// TestEmptyBodyEntryReplaysWithoutABodyFile pins the other side of the same
// rule end to end: an entry whose request body is genuinely empty still shows,
// previews, and replays, even after its zero-length body file is gone.
func TestEmptyBodyEntryReplaysWithoutABodyFile(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"
	rawRequest := "GET http://{{target}}/empty HTTP/1.1\r\n" +
		"Host: {{target}}\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)

	requestPath := filepath.Join(env.store, "bodies", id+"-req.bin")
	if info, err := os.Stat(requestPath); err != nil || info.Size() != 0 {
		t.Fatalf("request body file = %v, %v; want a zero-length file", info, err)
	}

	// Removing the file of a body the record says is empty changes nothing:
	// both forms mean "no bytes".
	if err := os.Remove(requestPath); err != nil {
		t.Fatalf("remove empty body file: %v", err)
	}
	requireExit(t, env.runWithStore(t, "show", id), 0)
	requireExit(t, env.runWithStore(t, "show", id, "--raw"), 0)
	requireExit(t, env.runWithStore(t, "send", id, "--dry-run"), 0)
}
