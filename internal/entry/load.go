package entry

import (
	"errors"
	"io"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// Load reads one entry and returns its detailed projection. diag receives the
// store's corrupt-line warnings, so a damaged history is visible to every
// caller instead of being swallowed by the ones that pass nil.
func Load(store *session.Store, id string, diag io.Writer) (*Detail, error) {
	events, err := store.ReadEntries([]string{id}, diag)
	if err != nil {
		return nil, wrapRead(err)
	}
	return buildDetail(store, events[0]), nil
}

// LoadMany reads several entries in one scan and returns their detailed
// projections in the requested order.
func LoadMany(store *session.Store, ids []string, diag io.Writer) ([]*Detail, error) {
	events, err := store.ReadEntries(ids, diag)
	if err != nil {
		return nil, wrapRead(err)
	}
	details := make([]*Detail, 0, len(events))
	for _, event := range events {
		details = append(details, buildDetail(store, event))
	}
	return details, nil
}

// wrapRead tags a store read failure as a ReadError while leaving a not-found
// result type intact: the two are different answers and Translate needs to
// tell them apart.
func wrapRead(err error) error {
	var notFound *session.NotFoundError
	if errors.As(err, &notFound) {
		return err
	}
	return &ReadError{Err: err}
}

// DetailOf projects an event the caller already holds — such as the entry
// `send` just wrote — onto the detailed shape. It reads no history: callers
// that have the event must not pay for, or race with, a second scan.
func DetailOf(store *session.Store, event *session.Event) *Detail {
	return buildDetail(store, event)
}

func buildDetail(store *session.Store, event *session.Event) *Detail {
	detail := &Detail{View: Summarize(event)}
	detail.requestBody, detail.responseBody = StoredBodiesOf(store, event)
	if event.Kind != session.KindMITMError {
		detail.RequestBodyPath = detail.requestBody.Path
		detail.ResponseBodyPath = detail.responseBody.Path
	}
	detail.RequestDocument = documentOf(event, detail.requestBody)
	return detail
}

// Filter narrows a history listing to one kind and one context. Empty fields
// match everything, which is what `mitt history` with no flags asks for.
type Filter struct {
	Kind    string
	Context string
}

// Matches reports whether view passes the filter. The context comparison is
// exact: X-Repeater-Context is an opaque label, not a pattern.
func (f Filter) Matches(view *View) bool {
	if f.Kind != "" && string(view.Kind) != f.Kind {
		return false
	}
	if f.Context != "" && view.Context != f.Context {
		return false
	}
	return true
}

// List returns the listing projection of every entry, filtered. It is the
// history command's whole read: one scan, one projection, and no second copy
// of the kind vocabulary or the nil handling.
func List(store *session.Store, filter Filter, diag io.Writer) ([]ListRow, error) {
	events, err := store.ReadHistory(diag)
	if err != nil {
		return nil, wrapRead(err)
	}
	rows := make([]ListRow, 0, len(events))
	for i := range events {
		view := Summarize(&events[i])
		if filter.Matches(&view) {
			rows = append(rows, view.ListRow())
		}
	}
	return rows, nil
}
