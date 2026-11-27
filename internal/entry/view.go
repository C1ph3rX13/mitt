// Package entry is the one answer to "what does a history entry look like".
//
// A stored session.Event is the disk contract. Every command that shows,
// lists, replays, or compares an entry reads it through this package, so the
// names, nil handling, and body-size defaults of an entry's outward form are
// defined once instead of in five DTOs that drift apart.
//
// There are exactly two levels: View (the summary every consumer shares) and
// Detail (View plus where the bodies live and the request document). There is
// one single-entry read: it scans history.jsonl once, backfills body sizes only
// for the entries returned, and reports missing ids as session.NotFoundError.
// Commands translate that value through Translate; they never re-implement the
// lookup or the not-found shape.
package entry

import (
	"github.com/C1ph3rX13/mitt/internal/session"
)

// View is the summary projection of one history entry: identity, routing,
// ordered headers, one body-size shape, and the failure fields of a mitm_error.
// `history` renders a narrow column subset of it (see ListRow) and `show` emits
// it whole, so the two cannot disagree about status or body sizes.
type View struct {
	ID       string       `json:"id"`
	Kind     session.Kind `json:"kind"`
	TS       string       `json:"ts" schema:"format=date-time"`
	ParentID string       `json:"parent_id,omitempty"`
	Context  string       `json:"context"`

	ClientAddr  string `json:"client_addr,omitempty"`
	Method      string `json:"method,omitempty"`
	URL         string `json:"url,omitempty" schema:"format=uri"`
	HTTPVersion string `json:"http_version,omitempty" desc:"Lowercased HTTP version token, for example http/1.1 or h2."`
	// RequestLine is the line the client wrote or, for a protocol with no
	// line, the synthesized equivalent. ForwardedRequestLine is the line the
	// next hop received. They differ when the proxy rewrites the target or
	// bridges protocols, and both are reported: `show --raw` writes the
	// forwarded line while the summary keeps the received one, so neither
	// answer has to be guessed from the other.
	RequestLine          string      `json:"request_line,omitempty"`
	ForwardedRequestLine string      `json:"forwarded_request_line,omitempty" desc:"Request line sent to the next hop; the HTTP/2 equivalent when the next hop speaks h2."`
	RequestHeaders       [][2]string `json:"request_headers" desc:"Request headers in stored order, including duplicates."`
	ReqBodySize          int64       `json:"req_body_size" schema:"min=0"`

	Status          *int             `json:"status" schema:"nullable"`
	ResponseLine    string           `json:"response_line,omitempty"`
	ResponseHeaders [][2]string      `json:"response_headers" desc:"Response headers in stored order, including duplicates."`
	RespBodySize    int64            `json:"resp_body_size" schema:"min=0"`
	Timings         *session.Timings `json:"timings,omitempty" schema:"open"`

	// Mutations is the raw --patch text (before --var substitution) that
	// produced a replayed request.
	Mutations string `json:"mutations,omitempty" desc:"Raw --patch text before --var substitution; present on replayed entries."`

	TargetHost string         `json:"target_host,omitempty" desc:"Lowercased target hostname without a port; present on mitm_error events."`
	Reason     string         `json:"reason,omitempty" desc:"Lowercase snake_case failure reason; present on mitm_error events."`
	Details    map[string]any `json:"details,omitempty"`

	UpstreamCertSHA256 string `json:"upstream_cert_sha256,omitempty" schema:"pattern=^[0-9a-f]{64}$" desc:"Lowercase hex SHA-256 of the upstream certificate's SubjectPublicKeyInfo; present on CONNECT MITM captures."`
}

// HasResponse reports whether the entry carries an upstream response.
// mitm_error entries never do, and an exchange that never completed has no
// status; both are rejected by `mitt diff`.
func (v *View) HasResponse() bool {
	return v.Kind != session.KindMITMError && v.Status != nil
}

