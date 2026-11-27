package replay

import (
	"os"

	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/httpwire"
	"github.com/C1ph3rX13/mitt/internal/output"
)

// Base is one of the three mutually exclusive sources of a send request.
// Only this package implements it, so a SendRequest carries one base and one
// payload; conflicting bases are not expressible.
type Base interface {
	base()
}

// EntryBase replays the request document of one history entry. It produces a
// replayed event whose parent is ID.
type EntryBase struct {
	ID string
}

func (EntryBase) base() {}

// URLBase constructs a request from an absolute http(s) URL. It produces a
// sent event.
type URLBase struct {
	URL string
}

func (URLBase) base() {}

// FileBase reads a raw HTTP/1.x request from Path. It produces a sent event.
type FileBase struct {
	Path string
}

func (FileBase) base() {}

// prepare resolves the request's single base into the exact request bytes, the
// final request document, and the metadata to dispatch. A missing base is the
// only invalid combination left; conflicting bases are rejected by the CLI
// before a SendRequest exists.
func prepare(request SendRequest) (*preparedRequest, error) {
	switch base := request.Base.(type) {
	case EntryBase:
		return prepareEntry(request, base.ID)
	case URLBase:
		return prepareURL(request, base.URL)
	case FileBase:
		return prepareFile(request, base.Path)
	default:
		return nil, output.NewError(
			output.CodeBaseConflict,
			"send requires exactly one base: <id>, --url, or --from-file",
			"pass exactly one base and retry",
		)
	}
}

func prepareEntry(request SendRequest, id string) (*preparedRequest, error) {
	if request.Store == nil {
		return nil, output.InternalErrorf("send requires a data store")
	}
	// The entry read is the same one `show --detail` performs: one scan, one
	// projection, and the store's corrupt-line warnings reach the caller's
	// diagnostics instead of being dropped.
	detail, err := entry.Load(request.Store, id, request.Diag)
	if err != nil {
		return nil, entry.Translate(err, request.Store.Dir())
	}
	document := detail.RequestDocument
	if document == nil {
		return nil, output.NewError(
			output.CodeInvalidRequest,
			"history entry "+id+" has no request",
			"choose a captured, replayed, or sent entry",
		).WithDetail("id", id).WithDetail("kind", string(detail.Kind))
	}
	target, err := parseAbsoluteURL(document.URL)
	if err != nil {
		return nil, output.NewError(
			output.CodeInvalidRequest,
			"history entry "+id+" has an invalid request URL: "+err.Error(),
			"use `send --url` to construct a new request instead",
		).WithDetail("id", id).WithDetail("url", document.URL)
	}
	if request.Patch != nil {
		return applyPatch(document, request.Patch)
	}
	body, err := document.BodyBytes()
	if err != nil {
		// The base's body comes from the record, so a store that cannot deliver
		// the bytes it claims gets the same diagnostic `show` would report.
		return nil, entry.Translate(err, request.Store.Dir())
	}
	rendered, err := finalRequest{
		method:  document.Method,
		target:  target,
		headers: document.Headers,
		body:    body,
	}.materialize()
	if err != nil {
		return nil, output.InternalErrorf("encode request for %s: %v", id, err)
	}
	return rendered.prepared(document, ""), nil
}

func prepareURL(request SendRequest, rawURL string) (*preparedRequest, error) {
	target, err := parseAbsoluteURL(rawURL)
	if err != nil {
		return nil, output.NewError(
			output.CodeInvalidRequest,
			"invalid --url: "+err.Error(),
			"pass an absolute http:// or https:// URL",
		).WithDetail("url", rawURL)
	}
	// A request built from scratch has no stored body, so its document is
	// self-contained: headers, query, and any body are all in memory.
	document := &entry.Document{
		Method:  "GET",
		URL:     rawURL,
		Headers: [][2]string{{"Host", target.Host}},
		Query:   entry.QueryPairs(target.RawQuery),
	}
	if request.Patch != nil {
		return applyPatch(document, request.Patch)
	}
	rendered, err := finalRequest{
		method:  "GET",
		target:  target,
		headers: document.Headers,
	}.materialize()
	if err != nil {
		return nil, output.InternalErrorf("encode request: %v", err)
	}
	return rendered.prepared(document, ""), nil
}

func prepareFile(request SendRequest, path string) (*preparedRequest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, output.NewError(
			output.CodeInvalidRequest,
			"read --from-file: "+err.Error(),
			"check that the file exists and is readable",
		).WithDetail("path", path)
	}
	parsed, err := parseRawHTTPRequest(data)
	if err != nil {
		return nil, output.NewError(
			output.CodeInvalidRequest,
			"parse --from-file: "+err.Error(),
			"provide a raw HTTP/1.x request with a request line, headers, and blank line",
		).WithDetail("path", path)
	}
	document := entry.NewDocument(parsed.method, parsed.targetURL, parsed.headers, parsed.body)
	if request.Patch != nil {
		return applyPatch(document, request.Patch)
	}
	// Without a patch the file is the byte-faithful escape hatch: its bytes are
	// sent verbatim, and the head parsed from those bytes is their identity. A
	// patched file is re-rendered like every other base.
	return &preparedRequest{
		target:   parsed.targetURL,
		origin:   httpwire.OriginOfURL(parsed.targetURL),
		head:     parsed.head,
		body:     parsed.body,
		raw:      data,
		document: document,
	}, nil
}
