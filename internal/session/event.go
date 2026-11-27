package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Kind is an event kind persisted in history.jsonl. It is a named type so a
// projection cannot invent a value, and `mitt spec` derives every kind enum
// from Kinds instead of repeating the vocabulary in struct tags.
type Kind string

// Event kinds persisted in history.jsonl.
const (
	KindCaptured  Kind = "captured"
	KindReplayed  Kind = "replayed"
	KindSent      Kind = "sent"
	KindMITMError Kind = "mitm_error"
)

// Kinds lists every event kind in contract order. The history filter enum and
// every published output schema derive from this list.
var Kinds = []string{string(KindCaptured), string(KindReplayed), string(KindSent), string(KindMITMError)}

// EnumValues makes Kind the one source of the kind vocabulary: spec derives
// the enum of every field of this type from it, so no output DTO repeats it.
func (Kind) EnumValues() []string {
	return Kinds
}

// Timings records when phases of a request completed. Only total_ms is
// required; additional fields may be added without breaking readers.
type Timings struct {
	TotalMS int64 `json:"total_ms" schema:"min=0"`
}

// Event is one line in history.jsonl. Fields not relevant to a kind are
// omitted. Body sizes are pointers so a zero-byte body is still persisted.
type Event struct {
	ID       string `json:"id"`
	TS       string `json:"ts" schema:"format=date-time"`
	Kind     Kind   `json:"kind"`
	ParentID string `json:"parent_id,omitempty"`
	// Mutations is the raw --patch text (before --var substitution) that
	// produced this request.
	Mutations string `json:"mutations,omitempty"`

	ClientAddr  string   `json:"client_addr,omitempty" desc:"Client address as host:port."`
	Method      string   `json:"method,omitempty"`
	URL         string   `json:"url,omitempty" schema:"format=uri"`
	HTTPVersion string   `json:"http_version,omitempty" desc:"Lowercased HTTP version token, for example http/1.1 or h2."`
	Status      int      `json:"status,omitempty" schema:"min=100,max=999"`
	Context     *string  `json:"context,omitempty"`
	Timings     *Timings `json:"timings,omitempty" schema:"open"`

	ReqBodyID    string `json:"req_body_id,omitempty" desc:"Body file prefix for the request; defaults to id when empty."`
	RespBodyID   string `json:"resp_body_id,omitempty" desc:"Body file prefix for the response; defaults to id when empty."`
	ReqBodySize  *int64 `json:"req_body_size,omitempty" schema:"min=0"`
	RespBodySize *int64 `json:"resp_body_size,omitempty" schema:"min=0"`

	RequestLine string `json:"request_line,omitempty"`
	// ForwardedRequestLine is the request line written to the next hop. A
	// proxy client speaks absolute-form and the origin receives origin-form,
	// so `show --raw` must use this field when it is present. When the next hop
	// speaks HTTP/2 there is no line, and this carries the equivalent head the
	// stream described.
	ForwardedRequestLine string      `json:"forwarded_request_line,omitempty" desc:"Request line sent to the next hop; the HTTP/2 equivalent when the next hop speaks h2."`
	ResponseLine         string      `json:"response_line,omitempty"`
	RequestHeaders       [][2]string `json:"request_headers,omitempty" desc:"Request headers in stored order, including duplicates."`
	ResponseHeaders      [][2]string `json:"response_headers,omitempty" desc:"Response headers in stored order, including duplicates."`

	TargetHost string         `json:"target_host,omitempty" desc:"Lowercased target hostname without a port; present on mitm_error events."`
	Reason     string         `json:"reason,omitempty" desc:"Lowercase snake_case MITM failure reason."`
	Details    map[string]any `json:"details,omitempty"`

	// UpstreamCertSHA256 pins the SHA-256 of the upstream certificate's
	// SubjectPublicKeyInfo for CONNECT MITM captures, making certificate
	// pinning and impersonation visible in history.
	UpstreamCertSHA256 string `json:"upstream_cert_sha256,omitempty" schema:"pattern=^[0-9a-f]{64}$" desc:"Lowercase hex SHA-256 of the upstream certificate's SubjectPublicKeyInfo; present on CONNECT MITM captures."`
}

// The "does this entry carry a response" question belongs to entry.View: the
// disk shape stores status as an int where 0 means "never completed", which is
// a rendering choice, not a fact about a stored line.

func (e *Event) requestBodyID() string {
	if e.ReqBodyID != "" {
		return e.ReqBodyID
	}
	return e.ID
}

func (e *Event) responseBodyID() string {
	if e.RespBodyID != "" {
		return e.RespBodyID
	}
	return e.ID
}

// HistoryPath is the event stream inside the data directory.
func (s *Store) HistoryPath() string {
	return filepath.Join(s.dir, "history.jsonl")
}

// MarshalEvent encodes one event as a single JSON line. HTML escaping is off
// so URLs and header values stay readable and match the CLI data output.
func MarshalEvent(event *Event) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(event); err != nil {
		return nil, fmt.Errorf("encode history event: %w", err)
	}
	return buf.Bytes(), nil
}
