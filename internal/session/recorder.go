package session

import (
	"errors"
	"fmt"
	"time"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// RecordSpec is the identity of one record before its exchange is known. The
// fields a kind requires are validated by Begin; fields belonging to another
// kind are rejected so a copy-paste spec cannot silently produce a bad event.
type RecordSpec struct {
	Kind       Kind
	Started    time.Time
	ParentID   string
	ClientAddr string
	TargetHost string
	Reason     string
}

type recordState int

const (
	recordOpen recordState = iota
	recordCommitted
	recordAborted
)

// Recorder owns one diagnostic record's commit range: its event skeleton and
// the body files that become visible with that event. Exchange records
// (captured / replayed / sent) are begun with BeginExchange and committed
// through Exchange.Commit, which owns completeness and delivery; this type
// persists whatever bodies a diagnostic record staged.
type Recorder struct {
	store   *Store
	event   *Event
	started time.Time

	bodies      []*Body
	byDirection map[string]*Body
	state       recordState
}

// Begin opens a diagnostic record such as mitm_error. Exchange records —
// captured, replayed and sent — are begun with BeginExchange, because their
// commit conditions (identity, completed response, delivery) belong to
// Exchange rather than to the persistence step.
func (s *Store) Begin(spec RecordSpec) (*Recorder, error) {
	if isExchangeKind(spec.Kind) {
		return nil, fmt.Errorf("record kind %q is an exchange; use BeginExchange", spec.Kind)
	}
	return s.beginRecord(spec)
}

// beginRecord opens the record skeleton every kind shares. It is the one
// allocation and validation path for both diagnostic and exchange records.
func (s *Store) beginRecord(spec RecordSpec) (*Recorder, error) {
	if err := validateRecordSpec(spec); err != nil {
		return nil, err
	}
	if err := s.ensureLayout(); err != nil {
		return nil, err
	}
	id, err := newIDAt(spec.Started)
	if err != nil {
		return nil, err
	}
	event := &Event{
		ID:   id,
		TS:   spec.Started.UTC().Format(time.RFC3339Nano),
		Kind: spec.Kind,
	}
	switch spec.Kind {
	case KindCaptured:
		event.ClientAddr = spec.ClientAddr
		// captured records always carry an explicit empty context, matching
		// the long-standing history line shape.
		context := ""
		event.Context = &context
	case KindReplayed:
		event.ParentID = spec.ParentID
	case KindMITMError:
		event.ClientAddr = spec.ClientAddr
		event.TargetHost = spec.TargetHost
		event.Reason = spec.Reason
	}
	return &Recorder{
		store:       s,
		event:       event,
		started:     spec.Started,
		byDirection: map[string]*Body{},
	}, nil
}

// Event is the record's event document.
func (r *Recorder) Event() *Event {
	if r == nil {
		return nil
	}
	return r.event
}

// Body opens one body file for the record. The file stays temporary until
// Commit. A direction can be opened once.
func (r *Recorder) Body(direction string) (*Body, error) {
	if r == nil {
		return nil, errors.New("recorder is nil")
	}
	if r.state != recordOpen {
		return nil, errors.New("record is already closed")
	}
	return r.openBody(direction)
}

func (r *Recorder) openBody(direction string) (*Body, error) {
	if !validBodyDirection(direction) {
		return nil, fmt.Errorf("invalid body direction %q", direction)
	}
	if _, ok := r.byDirection[direction]; ok {
		return nil, fmt.Errorf("body direction %q is already open", direction)
	}
	stage, err := atomicfile.NewStage(r.store.BodyPath(r.event.ID, direction), atomicfile.Private)
	if err != nil {
		return nil, fmt.Errorf("create body file: %w", err)
	}
	body := &Body{stage: stage}
	r.bodies = append(r.bodies, body)
	r.byDirection[direction] = body
	return body, nil
}

func (r *Recorder) ensureBody(direction string) (*Body, error) {
	if body, ok := r.byDirection[direction]; ok {
		return body, nil
	}
	return r.openBody(direction)
}

// commitState reports whether the record can still be committed. Exchange
// Commit checks it before doing completeness work, so a second commit fails
// without touching the published files.
func (r *Recorder) commitState() error {
	if r == nil {
		return errors.New("recorder is nil")
	}
	if r.state != recordOpen {
		return errors.New("record is already closed")
	}
	return nil
}

// Commit persists a diagnostic record: it publishes every staged body, appends
// the history line, and then calls publish (when non-nil). Exchange records
// must go through Exchange.Commit, which owns the pending-derivation and
// delivery rules this method must not duplicate.
func (r *Recorder) Commit(publish func(*Event)) error {
	if err := r.commitState(); err != nil {
		return err
	}
	if isExchangeKind(r.event.Kind) {
		return fmt.Errorf("record kind %q is an exchange; use Exchange.Commit", r.event.Kind)
	}
	return r.commit(publish)
}

// commit runs the publication and append sequence. A failure before the line is
// visible rolls the whole record back, including body files already renamed
// into place. A failure after the line landed keeps the bodies: the record is
// readable by id, so removing its bytes would leave a visible line pointing at
// nothing. See lineOutcome. publish is best-effort and runs only for a durable
// commit.
func (r *Recorder) commit(publish func(*Event)) error {
	for _, body := range r.bodies {
		if err := body.seal(); err != nil {
			r.abort()
			return err
		}
	}
	for _, body := range r.bodies {
		if err := body.publish(); err != nil {
			r.abort()
			return err
		}
	}

	outcome := r.store.history().appendEvent(r.event)
	if outcome.Err != nil {
		if outcome.abandonBodies() {
			r.abort()
		} else {
			// The line is in the log, so the record is visible. Keep the
			// bodies it names and report the durability failure; Abort must
			// not remove a record a reader can find.
			r.state = recordCommitted
		}
		return outcome.Err
	}
	r.state = recordCommitted
	if publish != nil {
		publish(r.event)
	}
	return nil
}

// Abort discards every body file. It is safe after Commit and safe to call
// more than once.
func (r *Recorder) Abort() {
	if r == nil || r.state != recordOpen {
		return
	}
	r.abort()
}

func (r *Recorder) abort() {
	for _, body := range r.bodies {
		body.discard()
	}
	r.state = recordAborted
}

func validBodyDirection(direction string) bool {
	if direction == "" {
		return false
	}
	for _, char := range direction {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '_' || char == '-':
		default:
			return false
		}
	}
	return true
}

