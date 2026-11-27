package replay_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/replay"
	"github.com/C1ph3rX13/mitt/internal/session"
)

func TestDryRunResolvesEachBase(t *testing.T) {
	store, entryID := newEntryStore(t)

	t.Run("entry", func(t *testing.T) {
		result, err := replay.DryRun(replay.SendRequest{
			Store: store,
			Base:  replay.EntryBase{ID: entryID},
		})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if result.Document.Method != "POST" || result.Document.URL != "http://example.test/base?q=1" {
			t.Errorf("document = %+v, want the entry request", result.Document)
		}
		if !strings.HasPrefix(string(result.Preview), "POST /base?q=1 HTTP/1.1\r\n") {
			t.Errorf("preview request line = %q", firstLine(result.Preview))
		}
		// A stored entry refers to its body by path; only a request rendered in
		// memory inlines bytes, so `show --detail` and `send --dry-run` describe
		// the same entry the same way.
		if result.Document.BodyFile == nil {
			t.Fatalf("document body_file = nil, want the stored body path")
		}
		body, err := result.Document.BodyBytes()
		if err != nil {
			t.Fatalf("resolve document body: %v", err)
		}
		if string(body) != "hello" {
			t.Errorf("document body bytes = %q, want hello", body)
		}
		if result.Document.Body != nil || result.Document.BodyBase64 != nil {
			t.Errorf("stored entry document inlined its body: %+v", result.Document)
		}
	})

	t.Run("url", func(t *testing.T) {
		result, err := replay.DryRun(replay.SendRequest{
			Base: replay.URLBase{URL: "http://example.test/path?x=1"},
		})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if result.Document.Method != "GET" || result.Document.URL != "http://example.test/path?x=1" {
			t.Errorf("document = %+v, want a constructed GET", result.Document)
		}
		if got := string(result.Preview); got != "GET /path?x=1 HTTP/1.1\r\nHost: example.test\r\n\r\n" {
			t.Errorf("preview = %q", got)
		}
	})

	t.Run("file", func(t *testing.T) {
		raw := "POST http://example.test/from-file HTTP/1.1\r\n" +
			"Host: example.test\r\n" +
			"Content-Length: 4\r\n\r\n" +
			"body"
		path := filepath.Join(t.TempDir(), "request.txt")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatalf("write request file: %v", err)
		}
		result, err := replay.DryRun(replay.SendRequest{
			Base: replay.FileBase{Path: path},
		})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if string(result.Preview) != raw {
			t.Errorf("preview = %q, want the file bytes verbatim", result.Preview)
		}
		if result.Document.Method != "POST" || result.Document.URL != "http://example.test/from-file" {
			t.Errorf("document = %+v, want the file request", result.Document)
		}
	})
}

func TestDryRunMissingBaseIsBaseConflict(t *testing.T) {
	_, err := replay.DryRun(replay.SendRequest{})
	assertCode(t, err, output.CodeBaseConflict)
}

func TestDryRunEmptyBasePayloadUsesExistingErrors(t *testing.T) {
	store, _ := newEntryStore(t)
	cases := []struct {
		name string
		base replay.Base
		code string
	}{
		{"entry", replay.EntryBase{ID: ""}, output.CodeEntryNotFound},
		{"url", replay.URLBase{URL: ""}, output.CodeInvalidRequest},
		{"file", replay.FileBase{Path: ""}, output.CodeInvalidRequest},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := replay.DryRun(replay.SendRequest{Store: store, Base: test.base})
			assertCode(t, err, test.code)
		})
	}
}

func TestDryRunPatchSources(t *testing.T) {
	base := replay.URLBase{URL: "http://example.test/patch"}
	t.Run("inline", func(t *testing.T) {
		result, err := replay.DryRun(replay.SendRequest{
			Base:  base,
			Patch: replay.InlinePatch{Text: `{"method":"PUT"}`},
		})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if result.Document.Method != "PUT" {
			t.Errorf("method = %q, want PUT", result.Document.Method)
		}
	})

	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "patch.json")
		if err := os.WriteFile(path, []byte(`{"method":"DELETE"}`), 0o600); err != nil {
			t.Fatalf("write patch file: %v", err)
		}
		result, err := replay.DryRun(replay.SendRequest{
			Base:  base,
			Patch: replay.FilePatch{Path: path},
		})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if result.Document.Method != "DELETE" {
			t.Errorf("method = %q, want DELETE", result.Document.Method)
		}
	})

	t.Run("stdin", func(t *testing.T) {
		result, err := replay.DryRun(replay.SendRequest{
			Base:  base,
			Patch: replay.StdinPatch{Reader: strings.NewReader(`{"method":"PATCH"}`)},
		})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if result.Document.Method != "PATCH" {
			t.Errorf("method = %q, want PATCH", result.Document.Method)
		}
	})

	t.Run("vars", func(t *testing.T) {
		result, err := replay.DryRun(replay.SendRequest{
			Base: base,
			Patch: replay.InlinePatch{
				Text: `{"headers":[["X-Token","{{TOKEN}}"]]}`,
				Vars: map[string]string{"TOKEN": "abc"},
			},
		})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if got := headerValue(result.Document.Headers, "X-Token"); got != "abc" {
			t.Errorf("X-Token = %q, want abc", got)
		}
	})
}

