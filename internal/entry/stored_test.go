package entry_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// storedBodyFor builds the reference a history line gives one of its bodies, the
// way entry does when it reads a record. Tests use it to describe damage the
// writer never produces (a deleted or truncated body file).
func storedBodyFor(store *session.Store, event *session.Event, direction string) entry.StoredBody {
	request, response := entry.StoredBodiesOf(store, event)
	if direction == session.BodyRequest {
		return request
	}
	return response
}

// loadCaptured records one captured entry and returns its detail plus the store
// that holds it, so a test can damage a body file after the record exists.
func loadCaptured(t *testing.T, requestBody, responseBody string) (*session.Store, *entry.Detail) {
	t.Helper()
	store := newStore(t)
	id := recordCaptured(t, store, nil, requestBody, responseBody)
	detail, err := entry.Load(store, id, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return store, detail
}

// TestStoredBodyKeepsAGenuineEmptyBodyWorking pins the one legitimate
// "no bytes" case across every reader: a record that claims zero bytes reads as
// an empty body, whether its file is a zero-length file or already gone.
func TestStoredBodyKeepsAGenuineEmptyBodyWorking(t *testing.T) {
	store, detail := loadCaptured(t, "", "")
	requestPath := detail.RequestBodyPath
	if info, err := os.Stat(requestPath); err != nil || info.Size() != 0 {
		t.Fatalf("empty request body file = %v, %v; want a zero-length file", info, err)
	}

	for name, body := range map[string]entry.StoredBody{
		"zero-length file": detail.RequestBodyHandle(),
		"record with no file": {
			EntryID:   detail.ID,
			Direction: "request",
			Path:      filepath.Join(store.Dir(), "bodies", "absent-req.bin"),
		},
		"no body at all": {},
	} {
		t.Run(name, func(t *testing.T) {
			reader, size, err := body.Open()
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if size != 0 {
				t.Errorf("Open size = %d, want 0", size)
			}
			if reader != nil {
				// An existing zero-length file may still be streamed; it must
				// simply hold no bytes.
				data := make([]byte, 1)
				if n, err := reader.Read(data); n != 0 || !errors.Is(err, io.EOF) {
					t.Errorf("empty body read = %d, %v; want EOF", n, err)
				}
				reader.Close()
			}
			data, size, truncated, err := body.Bytes(8 << 10)
			if err != nil || len(data) != 0 || size != 0 || truncated {
				t.Errorf("Bytes = %q, %d, %v, %v; want an empty body", data, size, truncated, err)
			}
		})
	}

	// The response half of the same record reads as the empty body it is, so
	// "zero bytes" stays distinguishable from "bytes went missing".
	responseData, size, truncated, err := detail.Response().Body.Bytes(8 << 10)
	if err != nil || len(responseData) != 0 || size != 0 || truncated {
		t.Errorf("empty response body = %q, %d, %v, %v; want an empty body", responseData, size, truncated, err)
	}
}

// TestStoredBodyDiagnosesMissingAndShortBodies pins the report's acceptance
// case: a record that claims bytes must not read as empty when the file is gone
// or short. The diagnostic names the entry, the direction, and the path.
func TestStoredBodyDiagnosesMissingAndShortBodies(t *testing.T) {
	cases := map[string]struct {
		damage func(t *testing.T, path string)
		check  func(t *testing.T, err error)
	}{
		"missing file": {
			damage: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove body: %v", err)
				}
			},
			check: func(t *testing.T, err error) {
				var missing *entry.MissingBodyError
				if !errors.As(err, &missing) {
					t.Fatalf("error = %v, want *entry.MissingBodyError", err)
				}
				if missing.Size != 5 {
					t.Errorf("MissingBodyError.Size = %d, want the recorded 5", missing.Size)
				}
			},
		},
		"truncated file": {
			damage: func(t *testing.T, path string) {
				if err := os.Truncate(path, 2); err != nil {
					t.Fatalf("truncate body: %v", err)
				}
			},
			check: func(t *testing.T, err error) {
				var mismatch *entry.BodySizeMismatchError
				if !errors.As(err, &mismatch) {
					t.Fatalf("error = %v, want *entry.BodySizeMismatchError", err)
				}
				if mismatch.Recorded != 5 || mismatch.Actual != 2 {
					t.Errorf("recorded/actual = %d/%d, want 5/2", mismatch.Recorded, mismatch.Actual)
				}
			},
		},
		"grew past the record": {
			damage: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("hello, world"), 0o600); err != nil {
					t.Fatalf("rewrite body: %v", err)
				}
			},
			check: func(t *testing.T, err error) {
				var mismatch *entry.BodySizeMismatchError
				if !errors.As(err, &mismatch) {
					t.Fatalf("error = %v, want *entry.BodySizeMismatchError", err)
				}
				if mismatch.Recorded != 5 || mismatch.Actual != 12 {
					t.Errorf("recorded/actual = %d/%d, want 5/12", mismatch.Recorded, mismatch.Actual)
				}
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store, detail := loadCaptured(t, "hello", "ok")
			requestPath := detail.RequestBodyPath
			tc.damage(t, requestPath)

			// Every reader of the same record must reach the same verdict.
			_, _, openErr := detail.RequestBodyHandle().Open()
			tc.check(t, openErr)

			_, bodyErr := detail.RequestDocument.BodyBytes()
			tc.check(t, bodyErr)

			var raw bytes.Buffer
			tc.check(t, entry.WriteRaw(detail, &raw))
			if raw.Len() != 0 {
				t.Errorf("raw wrote %q before failing", raw.String())
			}

			// The response half is intact, so only the damaged direction is
			// diagnosed: the record still has a usable response.
			if _, size, err := detail.ResponseBodyHandle().Open(); err != nil || size != 2 {
				t.Errorf("intact response body = %d, %v; want 2 bytes", size, err)
			}

			// The command-level answer is one code with the record's identity.
			var appErr *output.AppError
			translated := entry.Translate(openErr, store.Dir())
			if !errors.As(translated, &appErr) || appErr.Code != output.CodeInternalError {
				t.Fatalf("translated error = %v, want INTERNAL_ERROR", translated)
			}
			if appErr.Details["direction"] != "request" || appErr.Details["id"] != detail.ID {
				t.Errorf("translated details = %v, want the record's id and direction", appErr.Details)
			}
			if appErr.Details["store"] != store.Dir() {
				t.Errorf("translated details.store = %v, want %s", appErr.Details["store"], store.Dir())
			}
		})
	}
}

