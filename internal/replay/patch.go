package replay

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/httpwire"
	"github.com/C1ph3rX13/mitt/internal/output"
)

// patchVariablePattern matches one {{K}} placeholder in the raw patch text.
// Substitution happens before JSON parsing, so the patch text only carries the
// placeholder and never the (possibly secret) value.
var patchVariablePattern = regexp.MustCompile(`\{\{([^{}]+)\}\}`)

// Patch is the optional request-document modification: one source of JSON
// merge-patch text plus the --var substitutions the CLI already resolved.
// Only this package implements it; nil means no patch was requested.
type Patch interface {
	patch()
}

// InlinePatch carries merge-patch text directly on the command line.
type InlinePatch struct {
	Text string
	Vars map[string]string
}

func (InlinePatch) patch() {}

// FilePatch reads merge-patch text from Path.
type FilePatch struct {
	Path string
	Vars map[string]string
}

func (FilePatch) patch() {}

// StdinPatch reads merge-patch text from Reader.
type StdinPatch struct {
	Reader io.Reader
	Vars   map[string]string
}

func (StdinPatch) patch() {}

// applyPatch resolves the patch payload, applies RFC 7386 JSON Merge Patch
// semantics to the base request document, validates the result, and renders
// the final request bytes. The returned mutations string is the raw patch text
// before variable substitution, so environment-provided secrets are not
// persisted in history.
func applyPatch(base *entry.Document, patch Patch) (*preparedRequest, error) {
	raw, variables, err := patchText(patch)
	if err != nil {
		return nil, err
	}
	resolved, err := substitutePatchVariables(raw, variables)
	if err != nil {
		return nil, err
	}

	var patchValue any
	if err := json.Unmarshal([]byte(resolved), &patchValue); err != nil {
		return nil, invalidPatch("parse --patch: "+err.Error(), "pass a JSON object using RFC 7386 merge-patch syntax")
	}
	patchObject, ok := patchValue.(map[string]any)
	if !ok {
		return nil, invalidPatch("--patch must be a JSON object", "pass an object whose keys are request-document fields")
	}

	baseObject, err := documentObject(base)
	if err != nil {
		return nil, err
	}
	merged, ok := mergePatch(baseObject, patchObject).(map[string]any)
	if !ok {
		return nil, invalidPatch("--patch produced a non-object request document", "pass an object whose keys are request-document fields")
	}
	final, err := decodeMergedDocument(merged, patchObject, base.StoredBodyRef())
	if err != nil {
		return nil, err
	}

	rendered, err := final.materialize()
	if err != nil {
		return nil, invalidPatch("encode request: "+err.Error(), "produce a valid HTTP/1.1 request")
	}
	return rendered.prepared(
		// The final document is the one the bytes were rendered from, so
		// --dry-run cannot preview a document that disagrees with its bytes.
		entry.NewDocument(final.method, final.target, rendered.head.Headers, final.body),
		raw,
	), nil
}

// patchText resolves a patch payload to its raw text and substitutions. The
// CLI has already translated flag syntax into a payload type; this function
// only owns reading the payload.
func patchText(patch Patch) (string, map[string]string, error) {
	switch value := patch.(type) {
	case InlinePatch:
		return value.Text, value.Vars, nil
	case FilePatch:
		if value.Path == "" {
			return "", nil, invalidPatch("--patch @path requires a file path", "use --patch @patch.json or inline JSON")
		}
		data, err := os.ReadFile(value.Path)
		if err != nil {
			return "", nil, invalidPatch("read --patch file: "+err.Error(), "check that the patch file exists and is readable").
				WithDetail("path", value.Path)
		}
		return string(data), value.Vars, nil
	case StdinPatch:
		if value.Reader == nil {
			return "", nil, invalidPatch("--patch - requires stdin", "pipe the patch JSON into mitt")
		}
		data, err := io.ReadAll(value.Reader)
		if err != nil {
			return "", nil, invalidPatch("read --patch from stdin: "+err.Error(), "check the pipeline feeding stdin")
		}
		return string(data), value.Vars, nil
	default:
		return "", nil, invalidPatch("--patch source is invalid", "use inline JSON, @path, or - for stdin")
	}
}

