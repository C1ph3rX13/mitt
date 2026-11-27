package session

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func newRecorderStore(t *testing.T) *Store {
	t.Helper()
	return Open(t.TempDir())
}

func bodyFileNames(t *testing.T, store *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(store.BodiesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read bodies dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func historyObjects(t *testing.T, store *Store) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(store.HistoryPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	objects := []map[string]any{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			t.Fatalf("decode history line %q: %v", line, err)
		}
		objects = append(objects, object)
	}
	return objects
}

func readBodyFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read body %s: %v", path, err)
	}
	return string(data)
}

func TestRecorderCommitPublishesBodiesAndEvent(t *testing.T) {
	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{
		Kind:       KindCaptured,
		Started:    time.Now().Add(-5 * time.Millisecond),
		ClientAddr: "127.0.0.1:1234",
	})
	defer rec.Abort()

	event := rec.Event()
	request, err := rec.Request()
	if err != nil {
		t.Fatalf("request body: %v", err)
	}
	if _, err := request.Write([]byte("hello")); err != nil {
		t.Fatalf("write request body: %v", err)
	}
	response, err := rec.Response()
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	if _, err := response.Write([]byte("world")); err != nil {
		t.Fatalf("write response body: %v", err)
	}

	published := false
	if err := rec.Commit(completedResponse, nil, func(got *Event) {
		published = true
		if got != event {
			t.Errorf("publish event = %p, want %p", got, event)
		}
		if lines := historyObjects(t, store); len(lines) != 1 {
			t.Errorf("publish saw %d history lines, want 1", len(lines))
		}
		for _, direction := range []string{BodyRequest, BodyResponse} {
			if _, err := os.Stat(store.BodyPath(got.ID, direction)); err != nil {
				t.Errorf("publish ran before the %s body was published: %v", direction, err)
			}
		}
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !published {
		t.Fatal("publish was not called")
	}

	if event.ReqBodyID != event.ID || event.RespBodyID != event.ID {
		t.Errorf("body ids = (%q, %q), want both %q", event.ReqBodyID, event.RespBodyID, event.ID)
	}
	if event.ReqBodySize == nil || *event.ReqBodySize != 5 {
		t.Errorf("request body size = %v, want 5", event.ReqBodySize)
	}
	if event.RespBodySize == nil || *event.RespBodySize != 5 {
		t.Errorf("response body size = %v, want 5", event.RespBodySize)
	}
	if event.Timings == nil {
		t.Fatal("timings were not filled in")
	}
	if event.Context == nil || *event.Context != "" {
		t.Errorf("captured context = %v, want an explicit empty string", event.Context)
	}

	if got := readBodyFile(t, store.BodyPath(event.ID, BodyRequest)); got != "hello" {
		t.Errorf("request body = %q, want hello", got)
	}
	if got := readBodyFile(t, store.BodyPath(event.ID, BodyResponse)); got != "world" {
		t.Errorf("response body = %q, want world", got)
	}
	for _, name := range bodyFileNames(t, store) {
		if strings.HasSuffix(name, ".tmp") {
			t.Errorf("temporary body file survived commit: %s", name)
		}
	}

	lines := historyObjects(t, store)
	if len(lines) != 1 {
		t.Fatalf("history lines = %d, want 1", len(lines))
	}
	if lines[0]["kind"] != string(KindCaptured) || lines[0]["id"] != event.ID {
		t.Errorf("history line = %v, want captured event %s", lines[0], event.ID)
	}
	if _, ok := lines[0]["context"]; !ok {
		t.Error("captured history line must carry context")
	}

	if err := commitCompleted(rec); err == nil {
		t.Error("second commit succeeded, want an error")
	}
	rec.Abort()
	if got := readBodyFile(t, store.BodyPath(event.ID, BodyRequest)); got != "hello" {
		t.Errorf("abort after commit removed the request body: %q", got)
	}
}

func TestRecorderAbortLeavesNoTrace(t *testing.T) {
	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindSent})
	request, err := rec.Request()
	if err != nil {
		t.Fatalf("request body: %v", err)
	}
	if _, err := request.Write([]byte("hello")); err != nil {
		t.Fatalf("write request body: %v", err)
	}
	response, err := rec.Response()
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	if _, err := response.Write([]byte("world")); err != nil {
		t.Fatalf("write response body: %v", err)
	}

	rec.Abort()
	if names := bodyFileNames(t, store); len(names) != 0 {
		t.Errorf("abort left body files: %v", names)
	}
	if lines := historyObjects(t, store); len(lines) != 0 {
		t.Errorf("abort left history lines: %v", lines)
	}
	rec.Abort()
	if _, err := rec.Request(); err == nil {
		t.Error("body opened after abort, want an error")
	}
	if _, err := request.Write([]byte("x")); err == nil {
		t.Error("write after abort succeeded, want an error")
	}
}