// Detail is the detailed projection: the view plus the on-disk body locations
// and the structured request document. For an exchange entry both paths are
// present even when the corresponding body file is empty or missing, so a
// caller that wants the bytes never derives a file name itself.
type Detail struct {
	View

	RequestBodyPath  string    `json:"request_body_path"`
	ResponseBodyPath string    `json:"response_body_path"`
	RequestDocument  *Document `json:"request_document" schema:"nullable" desc:"At most one of body, body_base64, and body_file may be present and non-null."`

	// The body handles behind the two paths, already joined with this record's
	// size claims. Commands that need bytes read through these instead of
	// opening a path themselves, so "the record says N bytes" and "the disk
	// holds N bytes" are reconciled in one place.
	requestBody  StoredBody
	responseBody StoredBody
}

// RequestBodyHandle is the request body reference of this entry.
func (d *Detail) RequestBodyHandle() StoredBody { return d.requestBody }

// ResponseBodyHandle is the response body reference of this entry.
func (d *Detail) ResponseBodyHandle() StoredBody { return d.responseBody }

// ListRow is the stable narrow projection emitted by `mitt history`. It is
// derived from View, never from the disk event, so a listing row and a `show`
// summary always agree about kind, status, context, and body sizes.
type ListRow struct {
	ID           string       `json:"id"`
	Kind         session.Kind `json:"kind"`
	TS           string       `json:"ts" schema:"format=date-time"`
	Method       string       `json:"method"`
	URL          string       `json:"url"`
	Status       *int         `json:"status" schema:"nullable"`
	Context      string       `json:"context"`
	ReqBodySize  int64        `json:"req_body_size" schema:"min=0"`
	RespBodySize int64        `json:"resp_body_size" schema:"min=0"`
}

// ListRow projects a summary view onto the listing columns.
func (v *View) ListRow() ListRow {
	return ListRow{
		ID:           v.ID,
		Kind:         v.Kind,
		TS:           v.TS,
		Method:       v.Method,
		URL:          v.URL,
		Status:       v.Status,
		Context:      v.Context,
		ReqBodySize:  v.ReqBodySize,
		RespBodySize: v.RespBodySize,
	}
}

// Response is the response half of an entry: the input to a response
// comparison. It is projected from an entry by the same package that owns
// where its bytes live, so `diff` never re-derives a body path or decides on
// its own what a missing body means.
type Response struct {
	ID      string
	Kind    session.Kind
	URL     string
	Status  int
	Headers [][2]string
	Body    StoredBody
}

// Response projects the entry's response half for comparison. Status is the
// plain integer the comparison reports; HasResponse is the precondition.
func (d *Detail) Response() Response {
	status := 0
	if d.Status != nil {
		status = *d.Status
	}
	return Response{
		ID:      d.ID,
		Kind:    d.Kind,
		URL:     d.URL,
		Status:  status,
		Headers: d.ResponseHeaders,
		Body:    d.responseBody,
	}
}

// Summarize is the only projection from the disk shape onto the outward shape.
func Summarize(event *session.Event) View {
	view := View{
		ID:                   event.ID,
		Kind:                 event.Kind,
		TS:                   event.TS,
		ParentID:             event.ParentID,
		ClientAddr:           event.ClientAddr,
		Method:               event.Method,
		URL:                  event.URL,
		HTTPVersion:          event.HTTPVersion,
		RequestLine:          event.RequestLine,
		ForwardedRequestLine: event.ForwardedRequestLine,
		RequestHeaders:       ClonePairs(event.RequestHeaders),
		ResponseLine:         event.ResponseLine,
		ResponseHeaders:      ClonePairs(event.ResponseHeaders),
		Timings:              event.Timings,
		Mutations:            event.Mutations,
		TargetHost:           event.TargetHost,
		Reason:               event.Reason,
		Details:              event.Details,

		UpstreamCertSHA256: event.UpstreamCertSHA256,
	}
	if event.Context != nil {
		view.Context = *event.Context
	}
	if event.Status != 0 {
		status := event.Status
		view.Status = &status
	}
	if event.ReqBodySize != nil {
		view.ReqBodySize = *event.ReqBodySize
	}
	if event.RespBodySize != nil {
		view.RespBodySize = *event.RespBodySize
	}
	return view
}
