package session

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// ExchangeSpec identifies one request/response exchange being recorded.
// Exchange records are the captured, replayed, and sent kinds; diagnostic
// records such as mitm_error use RecordSpec directly.
type ExchangeSpec struct {
	Kind       Kind
	Started    time.Time
	ParentID   string
	ClientAddr string
}

// ResponseResult is the completed upstream half of an exchange: the status, the
// status line, and the header set the origin sent. Adapters hand it to Commit
// once the response has been read whole, so "which fields make a finished
// exchange" has one writer instead of three call sites assigning them by hand.
type ResponseResult struct {
	Status  int
	Line    string
	Headers [][2]string
}

// DeliveryError reports that an exchange was rolled back because the completed
// response never reached the client. The wrapped error is the write or flush
// failure the adapter observed.
type DeliveryError struct {
	Err error
}

func (e *DeliveryError) Error() string {
	return "client did not receive the response: " + e.Err.Error()
}

func (e *DeliveryError) Unwrap() error { return e.Err }

// Exchange is one request/response pair as it is being recorded. It owns the
// order every capture path must get right:
//
//  1. stage the request body while the request goes upstream,
//  2. stage the response body as it arrives,
//  3. apply the request identity,
//  4. deliver the completed response and report what the client observed, and
//  5. commit the record with the completed response and the delivery result.
//
// The three paths that record an exchange — the HTTP/1 tunnel, an HTTP/2
// stream, and `send` — differ only in how bytes move and who receives them.
// Keeping the order and the commit eligibility here means URL rules, body
// staging, and the readable-after-commit guarantee cannot drift between them.
//
// An Exchange either commits whole or leaves no trace. Abort, a failed stage, a
// failed commit, an exchange that never completed, and a delivery the client
// did not receive all leave no final body file and no history line. That is the
// ADR-0003 promise, enforced once instead of three times.
type Exchange struct {
	store      *Store
	rec        *Recorder
	identified bool
}

// BeginExchange opens an exchange record. The returned Exchange must be
// committed or aborted; callers normally use `defer exchange.Abort()`.
func (s *Store) BeginExchange(spec ExchangeSpec) (*Exchange, error) {
	if !isExchangeKind(spec.Kind) {
		return nil, fmt.Errorf("exchange record kind %q is not an exchange", spec.Kind)
	}
	rec, err := s.beginRecord(RecordSpec{
		Kind:       spec.Kind,
		Started:    spec.Started,
		ParentID:   spec.ParentID,
		ClientAddr: spec.ClientAddr,
	})
	if err != nil {
		return nil, err
	}
	return &Exchange{store: s, rec: rec}, nil
}

// Event is the record's event document. Callers apply the request identity
// through IdentifyRequest and the finished response through Commit; Commit
// fills the derived fields: body ids, body sizes, and total_ms measured from
// the record's start.
func (e *Exchange) Event() *Event {
	if e == nil || e.rec == nil {
		return nil
	}
	return e.rec.Event()
}

// Request returns the request body to stage. Bytes written here become the
// request body file that commits with the record.
func (e *Exchange) Request() (*Body, error) {
	if e == nil || e.rec == nil {
		return nil, errors.New("exchange is nil")
	}
	return e.rec.Body(BodyRequest)
}

// Response returns the response body to stage.
func (e *Exchange) Response() (*Body, error) {
	if e == nil || e.rec == nil {
		return nil, errors.New("exchange is nil")
	}
	return e.rec.Body(BodyResponse)
}

// ResponseReader returns a reader over the staged response body together with
// its size. The adapter delivers the completed response through this handle,
// then hands the delivery result to Commit. Delivery therefore does not depend
// on the commit succeeding: a storage failure can take the record away, but not
// bytes the client has already received.
func (e *Exchange) ResponseReader() (io.ReadCloser, int64, error) {
	if e == nil || e.rec == nil {
		return nil, 0, errors.New("exchange is nil")
	}
	response, err := e.rec.ensureBody(BodyResponse)
	if err != nil {
		return nil, 0, err
	}
	file, err := response.Open()
	if err != nil {
		return nil, 0, fmt.Errorf("open response body: %w", err)
	}
	size, err := response.Size()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, size, nil
}

