package entry_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// newStore returns an empty store plus a way to record one captured entry.
func newStore(t *testing.T) *session.Store {
	t.Helper()
	return session.Open(t.TempDir())
}

// recordCaptured writes one captured entry with an explicit identity and
// bodies, returning its id. It goes through the real recorder so the stored
// line is whatever the product writes, not a fixture.
func recordCaptured(t *testing.T, store *session.Store, fields func(*session.Event), requestBody, responseBody string) string {
	t.Helper()
	rec, err := store.BeginExchange(session.ExchangeSpec{
		Kind:       session.KindCaptured,
		Started:    time.Now(),
		ClientAddr: "127.0.0.1:1234",
	})
	if err != nil {
		t.Fatalf("begin record: %v", err)
	}
	defer rec.Abort()

	rec.IdentifyRequest(session.RequestIdentity{
		Method:               "POST",
		URL:                  "http://example.test/base?q=1",
		HTTPVersion:          "http/1.1",
		RequestLine:          "POST http://example.test/base?q=1 HTTP/1.1",
		ForwardedRequestLine: "POST /base?q=1 HTTP/1.1",
	})
	event := rec.Event()
	event.RequestHeaders = [][2]string{
		{"Host", "example.test"},
		{"Content-Length", "5"},
	}
	if fields != nil {
		fields(event)
	}

	for direction, body := range map[string]string{
		session.BodyRequest:  requestBody,
		session.BodyResponse: responseBody,
	} {
		if body == "" {
			continue
		}
		var staged *session.Body
		switch direction {
		case session.BodyRequest:
			staged, err = rec.Request()
		case session.BodyResponse:
			staged, err = rec.Response()
		}
		if err != nil {
			t.Fatalf("stage %s body: %v", direction, err)
		}
		if _, err := staged.Write([]byte(body)); err != nil {
			t.Fatalf("write %s body: %v", direction, err)
		}
	}
	completed := session.ResponseResult{
		Status:  201,
		Line:    "HTTP/1.1 201 Created",
		Headers: [][2]string{{"Content-Length", "2"}},
	}
	if err := rec.Commit(completed, nil, nil); err != nil {
		t.Fatalf("commit record: %v", err)
	}
	return event.ID
}

func TestLoadReturnsSummaryAndDetailFromOneRead(t *testing.T) {
	store := newStore(t)
	id := recordCaptured(t, store, nil, "hello", "ok")

	var diag bytes.Buffer
	detail, err := entry.Load(store, id, &diag)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if diag.Len() != 0 {
		t.Errorf("clean history produced diagnostics: %s", diag.String())
	}

	if detail.ID != id || detail.Kind != session.KindCaptured {
		t.Errorf("identity = %q/%q, want %s/captured", detail.ID, detail.Kind, id)
	}
	if detail.Status == nil || *detail.Status != 201 {
		t.Errorf("status = %v, want 201", detail.Status)
	}
	if detail.ReqBodySize != 5 || detail.RespBodySize != 2 {
		t.Errorf("body sizes = %d/%d, want 5/2", detail.ReqBodySize, detail.RespBodySize)
	}
	if detail.RequestLine != "POST http://example.test/base?q=1 HTTP/1.1" {
		t.Errorf("request_line = %q, want the received line", detail.RequestLine)
	}
	if detail.ForwardedRequestLine != "POST /base?q=1 HTTP/1.1" {
		t.Errorf("forwarded_request_line = %q, want the line the next hop received", detail.ForwardedRequestLine)
	}
	if want := store.BodyPath(id, session.BodyRequest); detail.RequestBodyPath != want {
		t.Errorf("request_body_path = %q, want %q", detail.RequestBodyPath, want)
	}
	if detail.ResponseBodyPath == "" {
		t.Error("response_body_path is empty for a captured entry")
	}
	if detail.RequestDocument == nil {
		t.Fatal("request_document is nil for a captured entry")
	}
	if detail.RequestDocument.BodyFile == nil {
		t.Fatalf("document body_file = nil, want the stored body path: %+v", detail.RequestDocument)
	}
	if body, err := detail.RequestDocument.BodyBytes(); err != nil || string(body) != "hello" {
		t.Errorf("document body = %q, %v; want hello", body, err)
	}
	if detail.RequestDocument.Body != nil || detail.RequestDocument.BodyBase64 != nil {
		t.Errorf("a stored entry must not inline its body: %+v", detail.RequestDocument)
	}
}

