package entry

import (
	"net/url"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// Document is the structured request form used by `show --detail`, `send
// --patch`, and `send --dry-run`. Headers and query parameters are ordered
// pairs so duplicate names survive a round trip. At most one body
// representation may be present and non-null; null or absence means no body.
// The body rule lives in this package (BodyBytes / BodyFromObject), and the
// at_most_one tags below publish the same rule as an executable JSON Schema
// constraint wherever a Document is projected.
type Document struct {
	Method     string      `json:"method" schema:"minlen=1"`
	URL        string      `json:"url" schema:"format=uri"`
	Headers    [][2]string `json:"headers" desc:"Ordered [name, value] pairs; order and duplicates are preserved."`
	Query      [][2]string `json:"query" desc:"Ordered [name, value] pairs derived from the URL query string."`
	Body       *string     `json:"body,omitempty" schema:"nullable,at_most_one=request-body" desc:"UTF-8 request body text."`
	BodyBase64 *string     `json:"body_base64,omitempty" schema:"nullable,at_most_one=request-body,encoding=base64" desc:"Base64-encoded binary request body."`
	BodyFile   *string     `json:"body_file,omitempty" schema:"nullable,at_most_one=request-body" desc:"Path to a file containing the request body."`

	// stored is the record reference this document was projected from, if any.
	// It is not part of the JSON shape: a document that came off disk knows
	// which of its representations is the record's own body, so BodyBytes can
	// interpret that body's availability with the record's size. A document
	// assembled from JSON or memory has no such provenance and applies the body
	// rule alone.
	stored *StoredBody
}

// documentOf projects an event onto the request document. The stored body is
// referenced by path whenever the record names one; projection decides nothing
// about whether those bytes can be read, so a missing body file keeps its
// reference intact for the reader that asks for bytes to diagnose.
// mitm_error entries have no request and yield nil.
func documentOf(event *session.Event, stored StoredBody) *Document {
	if event == nil || event.Kind == session.KindMITMError {
		return nil
	}

	method := event.Method
	rawURL := event.URL
	if method == "" || rawURL == "" {
		if lineMethod, lineURL, ok := splitRequestLine(event.RequestLine); ok {
			if method == "" {
				method = lineMethod
			}
			if rawURL == "" {
				rawURL = lineURL
			}
		}
	}
	if method == "" || rawURL == "" {
		return nil
	}

	document := &Document{
		Method:  method,
		URL:     rawURL,
		Headers: ClonePairs(event.RequestHeaders),
		Query:   QueryPairs(rawQuery(rawURL)),
	}
	if stored.Present() {
		document.stored = &stored
		// A zero-size body holds no bytes, so there is nothing for body_file to
		// point at. A record that names a body keeps its reference even when the
		// file is gone, because replaying it as a bodyless request would be a
		// silent lie about what was captured.
		if stored.Claimed() || !stored.SizeKnown {
			path := stored.Path
			document.BodyFile = &path
		}
	}
	return document
}

// NewDocument builds the document shape from a request rendered in memory,
// inlining the body.
func NewDocument(method string, target *url.URL, headers [][2]string, body []byte) *Document {
	document := &Document{
		Method:  method,
		URL:     target.String(),
		Headers: ClonePairs(headers),
		Query:   QueryPairs(target.RawQuery),
	}
	applyDocumentBody(document, body)
	return document
}

// QueryPairs decodes a raw query string into ordered name/value pairs. It
// preserves order and duplicates, which url.Values cannot.
func QueryPairs(rawQuery string) [][2]string {
	pairs := make([][2]string, 0)
	if rawQuery == "" {
		return pairs
	}
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		pairs = append(pairs, [2]string{decodeQueryPart(name), decodeQueryPart(value)})
	}
	return pairs
}

func decodeQueryPart(value string) string {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

func rawQuery(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		if index := strings.IndexByte(rawURL, '?'); index >= 0 {
			return rawURL[index+1:]
		}
		return ""
	}
	return parsed.RawQuery
}

func splitRequestLine(line string) (string, string, bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// ClonePairs copies ordered header or query pairs while keeping an empty
// result non-nil, so JSON always emits [] instead of null.
func ClonePairs(pairs [][2]string) [][2]string {
	cloned := make([][2]string, len(pairs))
	copy(cloned, pairs)
	return cloned
}
