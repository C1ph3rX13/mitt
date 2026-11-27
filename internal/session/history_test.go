package session_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// writeHistory installs lines verbatim so a test can describe the exact store
// it wants, including damage that no writer would produce.
func writeHistory(t *testing.T, store *session.Store, lines ...string) {
	t.Helper()
	if err := store.EnsureLayout(); err != nil {
		t.Fatalf("create layout: %v", err)
	}
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(store.HistoryPath(), []byte(body), 0o600); err != nil {
		t.Fatalf("write history: %v", err)
	}
}

func writeBody(t *testing.T, store *session.Store, id, direction, content string) {
	t.Helper()
	if err := os.WriteFile(store.BodyPath(id, direction), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s body: %v", direction, err)
	}
}

const (
	capturedLine = `{"id":"entry-a","ts":"2025-01-02T03:04:05Z","kind":"captured","method":"GET","url":"http://example.test/a","status":200,"context":""}`
	errorLine    = `{"id":"entry-e","ts":"2025-01-02T03:04:06Z","kind":"mitm_error","target_host":"blocked.test","reason":"scope_denied","context":"ops"}`
)

func TestReadEntriesReturnsRequestedOrderAndBackfillsSizes(t *testing.T) {
	store := session.Open(t.TempDir())
	writeHistory(t, store, capturedLine, errorLine)
	writeBody(t, store, "entry-a", session.BodyRequest, "hello")
	writeBody(t, store, "entry-a", session.BodyResponse, "world!!")

	var diag bytes.Buffer
	events, err := store.ReadEntries([]string{"entry-e", "entry-a"}, &diag)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	if diag.Len() != 0 {
		t.Errorf("clean history produced diagnostics: %s", diag.String())
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].ID != "entry-e" || events[1].ID != "entry-a" {
		t.Errorf("order = %q, %q, want the requested order", events[0].ID, events[1].ID)
	}
	// Only the requested entries are backfilled; the mitm_error has no body
	// files, so its sizes resolve to zero rather than failing the read.
	if events[1].ReqBodySize == nil || *events[1].ReqBodySize != 5 {
		t.Errorf("request body size = %v, want 5", events[1].ReqBodySize)
	}
	if events[1].RespBodySize == nil || *events[1].RespBodySize != 7 {
		t.Errorf("response body size = %v, want 7", events[1].RespBodySize)
	}
	if events[0].ReqBodySize == nil || *events[0].ReqBodySize != 0 {
		t.Errorf("mitm_error request body size = %v, want 0", events[0].ReqBodySize)
	}
}

func TestReadEntriesDeduplicatesIds(t *testing.T) {
	store := session.Open(t.TempDir())
	writeHistory(t, store, capturedLine)

	events, err := store.ReadEntries([]string{"entry-a", "entry-a"}, nil)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	if len(events) != 2 || events[0].ID != "entry-a" || events[1].ID != "entry-a" {
		t.Errorf("events = %v, want the id repeated in order", events)
	}
	if events[0] == events[1] {
		t.Error("the two results share a pointer; each request must get its own copy")
	}
}

func TestReadEntriesMissingIdsReportEveryOne(t *testing.T) {
	store := session.Open(t.TempDir())
	writeHistory(t, store, capturedLine)

	_, err := store.ReadEntries([]string{"missing-a", "entry-a", "missing-b"}, nil)
	var notFound *session.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("error = %v, want *session.NotFoundError", err)
	}
	if strings.Join(notFound.IDs, ",") != "missing-a,missing-b" {
		t.Errorf("missing ids = %v, want the two absent ids in order", notFound.IDs)
	}
	if got := notFound.Error(); !strings.Contains(got, "missing-a") || !strings.Contains(got, "missing-b") {
		t.Errorf("message = %q, want both ids named", got)
	}
}

func TestReadEntriesReportsCorruptLinesAfterTheEntry(t *testing.T) {
	store := session.Open(t.TempDir())
	writeHistory(t, store,
		capturedLine,
		`{"id":"broken","ts":`,
		errorLine,
	)

	var diag bytes.Buffer
	events, err := store.ReadEntries([]string{"entry-a"}, &diag)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	if len(events) != 1 || events[0].ID != "entry-a" {
		t.Fatalf("events = %v, want entry-a", events)
	}
	// The walk covers the whole file: damage after the entry is still damage.
	if !strings.Contains(diag.String(), "line 2") {
		t.Errorf("diagnostics = %q, want the corrupt line 2", diag.String())
	}
}

func TestReadEntriesIgnoresTruncatedTail(t *testing.T) {
	store := session.Open(t.TempDir())
	writeHistory(t, store, capturedLine)
	// A crash between write and flush leaves a partial final line; it must not
	// be reported as corruption and must not hide the complete lines before it.
	file, err := os.OpenFile(store.HistoryPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	if _, err := file.WriteString(`{"id":"partial","kind":"capt`); err != nil {
		t.Fatalf("append partial line: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close history: %v", err)
	}

	var diag bytes.Buffer
	events, err := store.ReadEntries([]string{"entry-a"}, &diag)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("events = %d, want the one complete entry", len(events))
	}
	if diag.Len() != 0 {
		t.Errorf("diagnostics = %q, want the truncated tail ignored silently", diag.String())
	}
}

func TestReadEntriesEmptyStoreHasNoEntries(t *testing.T) {
	store := session.Open(t.TempDir())

	events, err := store.ReadEntries([]string{"anything"}, nil)
	if err == nil {
		t.Fatalf("events = %v, want a not-found error", events)
	}
	var notFound *session.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("error = %v, want *session.NotFoundError", err)
	}

	// No ids is not a lookup at all: it must not open history or report a
	// missing directory as an error.
	empty, err := store.ReadEntries(nil, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("ReadEntries(nil) = %v, %v; want an empty result", empty, err)
	}
}

func TestReadHistoryAndReadEntriesAgreeOnBodySizes(t *testing.T) {
	store := session.Open(t.TempDir())
	writeHistory(t, store, capturedLine)
	writeBody(t, store, "entry-a", session.BodyRequest, "1234")

	events, err := store.ReadHistory(nil)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	entries, err := store.ReadEntries([]string{"entry-a"}, nil)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	if *events[0].ReqBodySize != *entries[0].ReqBodySize {
		t.Errorf("body size differs between reads: %d vs %d", *events[0].ReqBodySize, *entries[0].ReqBodySize)
	}
}

func TestKindsIsTheOnlyKindVocabulary(t *testing.T) {
	if got := session.Kinds; !equalStrings(got, []string{"captured", "replayed", "sent", "mitm_error"}) {
		t.Errorf("Kinds = %v, want the contract order", got)
	}
	var kind session.Kind
	if !equalStrings(kind.EnumValues(), session.Kinds) {
		t.Errorf("Kind.EnumValues() = %v, want session.Kinds", kind.EnumValues())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHistoryPathIsInsideTheStore(t *testing.T) {
	dir := t.TempDir()
	store := session.Open(dir)
	if got, want := store.HistoryPath(), filepath.Join(dir, "history.jsonl"); got != want {
		t.Errorf("HistoryPath = %q, want %q", got, want)
	}
}
