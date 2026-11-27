//go:build linux

package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// recordingSink is the test adapter for the receipt seam. It keeps the status
// and body the failure policy asked for so the table itself can be asserted.
type recordingSink struct {
	calls  int
	status int
	body   string
}

func (s *recordingSink) reject(status int, body string) {
	s.calls++
	s.status = status
	s.body = body
}

func newTestCapture(t *testing.T) *Capture {
	t.Helper()
	return &Capture{
		store:  session.Open(t.TempDir()),
		stderr: io.Discard,
		events: newEventEmitter(nil),
	}
}

func readMITMErrors(t *testing.T, capture *Capture) []session.Event {
	t.Helper()
	events, err := capture.store.ReadHistory(nil)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	var failures []session.Event
	for _, event := range events {
		if event.Kind == session.KindMITMError {
			failures = append(failures, event)
		}
	}
	return failures
}

func TestFailureTableCoversEveryKind(t *testing.T) {
	reasons := map[string]bool{}
	for _, reason := range MITMReasons {
		reasons[reason] = true
	}
	for kind := failureKind(0); kind < failureKindCount; kind++ {
		row, ok := failureTable[kind]
		if !ok {
			t.Errorf("failure kind %d has no table row", kind)
			continue
		}
		if row.reason != "" && !reasons[row.reason] {
			t.Errorf("failure kind %d records reason %q, which is not in MITMReasons", kind, row.reason)
		}
		if row.status == 0 {
			if row.text != "" {
				t.Errorf("failure kind %d has a receipt text but no status", kind)
			}
			if row.appendError {
				t.Errorf("failure kind %d appends an error without a receipt", kind)
			}
			continue
		}
		if http.StatusText(row.status) == "" {
			t.Errorf("failure kind %d has unknown status %d", kind, row.status)
		}
		if row.text == "" {
			t.Errorf("failure kind %d has status %d but no receipt text", kind, row.status)
		}
	}
}

// TestFailureTableRecordsOnlyConnectFailureReasons pins the ADR-0003 split:
// failures before a tunnel exists are history events, while a failed exchange
// after the tunnel is up stays out of history.
func TestFailureTableRecordsOnlyConnectFailureReasons(t *testing.T) {
	exchangeFailures := []failureKind{
		failH2Upstream,
		failForwardRequest,
		failReadUpstreamResponse,
		failReadUpstreamBody,
		failUpstreamDial,
	}
	for _, kind := range exchangeFailures {
		if reason := failureTable[kind].reason; reason != "" {
			t.Errorf("exchange failure kind %d records reason %q, want a receipt only", kind, reason)
		}
	}
	connectFailures := []failureKind{
		failScopeDenied,
		failUpstreamConnect,
		failUpstreamTLSHandshake,
		failUpstreamNoCertificate,
		failUpstreamALPN,
		failUpstreamFingerprint,
		failLeafCertificate,
		failClientNotTLS,
		failClientCertRejected,
		failH2OnlyTarget,
		failUpstreamRedial,
		failUpstreamRedialFingerprint,
	}
	for _, kind := range connectFailures {
		if reason := failureTable[kind].reason; reason == "" {
			t.Errorf("connect failure kind %d records no reason", kind)
		}
	}
}

func TestFailureReceiptText(t *testing.T) {
	tests := []struct {
		kind    failureKind
		details map[string]any
		want    string
	}{
		{kind: failScopeDenied, want: "target is outside the configured scope"},
		{kind: failInvalidConnectTarget, details: map[string]any{"error": "empty CONNECT target"}, want: "invalid CONNECT target: empty CONNECT target"},
		{kind: failMalformedRequest, details: map[string]any{"error": "bad line"}, want: "malformed HTTP request: bad line"},
		{kind: failInvalidConnectTarget, want: "invalid CONNECT target"},
	}
	for _, test := range tests {
		row := failureTable[test.kind]
		if got := row.receipt(test.details); got != test.want {
			t.Errorf("%d receipt(%v) = %q, want %q", test.kind, test.details, got, test.want)
		}
	}
}