// substitutePatchVariables replaces every {{K}} in the raw patch text in one
// pass. Substituted values are not rescanned, so a value may itself contain
// braces.
func substitutePatchVariables(raw string, variables map[string]string) (string, error) {
	missing := []string{}
	seen := map[string]bool{}
	resolved := patchVariablePattern.ReplaceAllStringFunc(raw, func(match string) string {
		name := match[2 : len(match)-2]
		value, ok := variables[name]
		if !ok {
			if !seen[name] {
				seen[name] = true
				missing = append(missing, name)
			}
			return match
		}
		return value
	})
	if len(missing) > 0 {
		return "", invalidPatch("--patch has unresolved variables: "+strings.Join(missing, ", "), "pass --var K=V for every {{K}} placeholder").
			WithDetail("variables", missing)
	}
	return resolved, nil
}

// mergePatch applies RFC 7386 semantics: a non-null object merges
// recursively, null deletes the key, and any other value replaces the target.
func mergePatch(target any, patch any) any {
	patchObject, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	targetObject, ok := target.(map[string]any)
	if !ok {
		targetObject = map[string]any{}
	}
	for key, value := range patchObject {
		if value == nil {
			delete(targetObject, key)
			continue
		}
		targetObject[key] = mergePatch(targetObject[key], value)
	}
	return targetObject
}

// documentObject projects a request document into the JSON object used as the
// merge-patch target. The base query is dropped so the final query follows the
// final URL unless the patch itself sets "query".
func documentObject(document *entry.Document) (map[string]any, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return nil, output.InternalErrorf("encode request document: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, output.InternalErrorf("decode request document: %v", err)
	}
	delete(object, "query")
	return object, nil
}

// decodeMergedDocument validates every field of the merged document and
// produces the final request materialize renders. Anything the request-document
// contract cannot express is an INVALID_PATCH.
func decodeMergedDocument(merged, patchObject map[string]any, stored *entry.StoredBody) (*finalRequest, error) {
	for key := range merged {
		switch key {
		case "method", "url", "headers", "query", "body", "body_base64", "body_file":
		default:
			return nil, invalidPatch("--patch produced unknown request-document field "+strconv.Quote(key), "only method, url, headers, query, body, body_base64, and body_file are patchable")
		}
	}

	method, ok := merged["method"].(string)
	if !ok || method == "" {
		return nil, invalidPatch("--patch produced an empty method", "set method to a non-empty HTTP token")
	}
	if !httpwire.ValidToken(method) {
		return nil, invalidPatch("--patch produced an invalid method "+strconv.Quote(method), "set method to a valid HTTP token")
	}

	rawURL, ok := merged["url"].(string)
	if !ok || rawURL == "" {
		return nil, invalidPatch("--patch produced an empty url", "set url to an absolute http:// or https:// URL")
	}
	target, err := parseAbsoluteURL(rawURL)
	if err != nil {
		return nil, invalidPatch("--patch produced an invalid url: "+err.Error(), "set url to an absolute http:// or https:// URL").
			WithDetail("url", rawURL)
	}

	headers := [][2]string{}
	if value, ok := merged["headers"]; ok && value != nil {
		headers, err = decodeDocumentPairs(value, "headers")
		if err != nil {
			return nil, err
		}
		if err := validateDocumentHeaders(headers); err != nil {
			return nil, err
		}
	}

	if value, ok := merged["query"]; ok && value != nil {
		query, err := decodeDocumentPairs(value, "query")
		if err != nil {
			return nil, err
		}
		target.RawQuery = encodeQueryPairs(query)
		target.ForceQuery = false
	}

	hostExplicit := false
	if value, ok := patchObject["headers"]; ok && value != nil {
		for _, header := range headers {
			if strings.EqualFold(header[0], "Host") {
				hostExplicit = true
				break
			}
		}
	}

	body, err := resolveDocumentBody(merged, stored)
	if err != nil {
		return nil, err
	}
	return &finalRequest{
		method:       method,
		target:       target,
		headers:      headers,
		body:         body,
		hostExplicit: hostExplicit,
	}, nil
}

// decodeDocumentPairs decodes an ordered [["K","V"], ...] array. Duplicates
// survive because the result is a slice, never a map.
func decodeDocumentPairs(value any, field string) ([][2]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, invalidPatch("--patch "+field+" must be an array of [name, value] pairs", "use [[\"name\",\"value\"], ...]")
	}
	pairs := make([][2]string, 0, len(items))
	for index, item := range items {
		pair, ok := item.([]any)
		if !ok || len(pair) != 2 {
			return nil, invalidPatch("--patch "+field+" entry "+strconv.Itoa(index)+" must be a [name, value] pair", "use [[\"name\",\"value\"], ...]").
				WithDetail("field", field).WithDetail("index", index)
		}
		name, nameOK := pair[0].(string)
		value, valueOK := pair[1].(string)
		if !nameOK || !valueOK {
			return nil, invalidPatch("--patch "+field+" entry "+strconv.Itoa(index)+" must contain two strings", "use [[\"name\",\"value\"], ...]").
				WithDetail("field", field).WithDetail("index", index)
		}
		pairs = append(pairs, [2]string{name, value})
	}
	return pairs, nil
}

