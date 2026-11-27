package entry_test

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/entry"
)

// bodyCase is one document body in both shapes the rule serves: the typed
// Document a command reads off disk, and the JSON object a merge patch
// produces. Both must reach the same verdict, because `show --detail` and
// `send --patch` are two views of one rule.
type bodyCase struct {
	name string
	// slots holds the three representation pointers of a typed Document.
	slots [3]*string
	// object holds the same document as a JSON object; nil means the body
	// fields are absent entirely.
	object map[string]any
	// want is the materialized body; valid cases set it.
	want  []byte
	valid bool
}

func TestBodyRuleHasOneVerdictForTypedAndObjectDocuments(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "body.bin")
	if err := os.WriteFile(filePath, []byte("from file"), 0o600); err != nil {
		t.Fatalf("write body file: %v", err)
	}
	text := func(value string) *string { return &value }
	emptyPath := ""

	cases := []bodyCase{
		{
			name:   "no representation",
			object: map[string]any{},
			want:   nil,
			valid:  true,
		},
		{
			name:   "explicit null is absence",
			slots:  [3]*string{nil, nil, nil},
			object: map[string]any{"body": nil, "body_base64": nil, "body_file": nil},
			want:   nil,
			valid:  true,
		},
		{
			name:   "text body",
			slots:  [3]*string{text("hello"), nil, nil},
			object: map[string]any{"body": "hello"},
			want:   []byte("hello"),
			valid:  true,
		},
		{
			name:   "explicit empty text is a present, empty body",
			slots:  [3]*string{text(""), nil, nil},
			object: map[string]any{"body": ""},
			want:   []byte{},
			valid:  true,
		},
		{
			name:   "base64 body",
			slots:  [3]*string{nil, text(base64.StdEncoding.EncodeToString([]byte{0x00, 0xff})), nil},
			object: map[string]any{"body_base64": base64.StdEncoding.EncodeToString([]byte{0x00, 0xff})},
			want:   []byte{0x00, 0xff},
			valid:  true,
		},
		{
			name:   "file body",
			slots:  [3]*string{nil, nil, text(filePath)},
			object: map[string]any{"body_file": filePath},
			want:   []byte("from file"),
			valid:  true,
		},
		{
			name:   "empty body_file path names no file",
			slots:  [3]*string{nil, nil, &emptyPath},
			object: map[string]any{"body_file": ""},
			want:   nil,
			valid:  true,
		},
		{
			name:   "two representations",
			slots:  [3]*string{text("hello"), text("aGVsbG8="), nil},
			object: map[string]any{"body": "hello", "body_base64": "aGVsbG8="},
			valid:  false,
		},
		{
			name:   "three representations",
			slots:  [3]*string{text("hello"), text("aGVsbG8="), text(filePath)},
			object: map[string]any{"body": "hello", "body_base64": "aGVsbG8=", "body_file": filePath},
			valid:  false,
		},
		{
			name:   "invalid base64",
			slots:  [3]*string{nil, text("!!!"), nil},
			object: map[string]any{"body_base64": "!!!"},
			valid:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := &entry.Document{
				Method:     "POST",
				URL:        "http://example.test/",
				Body:       tc.slots[0],
				BodyBase64: tc.slots[1],
				BodyFile:   tc.slots[2],
			}
			typedBody, typedErr := document.BodyBytes()
			objectBody, objectErr := entry.BodyFromObject(tc.object)

			if tc.valid {
				if typedErr != nil {
					t.Fatalf("BodyBytes: %v", typedErr)
				}
				if objectErr != nil {
					t.Fatalf("BodyFromObject: %v", objectErr)
				}
				if string(typedBody) != string(tc.want) {
					t.Errorf("BodyBytes = %q, want %q", typedBody, tc.want)
				}
				if string(objectBody) != string(tc.want) {
					t.Errorf("BodyFromObject = %q, want %q", objectBody, tc.want)
				}
				return
			}
			if typedErr == nil {
				t.Fatal("BodyBytes accepted an invalid document")
			}
			if objectErr == nil {
				t.Fatal("BodyFromObject accepted an invalid document")
			}
			// The two entries must also classify the failure the same way, or
			// a caller translating one path would miss the other's error.
			if reflect.TypeOf(typedErr) != reflect.TypeOf(objectErr) {
				t.Errorf("error types differ: BodyBytes %T, BodyFromObject %T", typedErr, objectErr)
			}
		})
	}
}

