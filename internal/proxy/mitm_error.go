//go:build linux

package proxy

import (
	"time"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// Stable mitm_error reasons. They are lowercase snake_case so history filters
// and agents can match them without parsing human-readable diagnostics.
const (
	MITMReasonScopeDenied        = "scope_denied"
	MITMReasonClientCertRejected = "client_cert_rejected"
	MITMReasonUpstreamTLS        = "upstream_tls"
	MITMReasonUpstreamConnect    = "upstream_connect"
	MITMReasonNotTLS             = "not_tls"
	MITMReasonLeafCertificate    = "leaf_certificate"
	// MITMReasonH2OnlyTarget reports a target that refuses HTTP/1.1 and only
	// completes an h2 handshake while the proxy advertises h1.
	MITMReasonH2OnlyTarget = "h2_only_target"
	// MITMReasonH2ParseError is the defensive warning event for an h2 frame or
	// stream that x/net/http2 could not parse. The raw frame bytes are kept in
	// body files referenced from details.
	MITMReasonH2ParseError = "h2_parse_error"
)

// MITMReasons lists every reason the proxy can record, in stable order. The
// failure table owns the mapping from a failure kind to its reason; this list
// is the vocabulary those rows must draw from, plus the defensive parse-error
// reason that is written directly with its captured bytes. The published show
// schema derives its reason description from this list, so a new reason cannot
// ship undocumented.
var MITMReasons = []string{
	MITMReasonScopeDenied,
	MITMReasonClientCertRejected,
	MITMReasonUpstreamTLS,
	MITMReasonUpstreamConnect,
	MITMReasonNotTLS,
	MITMReasonLeafCertificate,
	MITMReasonH2OnlyTarget,
	MITMReasonH2ParseError,
}

// recordMITMError commits one failure event and mirrors it to the NDJSON
// event sink when `proxy start --json` is active. A failure here never
// terminates the proxy; it only emits a diagnostic on stderr.
func (c *Capture) recordMITMError(clientAddr, targetHost, reason string, details map[string]any) {
	rec, err := c.store.Begin(session.RecordSpec{
		Kind:       session.KindMITMError,
		Started:    time.Now(),
		ClientAddr: clientAddr,
		TargetHost: normalizeHost(targetHost),
		Reason:     reason,
	})
	if err != nil {
		c.warn("create mitm_error event for %s: %v", targetHost, err)
		return
	}
	defer rec.Abort()
	rec.Event().Details = details
	if err := rec.Commit(c.publish); err != nil {
		c.warn("commit mitm_error for %s: %v", targetHost, err)
	}
}

func scopeDetails(decision ScopeDecision) map[string]any {
	details := map[string]any{"matched": decision.Matched}
	if decision.Pattern != "" {
		details["pattern"] = decision.Pattern
	}
	return details
}
