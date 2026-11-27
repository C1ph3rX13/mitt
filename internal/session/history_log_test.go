package session

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// appendTail is a test append implementation that writes like the protocol but
// fails at a chosen point. A regular file cannot be made to fail between a
// write and its sync portably, and those are exactly the two halves of "the
// line landed" that the record commit has to tell apart.
type appendTail int

const (
	appendOK appendTail = iota
	// appendFailureBeforeWrite fails before any byte of the line reaches the
	// file: the disk is full before the record exists.
	appendFailureBeforeWrite
	// appendFailureAfterWrite fails after the complete line is in the file but
	// before it is forced: the record is visible but not durable.
	appendFailureAfterWrite
)

// installAppend arm the chosen outcome for the rest of the test and restores the
// real protocol afterwards. Tests using it must not run in parallel.
func installAppend(t *testing.T, tail appendTail) {
	t.Helper()
	real := appendLine
	t.Cleanup(func() { appendLine = real })
	appendLine = func(path string, policy atomicfile.Policy, write func(*os.File) error) error {
		return atomicfile.Append(path, policy, func(file *os.File) error {
			switch tail {
			case appendFailureBeforeWrite:
				return errors.New("injected: write failed")
			}
			if err := write(file); err != nil {
				return err
			}
			if tail == appendFailureAfterWrite {
				return errors.New("injected: sync failed after the line landed")
			}
			return nil
		})
	}
}

func readHistoryBytes(t *testing.T, store *Store) string {
	t.Helper()
	data, err := os.ReadFile(store.HistoryPath())
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	return string(data)
}

