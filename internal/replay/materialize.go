package replay

import (
	"bytes"
	"net/url"

	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/httpwire"
)

// finalRequest is the request a base and an optional patch have finished
// resolving: method, absolute target, ordered headers, entity body, and whether
// the patch set Host itself. It is the only input to materialize, so every base
// reaches the wire through one strategy.
type finalRequest struct {
	method  string
	target  *url.URL
	headers [][2]string
	body    []byte
	// hostExplicit records that the patch set Host in the request document.
	// Otherwise the final URL owns it.
	hostExplicit bool
}

// resolvedRequest is the outcome of the preconditions plus rendering both send
// entry points share: the request about to be written and the outbound TLS
// profile it was validated against.
type resolvedRequest struct {
	prepared *preparedRequest
	profile  TLSProfile
}

// resolve validates the outbound TLS profile and then resolves the base and
// patch into the exact request bytes. Send and DryRun both call it, so
// everything DryRun previews is something Send would accept and send; the only
// difference between the two is that DryRun stops before dialing.
func resolve(request SendRequest) (*resolvedRequest, error) {
	profile, err := ParseTLSProfile(request.TLSProfile)
	if err != nil {
		return nil, err
	}
	prepared, err := prepare(request)
	if err != nil {
		return nil, err
	}
	return &resolvedRequest{prepared: prepared, profile: profile}, nil
}

// materialized is one rendering of a final request: the head that describes it,
// the exact bytes that carry it, and the address they go to. The head and the
// bytes come from the same value, so a recorded request line cannot describe a
// request other than the one sent.
type materialized struct {
	target *url.URL
	origin httpwire.Origin
	head   *httpwire.Request
	body   []byte
	raw    []byte
}

// materialize renders a final request document to the bytes put on the wire.
//
// Framing has exactly one rule and it reads only the final document: a head
// that declared a body (Content-Length or Transfer-Encoding) or a non-empty
// body gets a single Content-Length carrying the final size, and any
// Transfer-Encoding is removed. The rule does not depend on which base produced
// the document or whether a patch was applied, so an empty patch cannot change
// the bytes. Host follows the final URL unless the patch set it explicitly.
func (r finalRequest) materialize() (*materialized, error) {
	headers := r.headers
	if hasBodyFraming(headers) || len(r.body) > 0 {
		headers = httpwire.WithBodyLength(headers, int64(len(r.body)))
	}
	if !r.hostExplicit {
		headers = httpwire.WithHost(headers, r.target.Host)
	}

	method := r.method
	if method == "" {
		method = "GET"
	}
	head, err := httpwire.ParseRequest(method+" "+requestTarget(r.target)+" HTTP/1.1", headers)
	if err != nil {
		return nil, err
	}

	var rendered bytes.Buffer
	if err := head.Write(&rendered, bytes.NewReader(r.body), int64(len(r.body))); err != nil {
		return nil, err
	}
	return &materialized{
		target: r.target,
		origin: httpwire.OriginOfURL(r.target),
		head:   head,
		body:   r.body,
		raw:    rendered.Bytes(),
	}, nil
}

// prepared pairs the rendering with the request document the caller reports.
// A base that was not patched reports its own document; a patched request
// reports the document its rendered head carries.
func (m *materialized) prepared(document *entry.Document, mutations string) *preparedRequest {
	return &preparedRequest{
		target:    m.target,
		origin:    m.origin,
		head:      m.head,
		body:      m.body,
		raw:       m.raw,
		mutations: mutations,
		document:  document,
	}
}

// hasBodyFraming reports whether a header set declares an entity body. A base
// that framed its body keeps a length after re-rendering, so a decoded empty
// body does not silently change the head shape.
func hasBodyFraming(headers [][2]string) bool {
	return len(httpwire.Values(headers, "Content-Length")) > 0 ||
		len(httpwire.Values(headers, "Transfer-Encoding")) > 0
}
