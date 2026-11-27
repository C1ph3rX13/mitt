// Package httpwire owns HTTP/1 message framing: reading a request or response
// head, deciding where its body ends, copying that body, and writing a message
// with ordered, duplicate-preserving headers.
package httpwire

import (
	"strconv"
	"strings"
)

// bodyFraming records the rule that decides where a body ends. It is private
// so callers cannot combine a Message with a framing rule that disagrees with
// its headers.
type bodyFraming struct {
	chunked        bool
	length         int64
	hasBody        bool
	closeDelimited bool
	// bodyForbidden marks HEAD / 204 / 304 responses: a body must never be
	// written even if a stored body file unexpectedly has bytes.
	bodyForbidden bool
}

// Message is one HTTP/1 message head plus its body framing rule.
//
// StartLine and Headers are exported so callers can read the received head.
// They are not meant to be edited in place: the framing rule was computed when
// the Message was read or parsed, so a hand-edited Content-Length or
// Transfer-Encoding would leave the rule disagreeing with the head.
//
// Two operations cover the header work callers actually do, and they differ in
// whether the framing rule survives:
//
//   - Forwarded filters headers for the next hop and keeps the received rule,
//     because the body is re-encoded exactly as it was decoded.
//   - WithBodyLength describes a body that is being re-rendered. The caller
//     re-parses the result, so the rule is derived from what is really sent
//     rather than carried over from what was received.
//
// Host rewriting is WithHost, which never touches the rule because Host does
// not decide where a body ends.
type Message struct {
	StartLine string
	Headers   [][2]string
	framing   bodyFraming
}

// Request is a parsed HTTP/1 request head. Reading the body is a separate
// action so large bodies stay streamed.
type Request struct {
	Message
	Method  string
	Target  string
	Version string
}

// Response is a parsed final HTTP/1 response head. Interim 1xx responses are
// consumed by ReadResponse and never reach this type.
type Response struct {
	Message
	Version string
	Status  int
}

// CloseDelimited reports whether the body ends when the connection closes.
// Callers use it to decide whether the connection can be reused.
func (m *Message) CloseDelimited() bool {
	return m.framing.closeDelimited
}

// FramingHeaders returns a copy of the headers adjusted for safe forwarding:
// when Transfer-Encoding ends in chunked, every Content-Length field is
// removed. The Message itself is never modified, so history display can keep
// the bytes that were actually received.
func (m *Message) FramingHeaders() [][2]string {
	return FramingHeaders(m.Headers)
}

// FramingHeaders is FramingHeaders for a header set that is not (or not yet) a
// parsed Message, such as one a caller has already filtered for the next hop.
func FramingHeaders(headers [][2]string) [][2]string {
	cloned := clonePairs(headers)
	if !headersUseChunked(cloned) {
		return cloned
	}
	return Strip(cloned, "Content-Length")
}

func headersUseChunked(headers [][2]string) bool {
	last := ""
	for _, value := range Values(headers, "Transfer-Encoding") {
		for _, token := range splitTokens(value) {
			last = token
		}
	}
	return last == "chunked"
}

// Forwarded returns the head to send to the next hop. startLine and headers
// replace the received ones, so a caller that rewrote Host or dropped a marker
// field hands over its finished set; Forwarded then filters framing headers for
// the new hop and keeps the received framing rule.
//
// Both halves matter and both are easy to get wrong by hand: filtering is
// needed because a chunked message must not keep a Content-Length, and keeping
// the rule is needed because the body was decoded under it and is re-encoded
// under it. The returned Headers are the set that Write will emit, so callers
// must record those rather than their pre-filter input.
//
// Changing how much body there is belongs to WithBodyLength, not here.
func (m *Message) Forwarded(startLine string, headers [][2]string) Message {
	return Message{
		StartLine: startLine,
		Headers:   FramingHeaders(headers),
		framing:   m.framing,
	}
}

// WithBodyLength returns headers whose framing describes exactly size body
// bytes: every Transfer-Encoding field is removed and a single Content-Length
// field carries size, occupying the position the first Transfer-Encoding had
// (otherwise the position of the first Content-Length). Duplicate
// Content-Length fields collapse into that one field.
//
// This is the one place that decides how a re-rendered head declares its body,
// so callers cannot leave a head framing a body size they are not sending. The
// input is never modified and every other field keeps its order.
//
// Callers that must not declare a length (an unframed request with an empty
// body) simply do not call this: whether to frame belongs to them. Callers
// forwarding an unchanged body use FramingHeaders instead, which keeps the
// received rule.
func WithBodyLength(headers [][2]string, size int64) [][2]string {
	lengthValue := strconv.FormatInt(size, 10)

	rewritten := make([][2]string, 0, len(headers)+1)
	firstTransferEncoding := -1
	lengthWritten := false
	for _, header := range headers {
		switch {
		case strings.EqualFold(header[0], "Transfer-Encoding"):
			if firstTransferEncoding < 0 {
				firstTransferEncoding = len(rewritten)
			}
			continue
		case strings.EqualFold(header[0], "Content-Length"):
			// Collapse duplicate Content-Length fields into the first one.
			if lengthWritten {
				continue
			}
			rewritten = append(rewritten, [2]string{"Content-Length", lengthValue})
			lengthWritten = true
			continue
		}
		rewritten = append(rewritten, header)
	}
	if !lengthWritten {
		length := [2]string{"Content-Length", lengthValue}
		if firstTransferEncoding >= 0 && firstTransferEncoding <= len(rewritten) {
			rewritten = append(rewritten, [2]string{})
			copy(rewritten[firstTransferEncoding+1:], rewritten[firstTransferEncoding:])
			rewritten[firstTransferEncoding] = length
		} else {
			rewritten = append(rewritten, length)
		}
	}
	return rewritten
}

// WithHost returns headers with every Host field value replaced by authority.
// A header set with no Host field gets one appended. Position and multiplicity
// of existing Host fields are preserved, so a caller that rewrote the head for
// a different authority does not silently drop a duplicate the client sent.
//
// An empty authority returns the headers unchanged: "" means "the Host is not
// mine to set", not "remove it".
func WithHost(headers [][2]string, authority string) [][2]string {
	if authority == "" {
		return clonePairs(headers)
	}
	rewritten := clonePairs(headers)
	found := false
	for i := range rewritten {
		if strings.EqualFold(rewritten[i][0], "Host") {
			rewritten[i][1] = authority
			found = true
		}
	}
	if !found {
		rewritten = append(rewritten, [2]string{"Host", authority})
	}
	return rewritten
}

func splitTokens(value string) []string {
	tokens := []string{}
	for _, token := range strings.Split(value, ",") {
		if token = strings.TrimSpace(strings.ToLower(token)); token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}