func TestRejectRecordsFailureAndWritesReceipt(t *testing.T) {
	capture := newTestCapture(t)
	sink := &recordingSink{}

	capture.reject(sink, "127.0.0.1:5555", "blocked.example",
		newFailure(failScopeDenied).withDetails(map[string]any{"matched": "deny", "pattern": "blocked.example"}))

	if sink.calls != 1 || sink.status != http.StatusForbidden {
		t.Fatalf("receipt = %d calls, status %d, want one 403", sink.calls, sink.status)
	}
	if sink.body != "target is outside the configured scope" {
		t.Errorf("receipt body = %q", sink.body)
	}

	failures := readMITMErrors(t, capture)
	if len(failures) != 1 {
		t.Fatalf("history has %d mitm_error events, want 1", len(failures))
	}
	event := failures[0]
	if event.Reason != MITMReasonScopeDenied || event.TargetHost != "blocked.example" || event.ClientAddr != "127.0.0.1:5555" {
		t.Errorf("failure event = reason %q host %q client %q", event.Reason, event.TargetHost, event.ClientAddr)
	}
	if !reflect.DeepEqual(event.Details, map[string]any{"matched": "deny", "pattern": "blocked.example"}) {
		t.Errorf("failure details = %v", event.Details)
	}
}

func TestRejectLeavesFailedExchangeOutOfHistory(t *testing.T) {
	capture := newTestCapture(t)
	sink := &recordingSink{}

	capture.reject(sink, "127.0.0.1:5555", "example.test", newFailure(failReadUpstreamResponse))

	if sink.calls != 1 || sink.status != http.StatusBadGateway {
		t.Fatalf("receipt = %d calls, status %d, want one 502", sink.calls, sink.status)
	}
	if failures := readMITMErrors(t, capture); len(failures) != 0 {
		t.Errorf("exchange failure wrote %d history events, want none", len(failures))
	}
}

// A failure after the 200 already went out cannot be reported over HTTP; the
// history event is the only trace the caller gets.
func TestH1SinkAfterConnectOnlyRecords(t *testing.T) {
	capture := newTestCapture(t)
	var buffer bytes.Buffer
	sink := h1ReceiptSink{writer: bufio.NewWriter(&buffer), afterConnect: true}

	capture.reject(sink, "127.0.0.1:5555", "example.test", newFailure(failClientCertRejected))

	if buffer.Len() != 0 {
		t.Errorf("post-handshake failure wrote %q, want no HTTP bytes", buffer.String())
	}
	failures := readMITMErrors(t, capture)
	if len(failures) != 1 || failures[0].Reason != MITMReasonClientCertRejected {
		t.Fatalf("history failures = %v, want one client_cert_rejected", failures)
	}
}

// The afterConnect guard must hold even for a row that does carry a status:
// once the client believes the tunnel is open, its bytes are TLS, so writing
// an HTTP response into that stream would corrupt it.
func TestH1SinkRefusesReceiptAfterConnect(t *testing.T) {
	var buffer bytes.Buffer
	sink := h1ReceiptSink{writer: bufio.NewWriter(&buffer), afterConnect: true}
	sink.reject(http.StatusBadGateway, "could not verify upstream TLS")
	if buffer.Len() != 0 {
		t.Errorf("sink wrote %q after CONNECT 200, want nothing", buffer.String())
	}
}

func TestH1SinkWritesStatusLineHeadersAndBody(t *testing.T) {
	var buffer bytes.Buffer
	sink := h1ReceiptSink{writer: bufio.NewWriter(&buffer)}
	sink.reject(http.StatusBadGateway, "could not forward request to upstream")

	want := "HTTP/1.1 502 Bad Gateway\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: 38\r\n" +
		"Connection: close\r\n\r\n" +
		"could not forward request to upstream\n"
	if buffer.String() != want {
		t.Errorf("receipt = %q, want %q", buffer.String(), want)
	}
}