func TestRecorderCommitFailureRollsBackPublishedBodies(t *testing.T) {
	store := newRecorderStore(t)
	if err := store.EnsureLayout(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := os.Mkdir(store.HistoryPath(), 0o700); err != nil {
		t.Fatalf("create history directory: %v", err)
	}

	rec := openExchange(t, store, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer rec.Abort()
	request, _ := rec.Request()
	_, _ = request.Write([]byte("hello"))
	response, _ := rec.Response()
	_, _ = response.Write([]byte("world"))

	published := false
	if err := rec.Commit(completedResponse, nil, func(*Event) { published = true }); err == nil {
		t.Fatal("commit succeeded with a history directory in the way")
	}
	if published {
		t.Error("publish ran for a failed commit")
	}
	if names := bodyFileNames(t, store); len(names) != 0 {
		t.Errorf("failed commit left body files: %v", names)
	}
}

func TestExchangeKindsMaterializeEmptyBodies(t *testing.T) {
	testCases := []struct {
		kind Kind
		spec ExchangeSpec
	}{
		{KindCaptured, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"}},
		{KindSent, ExchangeSpec{Kind: KindSent}},
		{KindReplayed, ExchangeSpec{Kind: KindReplayed, ParentID: "parent-id"}},
	}
	for _, testCase := range testCases {
		t.Run(string(testCase.kind), func(t *testing.T) {
			store := newRecorderStore(t)
			rec := openExchange(t, store, testCase.spec)
			defer rec.Abort()
			if err := commitCompleted(rec); err != nil {
				t.Fatalf("commit: %v", err)
			}
			event := rec.Event()
			if event.ReqBodyID != event.ID || event.RespBodyID != event.ID {
				t.Errorf("body ids = (%q, %q), want both %q", event.ReqBodyID, event.RespBodyID, event.ID)
			}
			if event.ReqBodySize == nil || *event.ReqBodySize != 0 {
				t.Errorf("request body size = %v, want 0", event.ReqBodySize)
			}
			if event.RespBodySize == nil || *event.RespBodySize != 0 {
				t.Errorf("response body size = %v, want 0", event.RespBodySize)
			}
			if event.Timings == nil {
				t.Error("exchange timings were not filled in")
			}
			for _, direction := range []string{BodyRequest, BodyResponse} {
				info, err := os.Stat(store.BodyPath(event.ID, direction))
				if err != nil {
					t.Fatalf("stat %s body: %v", direction, err)
				}
				if info.Size() != 0 {
					t.Errorf("%s body size = %d, want 0", direction, info.Size())
				}
			}

			lines := historyObjects(t, store)
			if len(lines) != 1 {
				t.Fatalf("history lines = %d, want 1", len(lines))
			}
			_, hasContext := lines[0]["context"]
			switch testCase.kind {
			case KindCaptured:
				if !hasContext {
					t.Error("captured line must carry an explicit context")
				}
			case KindSent, KindReplayed:
				if hasContext {
					t.Error("sent/replayed lines must omit context")
				}
			}
		})
	}
}

func TestRecorderMITMErrorHasNoExchangeBodiesOrTimings(t *testing.T) {
	store := newRecorderStore(t)
	rec, err := store.Begin(RecordSpec{
		Kind:       KindMITMError,
		Started:    time.Now(),
		ClientAddr: "127.0.0.1:1",
		TargetHost: "blocked.example",
		Reason:     "scope_denied",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer rec.Abort()
	rec.Event().Details = map[string]any{"matched": "deny"}
	if err := rec.Commit(nil); err != nil {
		t.Fatalf("commit: %v", err)
	}
	event := rec.Event()
	if event.ReqBodyID != "" || event.RespBodyID != "" {
		t.Errorf("mitm_error body ids = (%q, %q), want empty", event.ReqBodyID, event.RespBodyID)
	}
	if event.ReqBodySize != nil || event.RespBodySize != nil {
		t.Errorf("mitm_error body sizes = (%v, %v), want nil", event.ReqBodySize, event.RespBodySize)
	}
	if event.Timings != nil {
		t.Errorf("mitm_error timings = %v, want nil", event.Timings)
	}
	if names := bodyFileNames(t, store); len(names) != 0 {
		t.Errorf("mitm_error left body files: %v", names)
	}
	lines := historyObjects(t, store)
	if len(lines) != 1 {
		t.Fatalf("history lines = %d, want 1", len(lines))
	}
	if lines[0]["target_host"] != "blocked.example" || lines[0]["reason"] != "scope_denied" {
		t.Errorf("mitm_error line = %v, want target_host and reason", lines[0])
	}
	if _, ok := lines[0]["context"]; ok {
		t.Error("mitm_error line must omit context")
	}
}

func TestRecorderBodyOpenReadsTemporaryFileBeforeCommit(t *testing.T) {
	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindSent})
	defer rec.Abort()
	body, err := rec.Request()
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	if _, err := body.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if size, err := body.Size(); err != nil || size != 5 {
		t.Fatalf("size before commit = (%d, %v), want (5, nil)", size, err)
	}
	file, err := body.Open()
	if err != nil {
		t.Fatalf("open before commit: %v", err)
	}
	before, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		t.Fatalf("read before commit: %v", err)
	}
	if string(before) != "hello" {
		t.Errorf("pre-commit read = %q, want hello", before)
	}

	if err := commitCompleted(rec); err != nil {
		t.Fatalf("commit: %v", err)
	}
	file, err = body.Open()
	if err != nil {
		t.Fatalf("open after commit: %v", err)
	}
	after, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		t.Fatalf("read after commit: %v", err)
	}
	if string(after) != "hello" {
		t.Errorf("post-commit read = %q, want hello", after)
	}
}

func TestRecorderPublishesExtraBodyDirections(t *testing.T) {
	store := newRecorderStore(t)
	rec, err := store.Begin(RecordSpec{
		Kind:       KindMITMError,
		Started:    time.Now(),
		ClientAddr: "127.0.0.1:1",
		TargetHost: "example.test",
		Reason:     "h2_parse_error",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer rec.Abort()
	body, err := rec.Body("h2-client_read")
	if err != nil {
		t.Fatalf("extra body: %v", err)
	}
	if _, err := body.Write([]byte("raw frames")); err != nil {
		t.Fatalf("write extra body: %v", err)
	}
	if err := rec.Commit(nil); err != nil {
		t.Fatalf("commit: %v", err)
	}
	path := store.BodyPath(rec.Event().ID, "h2-client_read")
	if got := readBodyFile(t, path); got != "raw frames" {
		t.Errorf("extra body = %q, want raw frames", got)
	}
}

func TestRecorderRejectsInvalidSpecs(t *testing.T) {
	started := time.Now()
	testCases := []struct {
		name string
		spec RecordSpec
	}{
		{"unknown kind", RecordSpec{Kind: "unknown", Started: started}},
		{"missing start", RecordSpec{Kind: KindMITMError, ClientAddr: "x", TargetHost: "h", Reason: "r"}},
		{"captured without client", RecordSpec{Kind: KindCaptured, Started: started}},
		{"mitm without reason", RecordSpec{Kind: KindMITMError, Started: started, ClientAddr: "x", TargetHost: "h"}},
		{"mitm with parent", RecordSpec{Kind: KindMITMError, Started: started, ClientAddr: "x", TargetHost: "h", Reason: "r", ParentID: "p"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			store := newRecorderStore(t)
			if _, err := store.Begin(testCase.spec); err == nil {
				t.Fatal("Begin accepted an invalid spec")
			}
		})
	}
}

// TestRecorderRejectsInvalidBodyDirections keeps the diagnostic path (which may
// stage extra directions such as h2-parse taps) honest about direction names
// and about opening a direction twice.
func TestRecorderRejectsInvalidBodyDirections(t *testing.T) {
	store := newRecorderStore(t)
	rec, err := store.Begin(RecordSpec{
		Kind:       KindMITMError,
		Started:    time.Now(),
		ClientAddr: "127.0.0.1:1",
		TargetHost: "example.test",
		Reason:     "h2_parse_error",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer rec.Abort()
	for _, direction := range []string{"", "../escape", "a/b", "a.b"} {
		if _, err := rec.Body(direction); err == nil {
			t.Errorf("Body(%q) succeeded, want an error", direction)
		}
	}
	if _, err := rec.Body(BodyRequest); err != nil {
		t.Fatalf("first Body(req): %v", err)
	}
	if _, err := rec.Body(BodyRequest); err == nil {
		t.Error("duplicate Body(req) succeeded, want an error")
	}
}