func TestDryRunEmptyInlinePatchIsInvalidPatch(t *testing.T) {
	_, err := replay.DryRun(replay.SendRequest{
		Base:  replay.URLBase{URL: "http://example.test/patch"},
		Patch: replay.InlinePatch{Text: ""},
	})
	assertCode(t, err, output.CodeInvalidPatch)
}

func TestPatchPayloadsRejectMissingInput(t *testing.T) {
	base := replay.URLBase{URL: "http://example.test/patch"}
	cases := []struct {
		name  string
		patch replay.Patch
	}{
		{"file", replay.FilePatch{Path: ""}},
		{"stdin", replay.StdinPatch{Reader: nil}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := replay.DryRun(replay.SendRequest{Base: base, Patch: test.patch})
			assertCode(t, err, output.CodeInvalidPatch)
		})
	}
}

// The recorded URL and the request-target written on the wire must come from
// the same value, so a URL the document exposes can always be replayed into
// the target the preview shows.
func TestDryRunDocumentURLAgreesWithTheRenderedTarget(t *testing.T) {
	cases := []struct {
		name       string
		rawURL     string
		wantURL    string
		wantTarget string
	}{
		{
			name:       "path and query",
			rawURL:     "http://example.test/a/b?q=1",
			wantURL:    "http://example.test/a/b?q=1",
			wantTarget: "/a/b?q=1",
		},
		{
			name:       "escaped path survives",
			rawURL:     "http://example.test/a%2Fb",
			wantURL:    "http://example.test/a%2Fb",
			wantTarget: "/a%2Fb",
		},
		{
			name:       "query without a path keeps its URL and gains the wire slash",
			rawURL:     "http://example.test?q=1",
			wantURL:    "http://example.test?q=1",
			wantTarget: "/?q=1",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := replay.DryRun(replay.SendRequest{
				Base: replay.URLBase{URL: testCase.rawURL},
			})
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if result.Document.URL != testCase.wantURL {
				t.Errorf("document url = %q, want %q", result.Document.URL, testCase.wantURL)
			}
			wantLine := "GET " + testCase.wantTarget + " HTTP/1.1"
			if got := firstLine(result.Preview); got != wantLine {
				t.Errorf("preview request line = %q, want %q", got, wantLine)
			}
		})
	}
}

func newEntryStore(t *testing.T) (*session.Store, string) {
	t.Helper()
	store := session.Open(t.TempDir())
	rec, err := store.BeginExchange(session.ExchangeSpec{
		Kind:       session.KindCaptured,
		Started:    time.Now(),
		ClientAddr: "127.0.0.1:1234",
	})
	if err != nil {
		t.Fatalf("begin record: %v", err)
	}
	defer rec.Abort()

	rec.IdentifyRequest(session.RequestIdentity{
		Method:               "POST",
		URL:                  "http://example.test/base?q=1",
		HTTPVersion:          "http/1.1",
		RequestLine:          "POST /base?q=1 HTTP/1.1",
		ForwardedRequestLine: "POST /base?q=1 HTTP/1.1",
	})
	event := rec.Event()
	event.RequestHeaders = [][2]string{
		{"Host", "example.test"},
		{"Content-Length", "5"},
	}
	body, err := rec.Request()
	if err != nil {
		t.Fatalf("request body: %v", err)
	}
	if _, err := body.Write([]byte("hello")); err != nil {
		t.Fatalf("write request body: %v", err)
	}
	completed := session.ResponseResult{Status: 200, Line: "HTTP/1.1 200 OK"}
	if err := rec.Commit(completed, nil, nil); err != nil {
		t.Fatalf("commit record: %v", err)
	}
	return store, event.ID
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", want)
	}
	var appErr *output.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v (%T), want *output.AppError", err, err)
	}
	if appErr.Code != want {
		t.Fatalf("code = %s, want %s (message: %s)", appErr.Code, want, appErr.Message)
	}
}

func firstLine(data []byte) string {
	line, _, _ := strings.Cut(string(data), "\r\n")
	return line
}

func headerValue(headers [][2]string, name string) string {
	for _, header := range headers {
		if strings.EqualFold(header[0], name) {
			return header[1]
		}
	}
	return ""
}
