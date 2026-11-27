package entry

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// Directions named in stored-body diagnostics. A record has exactly two bodies,
// and naming the direction lets a caller act on a diagnostic without reading the
// history line again.
const (
	directionRequest  = "request"
	directionResponse = "response"
)

// StoredBody is one body a history line points at: the path its bytes live at
// and the number of bytes the record claims. Projections never stat or open a
// body; when bytes are needed they come from this value, so the record's size
// and the disk's content are compared in one place instead of in every reader.
//
// A record that claims bytes must find them, unchanged in size, or reading is a
// diagnostic. A zero-size record reads as an empty body even when its file is
// gone: a zero-length body holds no bytes either way, and `exchange` entries
// from before every exchange published both files have no file to point at.
type StoredBody struct {
	EntryID   string
	Direction string
	Path      string
	// Size is the byte count the record carries and SizeKnown reports whether
	// it carried one at all. The history log backfills a missing size from the
	// file, so even a legacy line has a usable claim: the file's size when it
	// exists, zero when it does not.
	Size      int64
	SizeKnown bool
}

// Claimed reports whether the record vouches for at least one byte.
func (b StoredBody) Claimed() bool {
	return b.SizeKnown && b.Size > 0
}

// Present reports whether this value names a body at all. mitm_error entries
// carry no request or response body, so both of their handles are absent.
func (b StoredBody) Present() bool {
	return b.Path != ""
}

// Open returns the body for streaming together with the number of bytes it
// holds. When it returns no error, that size is both what the record claims and
// what the file contains. A bodyless handle, and a record that claims no bytes
// whose file is absent, return (nil, 0, nil) because both hold no bytes.
func (b StoredBody) Open() (io.ReadCloser, int64, error) {
	if !b.Present() {
		return nil, 0, nil
	}

	file, err := os.Open(b.Path)
	if errors.Is(err, fs.ErrNotExist) {
		if !b.Claimed() {
			return nil, 0, nil
		}
		return nil, 0, &MissingBodyError{
			EntryID:   b.EntryID,
			Direction: b.Direction,
			Path:      b.Path,
			Size:      b.Size,
		}
	}
	if err != nil {
		return nil, 0, b.readError(err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, b.readError(err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, 0, b.readError(errors.New("not a regular file"))
	}
	if b.SizeKnown && info.Size() != b.Size {
		file.Close()
		return nil, 0, &BodySizeMismatchError{
			EntryID:   b.EntryID,
			Direction: b.Direction,
			Path:      b.Path,
			Recorded:  b.Size,
			Actual:    info.Size(),
		}
	}
	return file, info.Size(), nil
}

// Bytes reads the body, keeping at most limit bytes when limit is positive.
// The returned size and truncation flag describe the body on disk rather than
// the returned slice, so a comparison can report a bounded read without
// confusing it with a short file.
func (b StoredBody) Bytes(limit int64) ([]byte, int64, bool, error) {
	reader, size, err := b.Open()
	if err != nil || reader == nil {
		return nil, size, false, err
	}
	defer reader.Close()

	if limit <= 0 {
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, 0, false, b.readError(err)
		}
		return data, size, false, nil
	}

	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, 0, false, b.readError(err)
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	return data, size, truncated, nil
}

// readError names the body a failed system call belongs to, so a diagnostic
// from any command identifies the record and the direction.
func (b StoredBody) readError(err error) *BodyReadError {
	return &BodyReadError{
		EntryID:   b.EntryID,
		Direction: b.Direction,
		Path:      b.Path,
		Err:       err,
	}
}

// StoredBodyDiagnostic is implemented by every failure a stored body can
// produce when the record claims bytes the disk cannot deliver. Callers tell
// these apart from a caller-named body_file (BodyFileError) so a damaged store
// is not reported as a bad patch.
type StoredBodyDiagnostic interface {
	error
	// Evidence returns the record, direction, and path the diagnostic is about.
	Evidence() (entryID, direction, path string)
	storedBodyDiagnostic()
}

// MissingBodyError reports a record that claims bytes for a body whose file is
// gone. The entry itself exists, so this is store damage, not a missing id.
type MissingBodyError struct {
	EntryID   string
	Direction string
	Path      string
	Size      int64
}

func (e *MissingBodyError) Error() string {
	return fmt.Sprintf(
		"history entry %s records %d bytes for its %s body, but %s does not exist",
		e.EntryID, e.Size, e.Direction, e.Path,
	)
}

func (e *MissingBodyError) Evidence() (string, string, string) {
	return e.EntryID, e.Direction, e.Path
}

func (e *MissingBodyError) storedBodyDiagnostic() {}

// BodySizeMismatchError reports a body file whose size disagrees with the
// record. Both directions are reported: a short file means recorded bytes were
// lost, and a long file means the record no longer describes what is on disk.
type BodySizeMismatchError struct {
	EntryID   string
	Direction string
	Path      string
	Recorded  int64
	Actual    int64
}

func (e *BodySizeMismatchError) Error() string {
	return fmt.Sprintf(
		"history entry %s records %d bytes for its %s body, but %s holds %d",
		e.EntryID, e.Recorded, e.Direction, e.Path, e.Actual,
	)
}

func (e *BodySizeMismatchError) Evidence() (string, string, string) {
	return e.EntryID, e.Direction, e.Path
}

func (e *BodySizeMismatchError) storedBodyDiagnostic() {}

// BodyReadError reports a body file that exists but could not be opened,
// inspected, or read. The system's reason is wrapped.
type BodyReadError struct {
	EntryID   string
	Direction string
	Path      string
	Err       error
}

func (e *BodyReadError) Error() string {
	return fmt.Sprintf(
		"read %s body %s of history entry %s: %v",
		e.Direction, e.Path, e.EntryID, e.Err,
	)
}

func (e *BodyReadError) Evidence() (string, string, string) {
	return e.EntryID, e.Direction, e.Path
}

func (e *BodyReadError) storedBodyDiagnostic() {}

func (e *BodyReadError) Unwrap() error { return e.Err }

// StoredBodiesOf returns the two body handles a record points at. It is the one
// place that joins the store's file layout with the record's size claims, so
// every reader of a stored body starts from the same reference: the projection,
// the raw reconstruction, the replay, and the comparison.
func StoredBodiesOf(store *session.Store, event *session.Event) (request, response StoredBody) {
	if event == nil || event.Kind == session.KindMITMError {
		return StoredBody{}, StoredBody{}
	}
	request = storedBodyOf(store, event, session.BodyRequest)
	response = storedBodyOf(store, event, session.BodyResponse)
	return request, response
}

func storedBodyOf(store *session.Store, event *session.Event, direction string) StoredBody {
	body := StoredBody{
		EntryID:   event.ID,
		Direction: directionName(direction),
	}
	if direction == session.BodyRequest {
		body.Path = store.RequestBodyPath(event)
		if event.ReqBodySize != nil {
			body.Size, body.SizeKnown = *event.ReqBodySize, true
		}
	} else {
		body.Path = store.ResponseBodyPath(event)
		if event.RespBodySize != nil {
			body.Size, body.SizeKnown = *event.RespBodySize, true
		}
	}
	return body
}

// directionName renders the body vocabulary for a diagnostic. The store names
// the directions req and resp because they are file-name fragments; a caller
// reading a message gets the word the entry view uses.
func directionName(direction string) string {
	if direction == session.BodyRequest {
		return directionRequest
	}
	return directionResponse
}