// TestLoadNamesAStoredBodyWithoutReadingIt pins the projection rule: a record
// that claims bytes keeps its body_file reference even when the file is gone,
// and nothing is decided about readability until bytes are asked for.
func TestLoadNamesAStoredBodyWithoutReadingIt(t *testing.T) {
	store := newStore(t)
	id := recordCaptured(t, store, nil, "hello", "ok")
	requestPath := store.RequestBodyPath(&session.Event{ID: id})
	if err := os.Remove(requestPath); err != nil {
		t.Fatalf("remove request body: %v", err)
	}

	detail, err := entry.Load(store, id, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	document := detail.RequestDocument
	if document == nil || document.BodyFile == nil {
		t.Fatalf("document = %+v, want the record's body reference", document)
	}
	if *document.BodyFile != requestPath {
		t.Errorf("document body_file = %q, want %q", *document.BodyFile, requestPath)
	}
	if _, err := document.BodyBytes(); err == nil {
		t.Fatal("BodyBytes invented bytes for a record whose body file is gone")
	}
}

func TestLoadMissingIDReturnsNotFound(t *testing.T) {
	store := newStore(t)

	_, err := entry.Load(store, "absent", nil)
	var notFound *session.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("error = %v, want *session.NotFoundError", err)
	}

	translated := entry.Translate(err, store.Dir())
	var appErr *output.AppError
	if !errors.As(translated, &appErr) {
		t.Fatalf("translated error = %v, want *output.AppError", translated)
	}
	if appErr.Code != output.CodeEntryNotFound {
		t.Errorf("code = %s, want %s", appErr.Code, output.CodeEntryNotFound)
	}
	if appErr.Details["store"] != store.Dir() {
		t.Errorf("details.store = %v, want %s", appErr.Details["store"], store.Dir())
	}
	if appErr.Details["id"] != "absent" {
		t.Errorf("details.id = %v, want absent", appErr.Details["id"])
	}
}

// TestTranslateIsTheOnlyNotFoundAnswer pins that two commands asking for two
// different ids through different entry calls produce the same error shape.
func TestTranslateIsTheOnlyNotFoundAnswer(t *testing.T) {
	store := newStore(t)
	recordCaptured(t, store, nil, "", "")

	_, errSingle := entry.Load(store, "missing-one", nil)
	_, errMany := entry.LoadMany(store, []string{"missing-one", "missing-two"}, nil)

	single := entry.Translate(errSingle, store.Dir()).(*output.AppError)
	many := entry.Translate(errMany, store.Dir()).(*output.AppError)
	if single.Code != many.Code {
		t.Errorf("codes differ: %s vs %s", single.Code, many.Code)
	}
	if single.Hint != many.Hint {
		t.Errorf("hints differ: %q vs %q", single.Hint, many.Hint)
	}
	ids, _ := many.Details["ids"].([]string)
	if strings.Join(ids, ",") != "missing-one,missing-two" {
		t.Errorf("details.ids = %v, want both ids", many.Details["ids"])
	}
}

func TestListFiltersWithoutASecondVocabulary(t *testing.T) {
	store := newStore(t)
	capturedID := recordCaptured(t, store, func(event *session.Event) {
		label := "alpha"
		event.Context = &label
	}, "", "")

	var diag bytes.Buffer
	all, err := entry.List(store, entry.Filter{}, &diag)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].ID != capturedID {
		t.Fatalf("list = %v, want the captured entry", all)
	}
	if all[0].Context != "alpha" || all[0].Kind != session.KindCaptured {
		t.Errorf("row = %+v, want the summary values", all[0])
	}

	byKind, err := entry.List(store, entry.Filter{Kind: "mitm_error"}, nil)
	if err != nil {
		t.Fatalf("list by kind: %v", err)
	}
	if len(byKind) != 0 {
		t.Errorf("kind filter returned %v, want nothing", byKind)
	}

	byContext, err := entry.List(store, entry.Filter{Context: "alpha"}, nil)
	if err != nil {
		t.Fatalf("list by context: %v", err)
	}
	if len(byContext) != 1 {
		t.Errorf("context filter returned %d rows, want 1", len(byContext))
	}

	// The context comparison is exact, not a substring or a pattern.
	partial, err := entry.List(store, entry.Filter{Context: "alph"}, nil)
	if err != nil {
		t.Fatalf("list by partial context: %v", err)
	}
	if len(partial) != 0 {
		t.Errorf("partial context matched %d rows, want 0", len(partial))
	}
}