// Commit is the one place that decides whether an exchange may become a
// record. result is the upstream half the adapter finished reading; delivery is
// what became of those bytes on their way to the client — nil when the whole
// response reached every party waiting for it. `send` has no waiting party, so
// it passes nil.
//
// An exchange commits only when all three hold:
//
//   - the request identity was applied through IdentifyRequest,
//   - result describes a completed response, and
//   - delivery observed no failure.
//
// Otherwise every staged body is discarded and no history line is written. That
// keeps a record from claiming an exchange the client never received, and stops
// a record with no completed response from being committed by filling in empty
// body files.
//
// A persistence failure after a successful delivery follows the history log's
// visibility rule: a line that landed keeps its bodies, a line that did not
// rolls back. The caller's error is the durability failure either way; publish
// runs only for a durable commit.
func (e *Exchange) Commit(result ResponseResult, delivery error, publish func(*Event)) error {
	if e == nil || e.rec == nil {
		return errors.New("exchange is nil")
	}
	if err := e.rec.commitState(); err != nil {
		return err
	}
	if err := e.checkComplete(result); err != nil {
		e.rec.abort()
		return err
	}
	if delivery != nil {
		e.rec.abort()
		return &DeliveryError{Err: delivery}
	}

	event := e.rec.event
	event.Status = result.Status
	event.ResponseLine = result.Line
	event.ResponseHeaders = result.Headers

	// A complete exchange always has both directions on disk, even when they
	// stay empty, so a reader never has to distinguish "absent" from
	// "zero-byte".
	if _, err := e.rec.ensureBody(BodyRequest); err != nil {
		e.rec.abort()
		return err
	}
	if _, err := e.rec.ensureBody(BodyResponse); err != nil {
		e.rec.abort()
		return err
	}
	event.ReqBodyID = event.ID
	event.RespBodyID = event.ID

	// The staged files already hold every written byte; measuring before the
	// publication step does not change the size the rename makes visible.
	requestSize, err := e.rec.byDirection[BodyRequest].Size()
	if err != nil {
		e.rec.abort()
		return err
	}
	responseSize, err := e.rec.byDirection[BodyResponse].Size()
	if err != nil {
		e.rec.abort()
		return err
	}
	event.ReqBodySize = &requestSize
	event.RespBodySize = &responseSize

	// TotalMS measures the exchange from the record's start, so every path
	// reports the same phase with the same origin. A caller may pre-fill
	// Timings to add phases of its own; only this field is owned here.
	if event.Timings == nil {
		event.Timings = &Timings{}
	}
	event.Timings.TotalMS = time.Since(e.rec.started).Milliseconds()

	return e.rec.commit(publish)
}

// checkComplete rejects an exchange whose identity or response half is missing.
// These are caller mistakes, not runtime conditions every adapter has to check:
// the three recording paths all reach Commit only after reading a whole
// response, so a failure here means a path forgot a step.
func (e *Exchange) checkComplete(result ResponseResult) error {
	if !e.identified {
		return errors.New("exchange has no request identity; call IdentifyRequest before Commit")
	}
	if result.Status < 100 || result.Status > 999 {
		return fmt.Errorf("response status %d is outside the recorded range 100-999", result.Status)
	}
	if result.Line == "" {
		return errors.New("response status line is required")
	}
	return nil
}

// Abort discards every body. It is safe after Commit and safe to call more
// than once.
func (e *Exchange) Abort() {
	if e == nil || e.rec == nil {
		return
	}
	e.rec.Abort()
}

// Store is the data directory this exchange writes to.
func (e *Exchange) Store() *Store {
	if e == nil {
		return nil
	}
	return e.store
}

func isExchangeKind(kind Kind) bool {
	switch kind {
	case KindCaptured, KindSent, KindReplayed:
		return true
	default:
		return false
	}
}
