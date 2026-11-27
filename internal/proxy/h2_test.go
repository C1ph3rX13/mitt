//go:build linux

package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestTailBufferKeepsLatestBytes(t *testing.T) {
	buffer := newTailBuffer(5)
	buffer.Write([]byte("abc"))
	buffer.Write([]byte("defg"))
	if got := string(buffer.Snapshot()); got != "cdefg" {
		t.Errorf("Snapshot() = %q, want cdefg", got)
	}
	buffer.Write([]byte("0123456789"))
	if got := string(buffer.Snapshot()); got != "56789" {
		t.Errorf("Snapshot() after oversized write = %q, want 56789", got)
	}
}

func TestH2RequestHeaderPairsPutsAuthorityFirstAndSorts(t *testing.T) {
	request := &http.Request{
		Host: "example.test:443",
		Header: http.Header{
			"X-Zeta":  []string{"last"},
			"X-Alpha": []string{"one", "two"},
		},
	}
	want := [][2]string{
		{"Host", "example.test:443"},
		{"X-Alpha", "one"},
		{"X-Alpha", "two"},
		{"X-Zeta", "last"},
	}
	if got := h2RequestHeaderPairs(request); !reflect.DeepEqual(got, want) {
		t.Errorf("h2RequestHeaderPairs() = %#v, want %#v", got, want)
	}
}

func TestH2ResponseLineUsesHTTP2Version(t *testing.T) {
	if got := h2ResponseLine(http.StatusOK); got != "HTTP/2.0 200 OK" {
		t.Errorf("h2ResponseLine(200) = %q, want HTTP/2.0 200 OK", got)
	}
	if got := h2ResponseLine(599); got != "HTTP/2.0 599 Unknown Status" {
		t.Errorf("h2ResponseLine(599) = %q, want the Unknown Status fallback", got)
	}
}

func TestH2OnlyHintDependsOnMode(t *testing.T) {
	if hint := h2OnlyHint(ALPNModeH1); hint == "" || !strings.Contains(hint, "--http2-alpn") {
		t.Errorf("h1 h2OnlyHint = %q, want a --http2-alpn hint", hint)
	}
	if hint := h2OnlyHint(ALPNModeH2H1); hint == "" || strings.Contains(hint, "restart") {
		t.Errorf("h2 h2OnlyHint = %q, want a client-side explanation", hint)
	}
}

// streamResetWriter mimics the h2 responseWriter after the client sent
// RST_STREAM: the headers are accepted, then every body byte and the flush
// report the reset. writeH2Response must surface that as a delivery failure
// instead of dropping it, because the record's eligibility depends on it.
type streamResetWriter struct {
	header http.Header
}

var errStreamReset = errors.New("http2: stream closed")

func (w *streamResetWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *streamResetWriter) Write([]byte) (int, error) { return 0, errStreamReset }

func (w *streamResetWriter) WriteHeader(int) {}

func (w *streamResetWriter) FlushError() error { return errStreamReset }

func TestWriteH2ResponseReportsAResetStream(t *testing.T) {
	response := &h2Response{
		status:         http.StatusOK,
		forwardHeaders: [][2]string{{"Content-Type", "text/plain"}},
	}
	err := writeH2Response(&streamResetWriter{}, http.MethodGet, response, strings.NewReader("hello"), 5)
	if !errors.Is(err, errStreamReset) {
		t.Fatalf("writeH2Response = %v, want the stream reset reported", err)
	}
}

// TestWriteH2ResponseFlushesAZeroByteResponse pins that a complete response with
// no body still runs the delivery step: END_STREAM is what tells the client the
// response is over, and a reset discovered there must not be mistaken for a
// successful delivery.
func TestWriteH2ResponseFlushesAZeroByteResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	response := &h2Response{status: http.StatusNoContent, forwardHeaders: [][2]string{{"X-Empty", "yes"}}}
	if err := writeH2Response(recorder, http.MethodGet, response, nil, 0); err != nil {
		t.Fatalf("writeH2Response = %v, want nil for a complete empty response", err)
	}
	if recorder.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", recorder.Code)
	}
	if !recorder.Flushed {
		t.Error("zero-byte response was not flushed; delivery cannot be observed")
	}
	if body, _ := io.ReadAll(recorder.Body); len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

// TestWriteH2ResponseSkipsTheBodyForHEAD pins the framing rule the h1 path also
// applies: the recorded body bytes are kept, but a HEAD response never sends
// them, and the flush still runs.
func TestWriteH2ResponseSkipsTheBodyForHEAD(t *testing.T) {
	recorder := httptest.NewRecorder()
	response := &h2Response{status: http.StatusOK, forwardHeaders: [][2]string{{"Content-Length", "5"}}}
	if err := writeH2Response(recorder, http.MethodHead, response, strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("writeH2Response = %v, want nil", err)
	}
	if body, _ := io.ReadAll(recorder.Body); len(body) != 0 {
		t.Errorf("HEAD body = %q, want none", body)
	}
	if !recorder.Flushed {
		t.Error("HEAD response was not flushed")
	}
}