func validateDocumentHeaders(headers [][2]string) error {
	for _, header := range headers {
		if !httpwire.ValidToken(header[0]) {
			return invalidPatch("--patch produced an invalid header name "+strconv.Quote(header[0]), "use an HTTP token as the header name")
		}
		if !httpwire.ValidHeaderValue(header[1]) {
			return invalidPatch("--patch produced a header value containing a control character", "remove control characters from header values").
				WithDetail("header", header[0])
		}
	}
	return nil
}

// resolveDocumentBody materializes the merged document's body. Choosing among
// body, body_base64, and body_file and turning the choice into bytes is the
// request document's rule (entry.BodyFromObjectWith), so a patch cannot accept
// a document the published schema rejects, and the merge cannot hold a second
// implementation that drifts. The base's record reference travels with the
// merge: a patch that leaves body_file alone still reads the captured body, so
// its recorded size is honoured rather than re-guessed from the path.
//
// This function only translates the document's diagnostic into the command's
// INVALID_PATCH, naming the field to fix. A damaged store is not a bad patch,
// so those diagnostics stay internal errors.
func resolveDocumentBody(merged map[string]any, stored *entry.StoredBody) ([]byte, error) {
	body, err := entry.BodyFromObjectWith(merged, stored)
	if err != nil {
		return nil, translateBodyError(err)
	}
	return body, nil
}

func translateBodyError(err error) error {
	var multiple *entry.MultipleBodyRepresentationsError
	if errors.As(err, &multiple) {
		names := make([]string, len(multiple.Fields))
		for i, field := range multiple.Fields {
			names[i] = string(field)
		}
		return invalidPatch(
			"--patch produced multiple body representations: "+strings.Join(names, ", "),
			"keep at most one of body, body_base64, and body_file and set the others to null",
		)
	}

	var typed *entry.BodyFieldTypeError
	if errors.As(err, &typed) {
		hints := map[entry.BodyField]string{
			entry.BodyFieldText:   "use body for UTF-8 text, body_base64 for binary, or body_file for a path",
			entry.BodyFieldBase64: "use body_base64 for base64-encoded binary",
			entry.BodyFieldFile:   "set body_file to a readable path",
		}
		return invalidPatch("--patch "+string(typed.Field)+" must be a string or null", hints[typed.Field])
	}

	var fileErr *entry.BodyFileError
	if errors.As(err, &fileErr) {
		return invalidPatch("read --patch body_file: "+fileErr.Err.Error(), "check that the body file exists and is readable").
			WithDetail("path", fileErr.Path)
	}

	var encodingErr *entry.BodyBase64Error
	if errors.As(err, &encodingErr) {
		return invalidPatch("--patch body_base64 is not valid base64: "+encodingErr.Err.Error(), "encode binary bodies with standard base64")
	}

	// A stored body the record claims but the disk cannot deliver is damage to
	// the data directory, not a mistake in the patch, so it keeps the error code
	// the rest of the store uses for that condition.
	var storedErr entry.StoredBodyDiagnostic
	if errors.As(err, &storedErr) {
		entryID, direction, path := storedErr.Evidence()
		return output.NewError(
			output.CodeInternalError,
			storedErr.Error(),
			"the history entry records body bytes its files no longer hold; inspect the data directory",
		).WithDetail("id", entryID).
			WithDetail("direction", direction).
			WithDetail("path", path)
	}

	// The document rule reports only the shapes above, so anything left is a
	// programmer error rather than a patch mistake.
	return output.InternalErrorf("resolve request body: %v", err)
}

// encodeQueryPairs renders ordered pairs back into a raw query string,
// preserving order and duplicates.
func encodeQueryPairs(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, url.QueryEscape(pair[0])+"="+url.QueryEscape(pair[1]))
	}
	return strings.Join(parts, "&")
}

func invalidPatch(message, hint string) *output.AppError {
	return output.NewError(output.CodeInvalidPatch, message, hint)
}