func validateRecordSpec(spec RecordSpec) error {
	if spec.Started.IsZero() {
		return errors.New("record start time is required")
	}
	switch spec.Kind {
	case KindCaptured:
		if spec.ClientAddr == "" {
			return errors.New("captured record requires a client address")
		}
		if spec.ParentID != "" || spec.TargetHost != "" || spec.Reason != "" {
			return errors.New("captured record does not accept replay or MITM fields")
		}
	case KindSent:
		if spec.ParentID != "" || spec.ClientAddr != "" || spec.TargetHost != "" || spec.Reason != "" {
			return errors.New("sent record does not accept replay, client, or MITM fields")
		}
	case KindReplayed:
		if spec.ParentID == "" {
			return errors.New("replayed record requires a parent id")
		}
		if spec.ClientAddr != "" || spec.TargetHost != "" || spec.Reason != "" {
			return errors.New("replayed record does not accept client or MITM fields")
		}
	case KindMITMError:
		if spec.ClientAddr == "" || spec.TargetHost == "" || spec.Reason == "" {
			return errors.New("mitm_error record requires client address, target host, and reason")
		}
		if spec.ParentID != "" {
			return errors.New("mitm_error record does not accept a parent id")
		}
	default:
		return fmt.Errorf("unknown record kind %q", spec.Kind)
	}
	return nil
}
