//go:build linux

package proxy

import (
	"bufio"
	"net/http"
)

// failureKind names one way a capture attempt can fail. Every kind has exactly
// one row in failureTable; that row is the only place its recorded reason,
// client receipt, and record policy are written down.
type failureKind int

const (
	// CONNECT preconditions.
	failInvalidConnectTarget failureKind = iota
	failScopeDenied

	// Upstream TLS establishment.
	failUpstreamConnect
	failUpstreamTLSHandshake
	failUpstreamNoCertificate
	failUpstreamALPN
	failUpstreamFingerprint
	failUpstreamRedial
	failUpstreamRedialFingerprint

	// Leaf issuance and the client handshake.
	failLeafCertificate
	failClientNotTLS
	failClientCertRejected

	// Tunneled or forwarded exchanges.
	failH2OnlyTarget
	failMalformedRequest
	failNestedConnect
	failNonAbsoluteTarget
	failHTTPSAbsoluteTarget
	failUnsupportedTarget
	failOriginFormRequired
	failCreateEntry
	failStoreRequestBody
	failReadRequestBody
	failUpstreamDial
	failForwardRequest
	failReadUpstreamResponse
	failStoreResponseBody
	failReadUpstreamBody
	failReadStoredBody
	// failH2Upstream covers every failure of an HTTP/2 stream's upstream leg,
	// where the transport reports one error for connect, write, and read.
	failH2Upstream

	// Connection-level rejections.
	failTunnelAuthorityMismatch
	failShuttingDown

	// failureKindCount is the sentinel after the last real kind, so tests can
	// prove every kind has a row.
	failureKindCount
)

// failureRow is the policy for one failure kind. An empty reason means the
// failure never becomes a history event: exchange-level upstream failures stay
// out of history per ADR-0003. A zero status means no HTTP receipt is possible
// because the client already owns a TLS stream, so only the connection closes.
type failureRow struct {
	reason      string
	status      int
	text        string
	appendError bool
}

var failureTable = map[failureKind]failureRow{
	failInvalidConnectTarget:      {status: 400, text: "invalid CONNECT target", appendError: true},
	failScopeDenied:               {reason: MITMReasonScopeDenied, status: 403, text: "target is outside the configured scope"},
	failUpstreamConnect:           {reason: MITMReasonUpstreamConnect, status: 502, text: "could not connect to upstream"},
	failUpstreamTLSHandshake:      {reason: MITMReasonUpstreamTLS, status: 502, text: "could not verify upstream TLS"},
	failUpstreamNoCertificate:     {reason: MITMReasonUpstreamTLS, status: 502, text: "upstream presented no certificate"},
	failUpstreamALPN:              {reason: MITMReasonUpstreamTLS, status: 502, text: "unsupported upstream protocol"},
	failUpstreamFingerprint:       {reason: MITMReasonUpstreamTLS, status: 502, text: "could not fingerprint the upstream certificate"},
	failUpstreamRedial:            {reason: MITMReasonUpstreamTLS},
	failUpstreamRedialFingerprint: {reason: MITMReasonUpstreamTLS},
	failLeafCertificate:           {reason: MITMReasonLeafCertificate, status: 500, text: "could not issue a leaf certificate"},
	failClientNotTLS:              {reason: MITMReasonNotTLS},
	failClientCertRejected:        {reason: MITMReasonClientCertRejected},
	failH2OnlyTarget:              {reason: MITMReasonH2OnlyTarget, status: 502, text: "target only supports h2"},
	failMalformedRequest:          {status: 400, text: "malformed HTTP request", appendError: true},
	failNestedConnect:             {status: 400, text: "nested CONNECT is not supported"},
	failNonAbsoluteTarget:         {status: 400, text: "mitt only forwards absolute-form http:// requests"},
	failHTTPSAbsoluteTarget:       {status: 400, text: "https:// requests must use CONNECT; absolute-form https:// is rejected"},
	failUnsupportedTarget:         {status: 400, text: "mitt does not forward this request target"},
	failOriginFormRequired:        {status: 400, text: "CONNECT tunnels only accept origin-form request targets"},
	failCreateEntry:               {status: 500, text: "could not create history entry"},
	failStoreRequestBody:          {status: 500, text: "could not store request body"},
	failReadRequestBody:           {status: 400, text: "could not read request body", appendError: true},
	failUpstreamDial:              {status: 502, text: "could not connect to upstream"},
	failForwardRequest:            {status: 502, text: "could not forward request to upstream"},
	failReadUpstreamResponse:      {status: 502, text: "could not read upstream response"},
	failStoreResponseBody:         {status: 500, text: "could not store response body"},
	failReadUpstreamBody:          {status: 502, text: "could not read upstream response body"},
	failReadStoredBody:            {status: 500, text: "could not read stored response body"},
	failH2Upstream:                {status: 502, text: "could not forward h2 stream upstream"},
	failTunnelAuthorityMismatch:   {status: 400, text: "requests inside a CONNECT tunnel must use the tunnel authority"},
	failShuttingDown:              {status: 503, text: "proxy is shutting down"},
}

// failure is one failure in flight: the table row that decides what to do plus
// the instance evidence that goes into a recorded event.
type failure struct {
	kind    failureKind
	details map[string]any
}

