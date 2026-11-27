package session

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// completedResponse is the upstream half every committable exchange test uses.
// A record needs a request identity, a status inside the recorded range, and a
// status line before Commit will write it.
var completedResponse = ResponseResult{
	Status:  200,
	Line:    "HTTP/1.1 200 OK",
	Headers: [][2]string{{"Content-Type", "text/plain"}},
}

// newExchange opens an exchange in its own temporary store.
func newExchange(t *testing.T, spec ExchangeSpec) *Exchange {
	t.Helper()
	return openExchange(t, newRecorderStore(t), spec)
}

// openExchange opens an exchange in store with the identity every real path
// applies before its response is read. Tests that need an unidentifiable
// exchange can still call store.BeginExchange directly.
func openExchange(t *testing.T, store *Store, spec ExchangeSpec) *Exchange {
	t.Helper()
	if spec.Started.IsZero() {
		spec.Started = time.Now()
	}
	exchange, err := store.BeginExchange(spec)
	if err != nil {
		t.Fatalf("begin exchange: %v", err)
	}
	exchange.IdentifyRequest(RequestIdentity{
		Method:               "GET",
		URL:                  "http://example.test/",
		HTTPVersion:          "http/1.1",
		RequestLine:          "GET / HTTP/1.1",
		ForwardedRequestLine: "GET / HTTP/1.1",
	})
	return exchange
}

// commitCompleted commits an identified exchange with a 200 and no delivery
// failure.
func commitCompleted(exchange *Exchange) error {
	return exchange.Commit(completedResponse, nil, nil)
}

func TestExchangeCommitsBothBodiesAndTheEvent(t *testing.T) {
	exchange := newExchange(t, ExchangeSpec{Kind: KindSent})
	defer exchange.Abort()

	request, err := exchange.Request()
	if err != nil {
		t.Fatalf("request body: %v", err)
	}
	if _, err := request.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatalf("write request body: %v", err)
	}
	response, err := exchange.Response()
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	if _, err := response.Write([]byte("hello")); err != nil {
		t.Fatalf("write response body: %v", err)
	}

	var published *Event
	if err := exchange.Commit(completedResponse, nil, func(event *Event) { published = event }); err != nil {
		t.Fatalf("commit: %v", err)
	}

	event := exchange.Event()
	if published != event {
		t.Error("publish did not receive the committed event")
	}
	if event.Status != completedResponse.Status || event.ResponseLine != completedResponse.Line {
		t.Errorf("status/line = %d/%q, want the committed result", event.Status, event.ResponseLine)
	}
	if len(event.ResponseHeaders) != len(completedResponse.Headers) {
		t.Errorf("response headers = %v, want the committed result", event.ResponseHeaders)
	}
	if event.RespBodySize == nil || *event.RespBodySize != 5 {
		t.Errorf("response body size = %v, want 5", event.RespBodySize)
	}
	if got := readBodyFile(t, exchange.Store().ResponseBodyPath(event)); got != "hello" {
		t.Errorf("committed response body = %q, want hello", got)
	}
	lines := historyObjects(t, exchange.Store())
	if len(lines) != 1 {
		t.Fatalf("history lines = %d, want 1", len(lines))
	}
}

