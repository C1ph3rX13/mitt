package entry

import (
	"errors"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// Translate maps an entry package error onto a command error. Every command
// that addresses a history id calls it, so one missing id produces one code,
// one message, one hint, and one set of details no matter which command asked.
// Errors that do not belong to this package are returned unchanged.
func Translate(err error, store string) error {
	var notFound *session.NotFoundError
	if errors.As(err, &notFound) {
		appErr := output.NewError(
			output.CodeEntryNotFound,
			notFound.Error(),
			"run `mitt history` to list valid entry ids",
		)
		if len(notFound.IDs) == 1 {
			appErr.WithDetail("id", notFound.IDs[0])
		} else {
			appErr.WithDetail("ids", notFound.IDs)
		}
		return appErr.WithDetail("store", store)
	}

	var noRaw *NoRawBytesError
	if errors.As(err, &noRaw) {
		return output.NewError(
			output.CodeInvalidRequest,
			noRaw.Error(),
			"mitm_error entries only carry failure metadata; use `mitt show <id>` without --raw",
		).WithDetail("id", noRaw.ID).WithDetail("kind", string(noRaw.Kind))
	}

	var readErr *ReadError
	if errors.As(err, &readErr) {
		return output.NewError(
			output.CodeInternalError,
			readErr.Error(),
			"check permissions on the data directory",
		).WithDetail("store", store)
	}

	// A body the record claims but the disk cannot deliver is damage to the data
	// directory. Every command that reads stored bytes reports it the same way:
	// the entry id, the direction, and the path the record points at.
	var bodyErr StoredBodyDiagnostic
	if errors.As(err, &bodyErr) {
		entryID, direction, path := bodyErr.Evidence()
		return output.NewError(
			output.CodeInternalError,
			bodyErr.Error(),
			"the history entry records body bytes its files no longer hold; inspect the data directory",
		).WithDetail("id", entryID).
			WithDetail("direction", direction).
			WithDetail("path", path).
			WithDetail("store", store)
	}

	return err
}

// ReadError wraps a history read failure so a command can name the data
// directory without inspecting the wrapped message.
type ReadError struct {
	Err error
}

func (e *ReadError) Error() string { return e.Err.Error() }
func (e *ReadError) Unwrap() error { return e.Err }