// TestStoredBodyReadsAreBounded pins the resource bound on the bounded read:
// a comparison of one huge body must not load it whole. The fixture is sparse,
// so the file costs no disk; the bounded read costs only the limit.
func TestStoredBodyReadsAreBounded(t *testing.T) {
	const (
		limit = 64 << 10
		size  = 1 << 20
	)
	path := filepath.Join(t.TempDir(), "huge-resp.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	if err := file.Truncate(size); err != nil {
		file.Close()
		t.Fatalf("size body: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	body := entry.StoredBody{EntryID: "big", Direction: "response", Path: path, Size: size, SizeKnown: true}
	data, reported, truncated, err := body.Bytes(limit)
	if err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	if len(data) != limit || !truncated {
		t.Errorf("bounded read = %d bytes, truncated %v; want exactly %d and true", len(data), truncated, limit)
	}
	if reported != size {
		t.Errorf("reported size = %d, want the full %d", reported, size)
	}

	// An unbounded read is the replay path: it must still return the whole body
	// because the bytes it sends are the record's bytes.
	full, reported, truncated, err := body.Bytes(0)
	if err != nil {
		t.Fatalf("full read: %v", err)
	}
	if int64(len(full)) != size || truncated || reported != size {
		t.Errorf("full read = %d bytes (reported %d, truncated %v), want %d", len(full), reported, truncated, size)
	}
}

// TestDocumentOfKeepsAReferenceForADamagedBody pins the projection split the
// report asks for: listing and projection stay metadata-only, and a body that
// cannot be delivered is diagnosed when bytes are requested, not silently
// dropped from the document.
func TestDocumentOfKeepsAReferenceForADamagedBody(t *testing.T) {
	store, detail := loadCaptured(t, "hello", "ok")
	path := detail.RequestBodyPath
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove body: %v", err)
	}

	reloaded, err := entry.Load(store, detail.ID, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	document := reloaded.RequestDocument
	if document == nil || document.BodyFile == nil {
		t.Fatalf("document = %+v, want the record's body reference even though the file is gone", document)
	}
	if *document.BodyFile != path {
		t.Errorf("body_file = %q, want %q", *document.BodyFile, path)
	}
	if document.Body != nil || document.BodyBase64 != nil {
		t.Errorf("damaged stored body was inlined: %+v", document)
	}
	var missing *entry.MissingBodyError
	if _, err := document.BodyBytes(); !errors.As(err, &missing) {
		t.Errorf("BodyBytes error = %v, want *entry.MissingBodyError", err)
	}
}