func TestH2SinkWritesPlainTextError(t *testing.T) {
	recorder := &h2ResponseRecorder{header: http.Header{}}
	sink := h2ReceiptSink{writer: recorder}
	sink.reject(http.StatusForbidden, "target is outside the configured scope")

	if recorder.status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", recorder.status)
	}
	if recorder.body.String() != "target is outside the configured scope\n" {
		t.Errorf("body = %q", recorder.body.String())
	}
	if got := recorder.header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
}

// h2ResponseRecorder is a minimal http.ResponseWriter that keeps the parts of
// an http.Error response the failure policy cares about.
type h2ResponseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *h2ResponseRecorder) Header() http.Header { return r.header }

func (r *h2ResponseRecorder) WriteHeader(status int) { r.status = status }

func (r *h2ResponseRecorder) Write(p []byte) (int, error) { return r.body.Write(p) }

func TestH2OnlyEvidence(t *testing.T) {
	tests := []struct {
		name       string
		mode       ALPNMode
		negotiated string
		want       bool
	}{
		{name: "h1 mode with no ALPN result is worth probing", mode: ALPNModeH1, negotiated: "", want: true},
		{name: "h1 mode with confirmed HTTP/1.1 is not", mode: ALPNModeH1, negotiated: "http/1.1", want: false},
		{name: "h2-enabled mode with no ALPN result is not", mode: ALPNModeH2H1, negotiated: "", want: false},
		{name: "origin that selected h2 and refused the h1 redial is", mode: ALPNModeH2H1, negotiated: "h2", want: true},
	}
	for _, test := range tests {
		if got := h2OnlyEvidence(test.mode, test.negotiated); got != test.want {
			t.Errorf("%s: h2OnlyEvidence(%q, %q) = %v, want %v", test.name, test.mode, test.negotiated, got, test.want)
		}
	}
}

func TestClientHelloIsTLS(t *testing.T) {
	tests := []struct {
		name   string
		header []byte
		want   bool
	}{
		{name: "handshake record", header: []byte{0x16, 0x03, 0x01}, want: true},
		{name: "empty header is the caller's abort case", header: nil, want: true},
		{name: "plain HTTP", header: []byte("GET"), want: false},
		{name: "wrong record version", header: []byte{0x16, 0x02, 0x00}, want: false},
		{name: "future record version", header: []byte{0x16, 0x03, 0x05}, want: false},
	}
	for _, test := range tests {
		got, detail := clientHelloIsTLS(test.header)
		if got != test.want {
			t.Errorf("%s: clientHelloIsTLS(%v) = %v (%q), want %v", test.name, test.header, got, detail, test.want)
		}
		if !got && !strings.HasPrefix(detail, "first byte") && !strings.HasPrefix(detail, "TLS record version") {
			t.Errorf("%s: unexpected detail %q", test.name, detail)
		}
	}
}

func TestConnectAuthority(t *testing.T) {
	tests := []struct {
		target      string
		wantHost    string
		wantPort    string
		wantInvalid bool
	}{
		{target: "example.com:8443", wantHost: "example.com", wantPort: "8443"},
		{target: "example.com", wantHost: "example.com", wantPort: "443"},
		{target: "[::1]:443", wantHost: "::1", wantPort: "443"},
		{target: "[::1]", wantHost: "::1", wantPort: "443"},
		{target: "", wantInvalid: true},
		{target: "example.com/path", wantInvalid: true},
		{target: "user@example.com:443", wantInvalid: true},
		{target: "example.com:0", wantInvalid: true},
		{target: "example.com:99999", wantInvalid: true},
		{target: ":443", wantInvalid: true},
	}
	for _, test := range tests {
		host, port, err := connectAuthority(test.target)
		if test.wantInvalid {
			if err == nil {
				t.Errorf("connectAuthority(%q) = %q, %q, want an error", test.target, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("connectAuthority(%q): %v", test.target, err)
			continue
		}
		if host != test.wantHost || port != test.wantPort {
			t.Errorf("connectAuthority(%q) = %q, %q, want %q, %q", test.target, host, port, test.wantHost, test.wantPort)
		}
	}
}