func TestWriteRawUsesTheForwardedRequestLine(t *testing.T) {
	store := newStore(t)
	id := recordCaptured(t, store, nil, "hello", "ok")

	detail, err := entry.Load(store, id, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var raw bytes.Buffer
	if err := entry.WriteRaw(detail, &raw); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	if !strings.HasPrefix(raw.String(), "POST /base?q=1 HTTP/1.1\r\n") {
		t.Errorf("raw starts with %q, want the forwarded origin-form line", raw.String())
	}
	if !strings.Contains(raw.String(), "hello") || !strings.HasSuffix(raw.String(), "ok") {
		t.Errorf("raw = %q, want the request body then the response", raw.String())
	}
}

func TestWriteRawRejectsMITMError(t *testing.T) {
	store := newStore(t)
	// A mitm_error entry has nothing to reconstruct; record one directly.
	rec, err := store.Begin(session.RecordSpec{
		Kind:       session.KindMITMError,
		Started:    time.Now(),
		ClientAddr: "127.0.0.1:1234",
		TargetHost: "blocked.test",
		Reason:     "scope_denied",
	})
	if err != nil {
		t.Fatalf("begin mitm_error: %v", err)
	}
	if err := rec.Commit(nil); err != nil {
		t.Fatalf("commit mitm_error: %v", err)
	}

	detail, err := entry.Load(store, rec.Event().ID, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if detail.RequestDocument != nil {
		t.Errorf("mitm_error request_document = %+v, want nil", detail.RequestDocument)
	}
	if detail.HasResponse() {
		t.Error("mitm_error must not claim a response")
	}
	if detail.RequestBodyPath != "" || detail.ResponseBodyPath != "" {
		t.Errorf("mitm_error body paths = %q/%q, want empty", detail.RequestBodyPath, detail.ResponseBodyPath)
	}

	var raw bytes.Buffer
	err = entry.WriteRaw(detail, &raw)
	var noRaw *entry.NoRawBytesError
	if !errors.As(err, &noRaw) {
		t.Fatalf("error = %v, want *entry.NoRawBytesError", err)
	}
	if raw.Len() != 0 {
		t.Errorf("raw wrote %q before failing", raw.String())
	}

	translated := entry.Translate(err, store.Dir())
	var appErr *output.AppError
	if !errors.As(translated, &appErr) || appErr.Code != output.CodeInvalidRequest {
		t.Errorf("translated error = %v, want INVALID_REQUEST", translated)
	}
}

func TestDocumentBodyBytesRejectsMultipleRepresentations(t *testing.T) {
	text := "hello"
	encoded := base64.StdEncoding.EncodeToString([]byte("hello"))
	document := &entry.Document{Method: "POST", URL: "http://example.test/", Body: &text, BodyBase64: &encoded}
	if _, err := document.BodyBytes(); err == nil {
		t.Fatal("BodyBytes accepted two body representations")
	}
}

// TestDocumentBodyBytesOutsideARecordTreatsAMissingFileAsEmpty pins the
// boundary of the stored-body rule. A document that was not projected from a
// history record has no size claim to check a path against, so a caller that
// built one by hand keeps the lenient read; a document that came off disk
// carries its record's reference and diagnoses the same missing file instead
// (see TestStoredBodyDiagnosesMissingAndShortBodies).
func TestDocumentBodyBytesOutsideARecordTreatsAMissingFileAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.bin")
	document := &entry.Document{Method: "POST", URL: "http://example.test/", BodyFile: &path}
	body, err := document.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty for a missing file", body)
	}
}

func TestNewDocumentInlinesOnlyRenderedBodies(t *testing.T) {
	target, err := url.Parse("http://example.test/a?x=1")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	for name, testCase := range map[string]struct {
		body     []byte
		wantText *string
	}{
		"empty":  {body: nil},
		"text":   {body: []byte("hello"), wantText: ptr("hello")},
		"binary": {body: []byte{0, 1, 0xff}},
	} {
		t.Run(name, func(t *testing.T) {
			document := entry.NewDocument("POST", target, [][2]string{{"Host", "example.test"}}, testCase.body)
			if len(testCase.body) == 0 {
				if document.Body != nil || document.BodyBase64 != nil || document.BodyFile != nil {
					t.Errorf("bodyless document = %+v, want no representation", document)
				}
				return
			}
			if testCase.wantText != nil {
				if document.Body == nil || *document.Body != *testCase.wantText {
					t.Errorf("body = %v, want %q", document.Body, *testCase.wantText)
				}
				if document.BodyBase64 != nil {
					t.Error("text body must not use body_base64")
				}
				return
			}
			if document.BodyBase64 == nil {
				t.Fatalf("binary body = %+v, want body_base64", document)
			}
			decoded, err := base64.StdEncoding.DecodeString(*document.BodyBase64)
			if err != nil || string(decoded) != string(testCase.body) {
				t.Errorf("body_base64 decodes to %q, %v; want the input bytes", decoded, err)
			}
		})
	}
}

func TestSummarizeUsesOneNilShape(t *testing.T) {
	// A zero status and a nil context are the two fields with disk-level
	// sentinels; the view must expose one answer for each.
	view := entry.Summarize(&session.Event{ID: "a", Kind: session.KindSent})
	if view.Status != nil {
		t.Errorf("status = %v, want nil", view.Status)
	}
	if view.Context != "" {
		t.Errorf("context = %q, want empty", view.Context)
	}
	if view.RequestHeaders == nil || view.ResponseHeaders == nil {
		t.Error("header slices must be non-nil so JSON emits [] not null")
	}
	if view.ReqBodySize != 0 || view.RespBodySize != 0 {
		t.Errorf("body sizes = %d/%d, want 0", view.ReqBodySize, view.RespBodySize)
	}

	zero := 0
	withZeroStatus := entry.Summarize(&session.Event{ID: "b", Kind: session.KindSent, Status: zero})
	if withZeroStatus.Status != nil {
		t.Errorf("status = %v for a stored 0, want nil", withZeroStatus.Status)
	}
}

func ptr(value string) *string { return &value }
