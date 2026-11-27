package session

import (
	"testing"
	"time"

	"github.com/C1ph3rX13/mitt/internal/httpwire"
)

func TestMessageIdentityRecordsReceivedAndForwardedLinesSeparately(t *testing.T) {
	received, err := httpwire.ParseRequest(
		"GET http://example.test/a?q=1 HTTP/1.1",
		[][2]string{{"Host", "example.test"}},
	)
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	origin, err := httpwire.NewOrigin("http", "example.test", "/a?q=1")
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	forwarded := received.Forwarded("GET /a?q=1 HTTP/1.1", received.Headers)

	identity := MessageIdentity(received, origin, forwarded.StartLine)
	if identity.Method != "GET" {
		t.Errorf("method = %q, want GET", identity.Method)
	}
	if identity.URL != "http://example.test/a?q=1" {
		t.Errorf("url = %q", identity.URL)
	}
	if identity.HTTPVersion != "http/1.1" {
		t.Errorf("http_version = %q, want http/1.1", identity.HTTPVersion)
	}
	if identity.RequestLine != "GET http://example.test/a?q=1 HTTP/1.1" {
		t.Errorf("request_line = %q, want the received absolute-form line", identity.RequestLine)
	}
	if identity.ForwardedRequestLine != "GET /a?q=1 HTTP/1.1" {
		t.Errorf("forwarded_request_line = %q, want the origin-form line", identity.ForwardedRequestLine)
	}
}

func TestMessageIdentityCarriesUnknownVersionsThrough(t *testing.T) {
	head, err := httpwire.ParseRequest("GET / HTTP/1.0", nil)
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	origin, err := httpwire.NewOrigin("https", "example.test", "/")
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	identity := MessageIdentity(head, origin, head.StartLine)
	if identity.HTTPVersion != "http/1.0" {
		t.Errorf("http_version = %q, want http/1.0", identity.HTTPVersion)
	}
}

func TestStreamIdentityNamesTheProducingAndForwardingProtocols(t *testing.T) {
	origin, err := httpwire.NewOrigin("https", "example.test:443", "/a?q=1")
	if err != nil {
		t.Fatalf("origin: %v", err)
	}

	h2Upstream := StreamIdentity("POST", origin, httpwire.HTTP2)
	if h2Upstream.HTTPVersion != "h2" {
		t.Errorf("http_version = %q, want h2 (the client-facing protocol)", h2Upstream.HTTPVersion)
	}
	if h2Upstream.RequestLine != "POST /a?q=1 HTTP/2.0" {
		t.Errorf("request_line = %q, want the synthesized HTTP/2.0 line", h2Upstream.RequestLine)
	}
	if h2Upstream.ForwardedRequestLine != "POST /a?q=1 HTTP/2.0" {
		t.Errorf("forwarded_request_line = %q, want HTTP/2.0 for an h2 upstream", h2Upstream.ForwardedRequestLine)
	}

	bridged := StreamIdentity("POST", origin, httpwire.HTTP11)
	if bridged.HTTPVersion != "h2" {
		t.Errorf("bridged http_version = %q, want h2", bridged.HTTPVersion)
	}
	// The h1 bridge writes an HTTP/1.1 line; a record claiming HTTP/2.0 here
	// would be the drift this identity exists to prevent.
	if bridged.ForwardedRequestLine != "POST /a?q=1 HTTP/1.1" {
		t.Errorf("bridged forwarded_request_line = %q, want the HTTP/1.1 line the origin receives", bridged.ForwardedRequestLine)
	}
	if bridged.URL != origin.URL() {
		t.Errorf("bridged url = %q, want %q", bridged.URL, origin.URL())
	}
}

func TestIdentifyRequestWritesEveryIdentityField(t *testing.T) {
	exchange := newExchange(t, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer exchange.Abort()

	exchange.IdentifyRequest(RequestIdentity{
		Method:               "PUT",
		URL:                  "https://example.test/a",
		HTTPVersion:          "h2",
		RequestLine:          "PUT /a HTTP/2.0",
		ForwardedRequestLine: "PUT /a HTTP/1.1",
		UpstreamCertSHA256:   "abc",
	})

	event := exchange.Event()
	if event.Method != "PUT" || event.URL != "https://example.test/a" || event.HTTPVersion != "h2" {
		t.Errorf("identity = %q/%q/%q", event.Method, event.URL, event.HTTPVersion)
	}
	if event.RequestLine != "PUT /a HTTP/2.0" || event.ForwardedRequestLine != "PUT /a HTTP/1.1" {
		t.Errorf("lines = %q/%q", event.RequestLine, event.ForwardedRequestLine)
	}
	if event.UpstreamCertSHA256 != "abc" {
		t.Errorf("upstream_cert_sha256 = %q, want abc", event.UpstreamCertSHA256)
	}
}

// A record with no upstream identity must not inherit one from a previous
// identity application: the certificate field belongs to the tunnel, not the
// request, and an empty value means this exchange has none.
func TestIdentifyRequestWithoutUpstreamIdentityLeavesItEmpty(t *testing.T) {
	exchange := newExchange(t, ExchangeSpec{Kind: KindSent})
	defer exchange.Abort()

	exchange.IdentifyRequest(RequestIdentity{Method: "GET", URL: "http://example.test/", HTTPVersion: "http/1.1"})
	if event := exchange.Event(); event.UpstreamCertSHA256 != "" {
		t.Errorf("upstream_cert_sha256 = %q, want empty", event.UpstreamCertSHA256)
	}
}

func TestCommitMeasuresTotalFromTheRecordStart(t *testing.T) {
	started := time.Now().Add(-5 * time.Second)
	exchange := newExchange(t, ExchangeSpec{Kind: KindSent, Started: started})
	defer exchange.Abort()

	exchange.Event().Timings = &Timings{}
	if err := commitCompleted(exchange); err != nil {
		t.Fatalf("commit: %v", err)
	}
	timings := exchange.Event().Timings
	if timings == nil {
		t.Fatal("timings missing after commit")
	}
	if timings.TotalMS < 4000 {
		t.Errorf("total_ms = %d, want at least the 5s the record has been open", timings.TotalMS)
	}
}
