package replay_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/replay"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// TestEmptyPatchIsEmpty pins the property the two rendering strategies broke:
// applying a no-op patch must not change the bytes a request is sent as. The
// case that used to differ is a chunked base with an empty body, where the
// unpatched path recomputed Content-Length: 0 while the patched path kept
// Transfer-Encoding: chunked.
func TestEmptyPatchIsEmpty(t *testing.T) {
	store, chunkedID := newChunkedEmptyBodyStore(t)
	lengthStore, lengthID := newEntryStore(t)

	cases := []struct {
		name  string
		store *session.Store
		base  replay.Base
	}{
		{"entry with chunked empty body", store, replay.EntryBase{ID: chunkedID}},
		{"entry with a body length", lengthStore, replay.EntryBase{ID: lengthID}},
		{"url base", store, replay.URLBase{URL: "http://example.test/patch?x=1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			without, err := replay.DryRun(replay.SendRequest{Store: tc.store, Base: tc.base})
			if err != nil {
				t.Fatalf("dry run without patch: %v", err)
			}
			with, err := replay.DryRun(replay.SendRequest{
				Store: tc.store,
				Base:  tc.base,
				Patch: replay.InlinePatch{Text: `{}`},
			})
			if err != nil {
				t.Fatalf("dry run with an empty patch: %v", err)
			}
			if string(with.Preview) != string(without.Preview) {
				t.Errorf("empty patch changed the bytes:\n without: %q\n with:    %q", without.Preview, with.Preview)
			}
			if with.RequestTotalBytes != without.RequestTotalBytes {
				t.Errorf("empty patch changed the size: without %d, with %d", without.RequestTotalBytes, with.RequestTotalBytes)
			}
		})
	}
}

// TestChunkedEmptyBodyReplayDeclaresALength pins the framing the one policy
// picks for a chunked base whose body decodes to nothing: the head carries an
// explicit Content-Length: 0 and no Transfer-Encoding, so the target cannot
// read a body that was not sent.
func TestChunkedEmptyBodyReplayDeclaresALength(t *testing.T) {
	store, id := newChunkedEmptyBodyStore(t)
	result, err := replay.DryRun(replay.SendRequest{Store: store, Base: replay.EntryBase{ID: id}})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	head, body, _ := strings.Cut(string(result.Preview), "\r\n\r\n")
	if strings.Contains(strings.ToLower(head), "transfer-encoding") {
		t.Errorf("head kept Transfer-Encoding:\n%s", head)
	}
	if !strings.Contains(head, "\r\nContent-Length: 0") {
		t.Errorf("head has no Content-Length: 0:\n%s", head)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

// TestDryRunRunsTheSamePreconditionsAsSend pins that a preview cannot succeed
// where a send of the same request would fail on its inputs. The TLS profile
// used to be validated only by Send.
func TestDryRunRunsTheSamePreconditionsAsSend(t *testing.T) {
	for _, profile := range []string{"safari", "Chrome"} {
		t.Run(profile, func(t *testing.T) {
			_, err := replay.DryRun(replay.SendRequest{
				Base:       replay.URLBase{URL: "https://example.test/"},
				TLSProfile: profile,
			})
			assertCode(t, err, output.CodeInvalidRequest)
		})
	}

	result, err := replay.DryRun(replay.SendRequest{
		Base:       replay.URLBase{URL: "https://example.test/"},
		TLSProfile: "chrome",
	})
	if err != nil {
		t.Fatalf("dry run with a supported profile: %v", err)
	}
	if string(result.Preview) == "" {
		t.Error("dry run produced no preview")
	}
}

// newChunkedEmptyBodyStore writes one captured entry whose request declares
// chunked framing and whose decoded body is empty — the shape the two old
// rendering strategies disagreed about.
func newChunkedEmptyBodyStore(t *testing.T) (*session.Store, string) {
	t.Helper()
	store := session.Open(t.TempDir())
	rec, err := store.BeginExchange(session.ExchangeSpec{
		Kind:       session.KindCaptured,
		Started:    time.Now(),
		ClientAddr: "127.0.0.1:4321",
	})
	if err != nil {
		t.Fatalf("begin record: %v", err)
	}
	defer rec.Abort()

	rec.IdentifyRequest(session.RequestIdentity{
		Method:               "POST",
		URL:                  "http://example.test/empty",
		HTTPVersion:          "http/1.1",
		RequestLine:          "POST /empty HTTP/1.1",
		ForwardedRequestLine: "POST /empty HTTP/1.1",
	})
	event := rec.Event()
	event.RequestHeaders = [][2]string{
		{"Host", "example.test"},
		{"Transfer-Encoding", "chunked"},
		{"Connection", "close"},
	}
	body, err := rec.Request()
	if err != nil {
		t.Fatalf("request body: %v", err)
	}
	// Zero bytes: the chunked framing decodes to an empty entity.
	if _, err := body.Write(nil); err != nil {
		t.Fatalf("write empty request body: %v", err)
	}
	completed := session.ResponseResult{Status: 200, Line: "HTTP/1.1 200 OK"}
	if err := rec.Commit(completed, nil, nil); err != nil {
		t.Fatalf("commit record: %v", err)
	}
	if info, err := os.Stat(filepath.Join(store.Dir(), "bodies", event.ID+"-req.bin")); err == nil && info.Size() != 0 {
		t.Fatalf("fixture body file = %d bytes, want 0", info.Size())
	}
	return store, event.ID
}