func newFailure(kind failureKind) failure {
	if _, ok := failureTable[kind]; !ok {
		panic("proxy: failure kind has no table row")
	}
	return failure{kind: kind}
}

func (f failure) withError(err error) failure {
	f.details = map[string]any{"error": err.Error()}
	return f
}

func (f failure) withDetails(details map[string]any) failure {
	f.details = details
	return f
}

// is reports whether this failure is of kind. Callers use it where the failure
// kind, not just the row, decides the diagnostic they emit.
func (f failure) is(kind failureKind) bool { return f.kind == kind }

// receiptSink is where a failure receipt goes. The two adapters are the raw
// HTTP/1.1 writer and the HTTP/2 stream writer; they are the only difference
// between reporting a failure before or inside a tunnel.
type receiptSink interface {
	reject(status int, body string)
}

// reject applies one failure. It is the only place a capture failure becomes a
// mitm_error history event or a client receipt, so failureTable is the whole
// policy: a new failure is one row, not a warn/record/receipt triple written
// again at every call site.
func (c *Capture) reject(sink receiptSink, clientAddr, targetHost string, f failure) {
	row := failureTable[f.kind]
	if row.reason != "" {
		c.recordMITMError(clientAddr, targetHost, row.reason, f.details)
	}
	if row.status == 0 || sink == nil {
		return
	}
	sink.reject(row.status, row.receipt(f.details))
}

func (r failureRow) receipt(details map[string]any) string {
	if !r.appendError {
		return r.text
	}
	message, _ := details["error"].(string)
	if message == "" {
		return r.text
	}
	return r.text + ": " + message
}

// h1ReceiptSink writes failures as raw HTTP/1.1 responses. afterConnect marks a
// CONNECT tunnel that has already delivered its 200: from then on the client is
// inside TLS, so no HTTP receipt can be written and the caller only closes.
type h1ReceiptSink struct {
	writer       *bufio.Writer
	afterConnect bool
}

func (s h1ReceiptSink) reject(status int, body string) {
	if s.writer == nil || s.afterConnect {
		return
	}
	_ = writeSimpleResponse(s.writer, status, http.StatusText(status), body+"\n")
	_ = s.writer.Flush()
}

// h2ReceiptSink reports a failure on one HTTP/2 stream.
type h2ReceiptSink struct{ writer http.ResponseWriter }

func (s h2ReceiptSink) reject(status int, body string) {
	http.Error(s.writer, body, status)
}

// h2OnlyEvidence reports whether a failed upstream step can be explained by an
// h2-only origin and is therefore worth probing. An origin that confirmed
// HTTP/1.1 rules it out; one that selected h2 and then failed the HTTP/1.1
// redial is exactly the case the probe explains.
func h2OnlyEvidence(mode ALPNMode, negotiatedALPN string) bool {
	switch negotiatedALPN {
	case "http/1.1":
		return false
	case "h2":
		return true
	}
	// No ALPN result: only an origin that was never offered h2 can be
	// explained by h2-only support.
	return !mode.AllowsH2()
}

// h2OnlyFailure classifies a failed upstream step as h2_only_target when a
// fresh verified h2 handshake succeeds. It is the one place the probe runs and
// the hint is assembled, so the CONNECT handshake, the protocol redial, and a
// failed exchange inside a tunnel all classify the same way.
func (c *Capture) h2OnlyFailure(mode ALPNMode, negotiatedALPN, host, address string, cause error) (failure, bool) {
	if !h2OnlyEvidence(mode, negotiatedALPN) {
		return failure{}, false
	}
	h2Only, probeDetail := probeH2OnlyTarget(host, address)
	if !h2Only {
		return failure{}, false
	}
	details := map[string]any{
		"error": cause.Error(),
		"hint":  h2OnlyHint(mode),
	}
	if probeDetail != "" {
		details["probe_error"] = probeDetail
	}
	return newFailure(failH2OnlyTarget).withDetails(details), true
}

// classifyTunnelFailure explains a failed exchange inside a tunnel: an h2-only
// origin takes precedence over the generic failure. Outside a tunnel there is
// nothing to probe, so the fallback stands.
func (c *Capture) classifyTunnelFailure(tunnel *mitmUpstream, fallback failureKind, cause error) failure {
	if tunnel != nil {
		if h2Only, ok := c.h2OnlyFailure(c.http2ALPN, tunnel.alpn, tunnel.host, tunnel.address, cause); ok {
			return h2Only
		}
	}
	return newFailure(fallback)
}

// h2OnlyHint explains what the caller can do about an h2-only target. Under
// the default h1 advertisement the remedy is the CLI flag; once h2 is enabled
// the client is the part that cannot speak it.
func h2OnlyHint(mode ALPNMode) string {
	if mode.AllowsH2() {
		return "the client negotiated HTTP/1.1 but the target only supports h2; use an h2-capable client"
	}
	return "restart the proxy with --http2-alpn h2 or h2,h1"
}

// probeH2OnlyTarget checks whether a target that refused HTTP/1.1 completes a
// verified h2 handshake. A successful probe is the h2-only evidence that makes
// the failure actionable for the caller.
func probeH2OnlyTarget(host, address string) (bool, string) {
	conn, state, err := dialVerifiedUpstream(host, address, []string{"h2"})
	if err != nil {
		return false, err.Error()
	}
	defer conn.Close()
	return state.NegotiatedProtocol == "h2", ""
}