// TestExchangeCommitsAZeroByteResponse pins the other side of the completeness
// rule: a completed exchange whose bodies are both empty is still a complete
// exchange and commits with two zero-length body files.
func TestExchangeCommitsAZeroByteResponse(t *testing.T) {
	exchange := newExchange(t, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer exchange.Abort()

	if err := exchange.Commit(ResponseResult{Status: 204, Line: "HTTP/1.1 204 No Content"}, nil, nil); err != nil {
		t.Fatalf("commit empty exchange: %v", err)
	}

	event := exchange.Event()
	if event.ReqBodySize == nil || *event.ReqBodySize != 0 || event.RespBodySize == nil || *event.RespBodySize != 0 {
		t.Errorf("body sizes = %v/%v, want two explicit zeroes", event.ReqBodySize, event.RespBodySize)
	}
	for _, direction := range []string{BodyRequest, BodyResponse} {
		info, err := os.Stat(exchange.Store().BodyPath(event.ID, direction))
		if err != nil {
			t.Fatalf("stat %s body: %v", direction, err)
		}
		if info.Size() != 0 {
			t.Errorf("%s body size = %d, want 0", direction, info.Size())
		}
	}
	if lines := historyObjects(t, exchange.Store()); len(lines) != 1 {
		t.Fatalf("history lines = %d, want 1", len(lines))
	}
}

// TestExchangeCommitRequiresIdentity pins the first commit condition: an
// exchange that never learned its request is not a record, so Commit refuses it
// and rolls the staged bodies back instead of writing a line with no method or
// URL.
func TestExchangeCommitRequiresIdentity(t *testing.T) {
	store := newRecorderStore(t)
	exchange, err := store.BeginExchange(ExchangeSpec{Kind: KindSent, Started: time.Now()})
	if err != nil {
		t.Fatalf("begin exchange: %v", err)
	}
	defer exchange.Abort()
	request, _ := exchange.Request()
	_, _ = request.Write([]byte("hello"))

	err = commitCompleted(exchange)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("commit without identity = %v, want an identity error", err)
	}
	if names := bodyFileNames(t, store); len(names) != 0 {
		t.Errorf("refused commit left body files: %v", names)
	}
	if got := historyObjects(t, store); len(got) != 0 {
		t.Errorf("refused commit left history lines: %v", got)
	}
}

// TestExchangeCommitRequiresACompletedResponse pins the second commit
// condition: an exchange with no finished upstream half cannot become a record
// by filling in empty body files, which is what a status of zero means.
func TestExchangeCommitRequiresACompletedResponse(t *testing.T) {
	testCases := []struct {
		name   string
		result ResponseResult
		want   string
	}{
		{"zero status", ResponseResult{Line: "HTTP/1.1 200 OK"}, "status 0"},
		{"status above the recorded range", ResponseResult{Status: 1000, Line: "HTTP/1.1 1000 X"}, "status 1000"},
		{"missing status line", ResponseResult{Status: 200}, "status line"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			exchange := newExchange(t, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
			defer exchange.Abort()
			response, _ := exchange.Response()
			_, _ = response.Write([]byte("bytes nobody described"))

			err := exchange.Commit(testCase.result, nil, nil)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("commit = %v, want an error containing %q", err, testCase.want)
			}
			if names := bodyFileNames(t, exchange.Store()); len(names) != 0 {
				t.Errorf("refused commit left body files: %v", names)
			}
			if got := historyObjects(t, exchange.Store()); len(got) != 0 {
				t.Errorf("refused commit left history lines: %v", got)
			}
		})
	}
}

