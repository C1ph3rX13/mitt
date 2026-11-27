package entry

import (
	"encoding/base64"
	"errors"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// BodyField names one slot a request document can carry its body in. This
// vocabulary is the rule's whole surface: the selector below, the errors a
// caller sees, and the JSON Schema derived from Document's at_most_one tags all
// read the same three names, so a representation cannot exist in one place and
// be forgotten by another.
type BodyField string

const (
	// BodyFieldText is a UTF-8 body held inline in the document.
	BodyFieldText BodyField = "body"
	// BodyFieldBase64 is a binary body held inline as standard base64.
	BodyFieldBase64 BodyField = "body_base64"
	// BodyFieldFile is a body held in a file the document points at.
	BodyFieldFile BodyField = "body_file"
)

// BodyFields lists every representation in document order. Selection reports
// the fields it sees in this order, so an ambiguous document names its
// representations the way the document declares them.
var BodyFields = []BodyField{BodyFieldText, BodyFieldBase64, BodyFieldFile}

// BodyFieldNames renders the vocabulary as JSON property names. The published
// request-document schema uses it to pin the at_most_one group to this list.
func BodyFieldNames() []string {
	names := make([]string, len(BodyFields))
	for i, field := range BodyFields {
		names[i] = string(field)
	}
	return names
}

// MultipleBodyRepresentationsError reports a document that names more than one
// non-null body representation. A document with two bodies is ambiguous rather
// than richer, so it is a contract violation and not a preference.
type MultipleBodyRepresentationsError struct {
	Fields []BodyField
}

func (e *MultipleBodyRepresentationsError) Error() string {
	names := make([]string, len(e.Fields))
	for i, field := range e.Fields {
		names[i] = string(field)
	}
	return "request document has multiple body representations: " + strings.Join(names, ", ")
}

// BodyFieldTypeError reports a representation whose value the document shape
// cannot hold. Every body field is a string or null; null is the same as
// absence.
type BodyFieldTypeError struct {
	Field BodyField
}

func (e *BodyFieldTypeError) Error() string {
	return "request document field " + strconv.Quote(string(e.Field)) + " must be a string or null"
}

// BodyBase64Error reports a body_base64 whose text is not standard base64.
// Decoding is the rule's job, so the failure belongs to the document rather
// than to whichever command asked for the bytes.
type BodyBase64Error struct {
	Err error
}

func (e *BodyBase64Error) Error() string {
	return "request document body_base64 is not valid base64: " + e.Err.Error()
}

func (e *BodyBase64Error) Unwrap() error { return e.Err }

// BodyFileError reports a body_file the document points at but cannot read. It
// carries the path because the caller that named the file needs it in the
// diagnostic; the wrapped error carries the system's reason.
type BodyFileError struct {
	Path string
	Err  error
}

func (e *BodyFileError) Error() string {
	return "read body_file " + strconv.Quote(e.Path) + ": " + e.Err.Error()
}

func (e *BodyFileError) Unwrap() error { return e.Err }

// bodyValue is one representation as the document holds it: a field that is
// present and non-null, and the text it carries.
type bodyValue struct {
	field BodyField
	text  string
}

// selectBody is the rule's only implementation: among the slots in BodyFields
// order (nil means absent or null) at most one may be present. The zero value
// means the document is bodyless. The arity check keeps the vocabulary honest:
// a slot added to BodyFields without a place to read it fails here instead of
// being silently ignored.
func selectBody(slots []*string) (bodyValue, error) {
	if len(slots) != len(BodyFields) {
		panic("entry: selectBody received " + strconv.Itoa(len(slots)) + " slots for " + strconv.Itoa(len(BodyFields)) + " body fields")
	}
	present := make([]bodyValue, 0, len(slots))
	for i, slot := range slots {
		if slot == nil {
			continue
		}
		present = append(present, bodyValue{field: BodyFields[i], text: *slot})
	}
	switch len(present) {
	case 0:
		return bodyValue{}, nil
	case 1:
		return present[0], nil
	default:
		fields := make([]BodyField, len(present))
		for i, item := range present {
			fields[i] = item.field
		}
		return bodyValue{}, &MultipleBodyRepresentationsError{Fields: fields}
	}
}

// materialize resolves a selected representation to bytes. An empty body_file
// path names no file and therefore holds no bytes, the same way an empty body
// holds zero bytes.
func (v bodyValue) materialize() ([]byte, error) {
	switch v.field {
	case BodyFieldText:
		return []byte(v.text), nil
	case BodyFieldBase64:
		decoded, err := base64.StdEncoding.DecodeString(v.text)
		if err != nil {
			return nil, &BodyBase64Error{Err: err}
		}
		return decoded, nil
	case BodyFieldFile:
		if v.text == "" {
			return nil, nil
		}
		data, err := os.ReadFile(v.text)
		if err != nil {
			return nil, &BodyFileError{Path: v.text, Err: err}
		}
		return data, nil
	}
	return nil, nil
}

// BodyBytes resolves the document's single body representation to bytes. A
// document with no representation is a bodyless request; a document with more
// than one is a contract violation the caller must fix, not guess around.
//
// A document projected from a history record keeps that record's reference, so
// its body_file is interpreted as the record's own body: the recorded size and
// the file on disk must agree, and a disagreement is a diagnostic. A caller
// that named a path itself (BodyFromObject) still gets the diagnostic for a
// file it cannot read.
func (d *Document) BodyBytes() ([]byte, error) {
	selected, err := selectBody([]*string{d.Body, d.BodyBase64, d.BodyFile})
	if err != nil {
		return nil, err
	}
	if d.stored != nil && selected.field == BodyFieldFile && selected.text == d.stored.Path {
		data, _, _, err := d.stored.Bytes(0)
		return data, err
	}
	body, err := selected.materialize()
	if errors.Is(err, os.ErrNotExist) {
		// The path was not the record's own body (or there is no record), so a
		// vanishing file is the empty body the rest of the store treats it as.
		return nil, nil
	}
	return body, err
}

// StoredBodyRef returns the record reference this document was projected from,
// or nil for a document built in memory. A merge patch keeps the reference when
// the merged body_file still names it, which is how a replayed request reads
// its inherited body under the record's size claim instead of re-deciding what
// the path means.
func (d *Document) StoredBodyRef() *StoredBody {
	if d == nil {
		return nil
	}
	return d.stored
}

// BodyFromObject resolves the body of a request document held as a JSON
// object. That is both the shape a merge patch produces and the shape the
// published request-document schema describes, so input validation and the
// runtime selector read the same three fields through the same rule. Absence
// and explicit null mean "no representation"; a value of any other type than
// string is a contract violation.
func BodyFromObject(object map[string]any) ([]byte, error) {
	return BodyFromObjectWith(object, nil)
}

// BodyFromObjectWith is BodyFromObject with the record reference a base
// document carried. When the object's body_file still names that reference, the
// bytes are read as the record's own body — the recorded size, not a fresh
// guess from the path. Any other body_file is a path the caller named, and an
// unreadable one is that caller's diagnostic.
func BodyFromObjectWith(object map[string]any, stored *StoredBody) ([]byte, error) {
	slots := make([]*string, len(BodyFields))
	for i, field := range BodyFields {
		value, present := object[string(field)]
		if !present || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, &BodyFieldTypeError{Field: field}
		}
		slots[i] = &text
	}
	selected, err := selectBody(slots)
	if err != nil {
		return nil, err
	}
	if stored != nil && selected.field == BodyFieldFile && selected.text == stored.Path {
		data, _, _, err := stored.Bytes(0)
		return data, err
	}
	return selected.materialize()
}

// applyDocumentBody selects the inline representation for an entity body:
// UTF-8 text uses body, anything else uses body_base64, and no bytes uses
// neither.
func applyDocumentBody(document *Document, body []byte) {
	if len(body) == 0 {
		return
	}
	if utf8.Valid(body) {
		text := string(body)
		document.Body = &text
		return
	}
	encoded := base64.StdEncoding.EncodeToString(body)
	document.BodyBase64 = &encoded
}