// TestCommitAfterTruncatedTailIsReadable is the report's acceptance case: a
// crash left a half line, and the next commit must produce a record that can be
// read back by id together with both of its bodies, without damaging the record
// before the fragment.
func TestCommitAfterTruncatedTailIsReadable(t *testing.T) {
	store := newRecorderStore(t)
	if err := store.EnsureLayout(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	earlier := `{"id":"earlier","ts":"2025-01-02T03:04:05Z","kind":"mitm_error","target_host":"x.test","reason":"scope_denied"}`
	if err := os.WriteFile(store.HistoryPath(), []byte(earlier+"\n"+`{"id":"partial","kind":"capt`), 0o600); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	rec := openExchange(t, store, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer rec.Abort()
	request, _ := rec.Request()
	_, _ = request.Write([]byte("hello"))
	response, _ := rec.Response()
	_, _ = response.Write([]byte("world"))
	if err := commitCompleted(rec); err != nil {
		t.Fatalf("commit after a truncated tail: %v", err)
	}

	// The fragment is gone, so neither record joined it into a corrupt line.
	if got := readHistoryBytes(t, store); strings.Contains(got, `"partial"`) {
		t.Errorf("truncated tail survived the append: %q", got)
	}

	var diag strings.Builder
	events, err := store.ReadEntries([]string{"earlier", rec.Event().ID}, &diag)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	if diag.Len() != 0 {
		t.Errorf("clean history produced diagnostics: %s", diag.String())
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want the earlier record and the new one", len(events))
	}
	if events[0].ID != "earlier" {
		t.Errorf("earlier record = %q, want it still readable", events[0].ID)
	}
	if events[1].ReqBodySize == nil || *events[1].ReqBodySize != 5 {
		t.Errorf("request body size = %v, want 5", events[1].ReqBodySize)
	}
	if events[1].RespBodySize == nil || *events[1].RespBodySize != 5 {
		t.Errorf("response body size = %v, want 5", events[1].RespBodySize)
	}
	if got := readBodyFile(t, store.RequestBodyPath(events[1])); got != "hello" {
		t.Errorf("request body = %q, want hello", got)
	}
	if got := readBodyFile(t, store.ResponseBodyPath(events[1])); got != "world" {
		t.Errorf("response body = %q, want world", got)
	}
}

// TestCommitRepairsATailLargerThanOneRead pins the tail scan's chunking: a
// fragment bigger than the read window must still be removed whole, not left
// as a prefix that the next line could join.
func TestCommitRepairsATailLargerThanOneRead(t *testing.T) {
	store := newRecorderStore(t)
	if err := store.EnsureLayout(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	fragment := strings.Repeat("x", 3*tailRepairChunk)
	if err := os.WriteFile(store.HistoryPath(), []byte(fragment), 0o600); err != nil {
		t.Fatalf("seed fragment: %v", err)
	}

	rec := openExchange(t, store, ExchangeSpec{Kind: KindSent})
	defer rec.Abort()
	if err := commitCompleted(rec); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The record's own fields can contain a stray "x" (a hostname, say), so
	// look for the seeded run rather than the character.
	data := readHistoryBytes(t, store)
	if strings.Contains(data, strings.Repeat("x", 8)) {
		t.Errorf("tail fragment survived: %d bytes of the x run remain", strings.Count(data, "x"))
	}
	if len(data) >= tailRepairChunk {
		t.Errorf("history is %d bytes after one append, want the fragment gone", len(data))
	}
	lines := historyObjects(t, store)
	if len(lines) != 1 || lines[0]["id"] != rec.Event().ID {
		t.Fatalf("history = %v, want only the new record", lines)
	}
}

// TestCommitBeforeTheLineLandsRollsBack is the report's first I/O failure: the
// write fails before any byte of the line is in the log, so the record must
// disappear completely — no line, no bodies.
func TestCommitBeforeTheLineLandsRollsBack(t *testing.T) {
	installAppend(t, appendFailureBeforeWrite)

	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer rec.Abort()
	request, _ := rec.Request()
	_, _ = request.Write([]byte("hello"))
	response, _ := rec.Response()
	_, _ = response.Write([]byte("world"))

	if err := commitCompleted(rec); err == nil {
		t.Fatal("commit succeeded although the line never landed")
	}
	if names := bodyFileNames(t, store); len(names) != 0 {
		t.Errorf("failed commit left body files: %v", names)
	}
	if got := readHistoryBytes(t, store); strings.Contains(got, rec.Event().ID) {
		t.Errorf("failed commit left a history line: %q", got)
	}
	if _, err := store.ReadEntries([]string{rec.Event().ID}, nil); err == nil {
		t.Error("rolled-back record is readable by id")
	}
}

// TestCommitAfterTheLineLandsKeepsBodies is the report's second I/O failure:
// the line is complete in the log but the forcing step fails. The record is
// visible, so removing its bodies would hand a reader a line whose bytes are
// gone. The commit reports the durability failure and keeps the record.
func TestCommitAfterTheLineLandsKeepsBodies(t *testing.T) {
	installAppend(t, appendFailureAfterWrite)

	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer rec.Abort() // Abort after a landed line must not delete the record.
	request, _ := rec.Request()
	_, _ = request.Write([]byte("hello"))
	response, _ := rec.Response()
	_, _ = response.Write([]byte("world"))

	err := commitCompleted(rec)
	if err == nil {
		t.Fatal("commit reported success although the line was never forced")
	}
	if !strings.Contains(err.Error(), "sync") {
		t.Errorf("commit error = %v, want the forcing failure named", err)
	}

	var diag strings.Builder
	events, readErr := store.ReadEntries([]string{rec.Event().ID}, &diag)
	if readErr != nil {
		t.Fatalf("visible record is not readable by id: %v", readErr)
	}
	if diag.Len() != 0 {
		t.Errorf("clean history produced diagnostics: %s", diag.String())
	}
	if events[0].ReqBodySize == nil || *events[0].ReqBodySize != 5 {
		t.Errorf("request body size = %v, want 5", events[0].ReqBodySize)
	}
	if events[0].RespBodySize == nil || *events[0].RespBodySize != 5 {
		t.Errorf("response body size = %v, want 5", events[0].RespBodySize)
	}
	if names := bodyFileNames(t, store); len(names) != 2 {
		t.Errorf("body files = %v, want both kept beside the visible line", names)
	}
	if got := readBodyFile(t, store.RequestBodyPath(events[0])); got != "hello" {
		t.Errorf("request body = %q, want hello", got)
	}
	if got := readBodyFile(t, store.ResponseBodyPath(events[0])); got != "world" {
		t.Errorf("response body = %q, want world", got)
	}
}

// TestCommitSecondAttemptAfterALandedFailureIsRejected pins that a record whose
// line is already in the log cannot be committed again: the retry would append
// a second line for one id.
func TestCommitSecondAttemptAfterALandedFailureIsRejected(t *testing.T) {
	installAppend(t, appendFailureAfterWrite)

	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindSent})
	defer rec.Abort()
	if err := commitCompleted(rec); err == nil {
		t.Fatal("commit succeeded although the line was never forced")
	}
	if err := commitCompleted(rec); err == nil {
		t.Error("second commit succeeded after the line already landed")
	}
	if lines := historyObjects(t, store); len(lines) != 1 {
		t.Errorf("history lines = %d, want exactly one line for the record", len(lines))
	}
}

// TestAppendLineSerializesWithTailRepair pins that the tail repair happens
// inside the same lock as the write: two records committed at once each land as
// their own complete line, and neither loses the other.
func TestAppendLineSerializesWithTailRepair(t *testing.T) {
	store := newRecorderStore(t)
	if err := store.EnsureLayout(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := os.WriteFile(store.HistoryPath(), []byte(`{"id":"fragment"`), 0o600); err != nil {
		t.Fatalf("seed fragment: %v", err)
	}

	type result struct {
		id  string
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rec, err := store.BeginExchange(ExchangeSpec{Kind: KindSent, Started: time.Now()})
			if err != nil {
				results <- result{err: err}
				return
			}
			defer rec.Abort()
			rec.IdentifyRequest(RequestIdentity{Method: "GET", URL: "http://example.test/"})
			results <- result{id: rec.Event().ID, err: commitCompleted(rec)}
		}()
	}

	ids := make([]string, 0, 2)
	deadline := time.After(30 * time.Second)
	for len(ids) < 2 {
		select {
		case res := <-results:
			if res.err != nil {
				t.Fatalf("commit: %v", res.err)
			}
			ids = append(ids, res.id)
		case <-deadline:
			t.Fatal("concurrent commits did not finish within 30s; a writer lock is stuck")
		}
	}

	lines := historyObjects(t, store)
	if len(lines) != 2 {
		t.Fatalf("history lines = %d, want one per record", len(lines))
	}
	for _, id := range ids {
		if _, err := store.ReadEntries([]string{id}, nil); err != nil {
			t.Errorf("record %s is not readable: %v", id, err)
		}
	}
	if got := readHistoryBytes(t, store); strings.Contains(got, `"fragment"`) {
		t.Errorf("tail fragment survived concurrent appends: %q", got)
	}
}