// TestMissingBodyFileIsADiagnosticForTheCallerThatNamedIt pins the one place
// the two entry points deliberately differ: a stored document whose body file
// vanished reads as an empty body, because the store already treats it that
// way, while a caller that named the path in a patch is told the file is gone
// instead of silently sending nothing.
func TestMissingBodyFileIsADiagnosticForTheCallerThatNamedIt(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.bin")

	document := &entry.Document{Method: "POST", URL: "http://example.test/", BodyFile: &absent}
	if body, err := document.BodyBytes(); err != nil || len(body) != 0 {
		t.Errorf("BodyBytes = %q, %v; want an empty stored body", body, err)
	}

	_, err := entry.BodyFromObject(map[string]any{"body_file": absent})
	var fileErr *entry.BodyFileError
	if !errors.As(err, &fileErr) {
		t.Fatalf("BodyFromObject error = %v, want *entry.BodyFileError", err)
	}
	if fileErr.Path != absent {
		t.Errorf("BodyFileError.Path = %q, want %q", fileErr.Path, absent)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("BodyFileError must wrap the system error, got %v", err)
	}
}

// TestBodyFieldNamesMatchTheTaggedDocumentFields closes the loop between the
// two ways a body slot is declared: the BodyFields vocabulary the selector
// reads, and the at_most_one schema tags the published schema is derived from.
// Adding a slot to one without the other fails here, before a caller can see a
// document that one half accepts and the other rejects.
func TestBodyFieldNamesMatchTheTaggedDocumentFields(t *testing.T) {
	tagged := []string{}
	documentType := reflect.TypeOf(entry.Document{})
	for i := 0; i < documentType.NumField(); i++ {
		field := documentType.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		for _, rule := range strings.Split(field.Tag.Get("schema"), ",") {
			group, ok := strings.CutPrefix(strings.TrimSpace(rule), "at_most_one=")
			if !ok {
				continue
			}
			if group != "request-body" {
				t.Errorf("%s declares unexpected at_most_one group %q", field.Name, group)
			}
			tagged = append(tagged, name)
		}
	}
	if got := entry.BodyFieldNames(); !reflect.DeepEqual(tagged, got) {
		t.Fatalf("tagged document fields = %v, BodyFields = %v; they must be the same list in document order", tagged, got)
	}
}

// TestBodyFieldsAreTheDocumentSlots pins the vocabulary the published schema
// is compared against: the field names a caller sees are exactly the slots
// selectBody reads, in document order.
func TestBodyFieldsAreTheDocumentSlots(t *testing.T) {
	want := []string{"body", "body_base64", "body_file"}
	if got := entry.BodyFieldNames(); len(got) != len(want) {
		t.Fatalf("BodyFieldNames = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("BodyFieldNames = %v, want %v", got, want)
			}
		}
	}

	// A multiple-representation error names every present field in document
	// order, because that is the order a caller scans the document in.
	text := "x"
	document := &entry.Document{Body: &text, BodyBase64: &text, BodyFile: &text}
	_, err := document.BodyBytes()
	var multiple *entry.MultipleBodyRepresentationsError
	if !errors.As(err, &multiple) {
		t.Fatalf("error = %v, want *entry.MultipleBodyRepresentationsError", err)
	}
	if len(multiple.Fields) != len(want) {
		t.Fatalf("Fields = %v, want all three", multiple.Fields)
	}
	for i, field := range multiple.Fields {
		if string(field) != want[i] {
			t.Fatalf("Fields = %v, want document order %v", multiple.Fields, want)
		}
	}
}
