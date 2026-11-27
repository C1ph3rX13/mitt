package replay

import (
	"net/url"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/httpwire"
)

// TestMaterializeIsTheOnlyFramingPolicy pins the single rule every rendered
// request obeys: framing is derived from the final body and the final headers,
// never from which base produced the document or whether a patch ran.
func TestMaterializeIsTheOnlyFramingPolicy(t *testing.T) {
	target, err := url.Parse("http://example.test/frame?q=1")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	cases := []struct {
		name         string
		headers      [][2]string
		body         []byte
		hostExplicit bool
		wantHead     string
		wantBody     string
	}{
		{
			name:     "bodyless request declares nothing",
			headers:  [][2]string{{"X-A", "1"}},
			wantHead: "GET /frame?q=1 HTTP/1.1\r\nX-A: 1\r\nHost: example.test\r\n\r\n",
		},
		{
			name:     "declared length survives an empty body",
			headers:  [][2]string{{"Content-Length", "5"}},
			wantHead: "GET /frame?q=1 HTTP/1.1\r\nContent-Length: 0\r\nHost: example.test\r\n\r\n",
		},
		{
			name:     "chunked framing becomes a length even for an empty body",
			headers:  [][2]string{{"Transfer-Encoding", "chunked"}},
			wantHead: "GET /frame?q=1 HTTP/1.1\r\nContent-Length: 0\r\nHost: example.test\r\n\r\n",
		},
		{
			name:     "chunked framing becomes a length for a real body",
			headers:  [][2]string{{"Transfer-Encoding", "chunked"}},
			body:     []byte("hello"),
			wantHead: "GET /frame?q=1 HTTP/1.1\r\nContent-Length: 5\r\nHost: example.test\r\n\r\n",
			wantBody: "hello",
		},
		{
			name:     "a body without framing gains a length",
			headers:  [][2]string{{"X-A", "1"}},
			body:     []byte("hi"),
			wantHead: "GET /frame?q=1 HTTP/1.1\r\nX-A: 1\r\nContent-Length: 2\r\nHost: example.test\r\n\r\n",
			wantBody: "hi",
		},
		{
			name:     "the length takes the transfer-encoding position",
			headers:  [][2]string{{"X-A", "1"}, {"Transfer-Encoding", "chunked"}, {"X-B", "2"}},
			body:     []byte("hello"),
			wantHead: "GET /frame?q=1 HTTP/1.1\r\nX-A: 1\r\nContent-Length: 5\r\nX-B: 2\r\nHost: example.test\r\n\r\n",
			wantBody: "hello",
		},
		{
			name:     "duplicate lengths collapse into one",
			headers:  [][2]string{{"Content-Length", "3"}, {"Content-Length", "3"}},
			body:     []byte("abc"),
			wantHead: "GET /frame?q=1 HTTP/1.1\r\nContent-Length: 3\r\nHost: example.test\r\n\r\n",
			wantBody: "abc",
		},
		{
			name:         "an explicit host is kept",
			headers:      [][2]string{{"Host", "custom.example"}},
			hostExplicit: true,
			wantHead:     "GET /frame?q=1 HTTP/1.1\r\nHost: custom.example\r\n\r\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rendered, err := finalRequest{
				method:       "GET",
				target:       target,
				headers:      tc.headers,
				body:         tc.body,
				hostExplicit: tc.hostExplicit,
			}.materialize()
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}
			want := tc.wantHead + tc.wantBody
			if got := string(rendered.raw); got != want {
				t.Errorf("bytes = %q, want %q", got, want)
			}
			if len(rendered.raw) != len(want) {
				t.Errorf("raw length = %d, want %d", len(rendered.raw), len(want))
			}
			// The head must describe the bytes that carry it, so the framing
			// rule a reader derives from the head matches the emitted body.
			if got := httpwire.Values(rendered.head.Headers, "Transfer-Encoding"); len(got) != 0 {
				t.Errorf("rendered head kept Transfer-Encoding %v", got)
			}
		})
	}
}

// TestMaterializeRejectsInvalidMethod proves the single rendering point is
// also the single validation point for the request line it writes.
func TestMaterializeRejectsInvalidMethod(t *testing.T) {
	target, err := url.Parse("http://example.test/")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	if _, err := (finalRequest{method: "BAD METHOD", target: target}).materialize(); err == nil {
		t.Fatal("materialize accepted a method with a space")
	}
}