// TestExchangeCommitRefusesUndeliveredResponse pins the third commit condition:
// when the adapter observed that the client never received the completed
// response, the exchange is not a complete transfer. The record and its bodies
// must disappear, and the caller can tell a delivery failure from a storage
// one.
func TestExchangeCommitRefusesUndeliveredResponse(t *testing.T) {
	store := newRecorderStore(t)
	exchange := openExchange(t, store, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer exchange.Abort()
	request, _ := exchange.Request()
	_, _ = request.Write([]byte("hello"))
	response, _ := exchange.Response()
	_, _ = response.Write([]byte("world"))

	delivery := errors.New("write tcp: connection reset by peer")
	err := exchange.Commit(completedResponse, delivery, nil)
	var deliveryErr *DeliveryError
	if !errors.As(err, &deliveryErr) {
		t.Fatalf("commit = %v, want a *DeliveryError", err)
	}
	if !errors.Is(err, delivery) {
		t.Errorf("delivery error does not wrap the observed failure: %v", err)
	}
	if names := bodyFileNames(t, store); len(names) != 0 {
		t.Errorf("undelivered exchange left body files: %v", names)
	}
	if got := historyObjects(t, store); len(got) != 0 {
		t.Errorf("undelivered exchange left history lines: %v", got)
	}
	if _, err := store.ReadEntries([]string{exchange.Event().ID}, nil); err == nil {
		t.Error("undelivered exchange is readable by id")
	}
}

func TestExchangeResponseReaderSurvivesCommit(t *testing.T) {
	exchange := newExchange(t, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer exchange.Abort()

	response, err := exchange.Response()
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	if _, err := response.Write([]byte("body-bytes")); err != nil {
		t.Fatalf("write response body: %v", err)
	}

	// The adapter opens this handle before delivery so the bytes it hands the
	// client cannot depend on the commit succeeding.
	reader, size, err := exchange.ResponseReader()
	if err != nil {
		t.Fatalf("response reader: %v", err)
	}
	defer reader.Close()
	if size != int64(len("body-bytes")) {
		t.Fatalf("response size = %d, want %d", size, len("body-bytes"))
	}

	if err := commitCompleted(exchange); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read through the pre-commit handle: %v", err)
	}
	if string(got) != "body-bytes" {
		t.Fatalf("response body = %q, want body-bytes", got)
	}
}

func TestExchangeResponseReaderSurvivesFailedCommit(t *testing.T) {
	exchange := newExchange(t, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer exchange.Abort()

	response, err := exchange.Response()
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	if _, err := response.Write([]byte("still-deliverable")); err != nil {
		t.Fatalf("write response body: %v", err)
	}

	reader, _, err := exchange.ResponseReader()
	if err != nil {
		t.Fatalf("response reader: %v", err)
	}
	defer reader.Close()

	// A history directory in the way makes the append fail after the bodies
	// were renamed, which is the worst case: committed files that then roll
	// back.
	if err := os.Mkdir(exchange.Store().HistoryPath(), 0o700); err != nil {
		t.Fatalf("create history directory: %v", err)
	}
	if err := commitCompleted(exchange); err == nil {
		t.Fatal("commit succeeded with a history directory in the way")
	}

	// The record is gone from disk, but a client already waiting for this
	// response must still get it.
	if names := bodyFileNames(t, exchange.Store()); len(names) != 0 {
		t.Errorf("failed commit left body files: %v", names)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read through the pre-commit handle: %v", err)
	}
	if string(got) != "still-deliverable" {
		t.Fatalf("response body = %q, want still-deliverable", got)
	}
}

func TestExchangeAbortLeavesNoTrace(t *testing.T) {
	exchange := newExchange(t, ExchangeSpec{Kind: KindReplayed, ParentID: "parent"})
	request, err := exchange.Request()
	if err != nil {
		t.Fatalf("request body: %v", err)
	}
	if _, err := request.Write([]byte("staged")); err != nil {
		t.Fatalf("write request body: %v", err)
	}
	exchange.Abort()

	if names := bodyFileNames(t, exchange.Store()); len(names) != 0 {
		t.Errorf("abort left body files: %v", names)
	}
	if lines := historyObjects(t, exchange.Store()); len(lines) != 0 {
		t.Errorf("abort wrote history lines: %v", lines)
	}
}

func TestBeginExchangeRejectsDiagnosticKinds(t *testing.T) {
	store := newRecorderStore(t)
	for _, kind := range []Kind{KindMITMError, "", "unknown"} {
		if _, err := store.BeginExchange(ExchangeSpec{Kind: kind, Started: time.Now()}); err == nil {
			t.Errorf("BeginExchange(%q) succeeded, want an error", kind)
		}
	}
}

func TestBeginExchangeKeepsRecordValidation(t *testing.T) {
	store := newRecorderStore(t)
	// A sent record must not carry a client address; the exchange must not
	// quietly accept fields that belong to another kind.
	if _, err := store.BeginExchange(ExchangeSpec{
		Kind:       KindSent,
		Started:    time.Now(),
		ClientAddr: "127.0.0.1:1",
	}); err == nil {
		t.Fatal("BeginExchange accepted a client address on a sent record")
	}
}

// TestBeginRefusesExchangeKinds pins the one way into a record: exchange kinds
// go through BeginExchange, because only that path enforces identity,
// completeness, and delivery at commit. A diagnostic Begin must not produce a
// recorder whose Commit skips those rules.
func TestBeginRefusesExchangeKinds(t *testing.T) {
	store := newRecorderStore(t)
	for _, kind := range []Kind{KindCaptured, KindSent, KindReplayed} {
		spec := RecordSpec{Kind: kind, Started: time.Now()}
		switch kind {
		case KindCaptured:
			spec.ClientAddr = "127.0.0.1:1"
		case KindReplayed:
			spec.ParentID = "parent"
		}
		if _, err := store.Begin(spec); err == nil {
			t.Errorf("Begin(%q) succeeded, want a pointer to BeginExchange", kind)
		}
	}
}
