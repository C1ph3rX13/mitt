package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// Body directions used by the <entry-id>-<direction>.bin naming contract.
const (
	BodyRequest  = "req"
	BodyResponse = "resp"
)

// BodiesDir is the directory that stores decoded entity bodies.
func (s *Store) BodiesDir() string {
	return filepath.Join(s.dir, "bodies")
}

// BodyPath returns the final path of one body file.
func (s *Store) BodyPath(bodyID, direction string) string {
	return filepath.Join(s.BodiesDir(), bodyID+"-"+direction+".bin")
}

// RequestBodyPath returns the request body path for an event. The body id is
// the entry id when present; malformed legacy events fall back to the event id.
func (s *Store) RequestBodyPath(event *Event) string {
	return s.BodyPath(event.requestBodyID(), BodyRequest)
}

// ResponseBodyPath returns the response body path for an event.
func (s *Store) ResponseBodyPath(event *Event) string {
	return s.BodyPath(event.responseBodyID(), BodyResponse)
}

// Body is one entity body staged for a record. It lives in a protocol stage —
// a locked temporary file beside its final path — until the owning Recorder
// commits. Open and CopyBody work before and after that point, which lets
// callers stream a body upstream or to the client without observing the
// publication state. The persistence rules (naming, permissions, lock, sync)
// belong to atomicfile; Body only decides which path is readable now.
type Body struct {
	stage     *atomicfile.Stage
	committed bool
}

// Write appends body bytes while the record is open. A sealed body — one whose
// bytes are already synced but not yet published — rejects further writes so
// the synced prefix can never diverge from what gets renamed into place.
func (b *Body) Write(p []byte) (int, error) {
	if b == nil || b.stage == nil {
		return 0, errors.New("body is closed")
	}
	return b.stage.Write(p)
}

// Open streams the body. Before commit it reads the temporary file; after
// commit it reads the published file.
func (b *Body) Open() (*os.File, error) {
	if b == nil || b.stage == nil {
		return nil, errors.New("body is nil")
	}
	return os.Open(b.path())
}

// CopyBody copies the body into w.
func (b *Body) CopyBody(w io.Writer) (int64, error) {
	file, err := b.Open()
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return io.Copy(w, file)
}

// Size returns the current body size.
func (b *Body) Size() (int64, error) {
	if b == nil || b.stage == nil {
		return 0, errors.New("body is nil")
	}
	info, err := os.Stat(b.path())
	if err != nil {
		return 0, fmt.Errorf("stat body file: %w", err)
	}
	return info.Size(), nil
}

func (b *Body) path() string {
	if b.committed {
		return b.stage.FinalPath()
	}
	return b.stage.TempPath()
}

// seal flushes the write side so the bytes are durable before the rename. The
// write descriptor stays open — and keeps its lock — until publish, so a stale
// temporary sweep cannot take the body between sealing and renaming it.
func (b *Body) seal() error {
	if b == nil || b.stage == nil {
		return errors.New("body is closed")
	}
	return b.stage.Seal()
}

// publish renames the temporary file into its final path and then closes the
// write side, releasing the writer lock only after the temporary name is gone.
// A close failure still fails the commit so the caller rolls the record back.
func (b *Body) publish() error {
	if b == nil || b.stage == nil {
		return errors.New("body is nil")
	}
	if err := b.stage.Publish(); err != nil {
		return err
	}
	b.committed = true
	return nil
}

// discard removes both the temporary and published files. It is used by a
// record rollback, including a partially published commit.
func (b *Body) discard() {
	if b == nil || b.stage == nil {
		return
	}
	b.stage.Discard()
	b.committed = false
}
