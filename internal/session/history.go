package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// NotFoundError reports history ids that do not exist. It is the single
// not-found shape of the store: every command maps it to the same
// ENTRY_NOT_FOUND error instead of inventing its own message and details.
type NotFoundError struct {
	IDs []string
}

func (e *NotFoundError) Error() string {
	if len(e.IDs) == 1 {
		return "history entry " + e.IDs[0] + " does not exist"
	}
	return "history entries " + strings.Join(e.IDs, ", ") + " do not exist"
}

// historyLog is history.jsonl's one read/write boundary: the path, the line
// scanner every read shares, and the append that repairs a crash-truncated tail
// under the same lock as the line that follows it.
//
// Reads and writes agree on what a record is: one complete line terminated by a
// newline. A fragment without one is a crash during append, never a record.
// Readers ignore it, and the next append removes it before adding a line, so a
// new record can neither join the fragment into one corrupt line nor be lost
// behind it.
type historyLog struct {
	path string
}

// history returns the log this store reads and writes.
func (s *Store) history() historyLog {
	return historyLog{path: s.HistoryPath()}
}

// lineOutcome is what one append did to the log. It is the one place that
// answers whether a record became visible; body retention follows from it.
type lineOutcome struct {
	// Visible reports that the line is complete in history.jsonl and can be
	// found by id.
	Visible bool
	// Err is the commit error. It is nil exactly when the line is visible and
	// durable.
	Err error
}

// abandonBodies reports whether the commit failed before the line became
// visible. That is the only failure where removing a record's body files
// cannot leave a readable line pointing at missing bytes.
func (o lineOutcome) abandonBodies() bool {
	return !o.Visible
}

// appendLine is the persistence protocol's append, held as a function value so
// a test can make a line land and the forcing step after it fail — the one
// failure a real regular file cannot be made to produce portably. Production
// never reassigns it.
var appendLine = atomicfile.Append

// appendEvent appends one event line and reports what became of it. Encoding
// can fail before the log is touched; the write callback can fail before a
// complete line exists; a failure after the write (the file sync or the
// directory sync) leaves the line visible but not durable. Only the last case
// keeps the record — see lineOutcome.
func (h historyLog) appendEvent(event *Event) lineOutcome {
	data, err := MarshalEvent(event)
	if err != nil {
		return lineOutcome{Err: err}
	}

	landed := false
	err = appendLine(h.path, atomicfile.Private, func(file *os.File) error {
		if err := repairTruncatedTail(file); err != nil {
			return err
		}
		if _, err := file.Write(data); err != nil {
			return err
		}
		landed = true
		return nil
	})
	if err != nil {
		return lineOutcome{Visible: landed, Err: fmt.Errorf("append history: %w", err)}
	}
	return lineOutcome{Visible: true}
}

// tailRepairChunk bounds how much of the tail is read while looking for the last
// newline; a history line is normally far smaller, and the loop only makes a
// second pass for a fragment larger than one chunk.
const tailRepairChunk = 32 * 1024

// repairTruncatedTail removes a final line that never got its newline. It runs
// with the append lock held, so truncation and the bytes that follow are one
// step for every writer: no line can be appended after a fragment.
func repairTruncatedTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat history: %w", err)
	}
	size := info.Size()
	for end := size; end > 0; {
		start := end - tailRepairChunk
		if start < 0 {
			start = 0
		}
		chunk := make([]byte, end-start)
		if _, err := file.ReadAt(chunk, start); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read history tail: %w", err)
		}
		if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
			keep := start + int64(index) + 1
			if keep == size {
				return nil
			}
			if err := file.Truncate(keep); err != nil {
				return fmt.Errorf("truncate history tail: %w", err)
			}
			return nil
		}
		end = start
	}
	if size > 0 {
		if err := file.Truncate(0); err != nil {
			return fmt.Errorf("truncate history tail: %w", err)
		}
	}
	return nil
}

// ReadHistory reads every complete event line from history.jsonl.
//
// A truncated final line (a crash during append) is ignored. A corrupt line in
// the middle is skipped with a warning so one damaged record cannot make the
// entire history unreadable. Missing body sizes are backfilled from the body
// files; a missing body is treated as an empty body.
func (s *Store) ReadHistory(stderr io.Writer) ([]Event, error) {
	events := []Event{}
	err := s.history().each(stderr, func(event *Event) {
		event.backfillBodySizes(s)
		events = append(events, *event)
	})
	return events, err
}

// ReadEntries reads the requested entries in one pass over history.jsonl and
// returns them in the requested order. Only the returned events have their body
// sizes backfilled, so a command that addresses one entry does not stat every
// body in the store.
//
// The walk covers the whole file even after every id is found: a warning is
// about the store, not about the entry that happened to be asked for, and a
// read that reported damage only when the damage came first would be a second
// corrupt-line policy. Corrupt lines reach stderr exactly as ReadHistory does.
//
// A missing id yields *NotFoundError naming every id that was not found.
func (s *Store) ReadEntries(ids []string, stderr io.Writer) ([]*Event, error) {
	if len(ids) == 0 {
		return []*Event{}, nil
	}

	wanted := make(map[string]bool, len(ids))
	order := make([]string, 0, len(ids))
	for _, id := range ids {
		if !wanted[id] {
			wanted[id] = true
			order = append(order, id)
		}
	}
	found := make(map[string]*Event, len(order))
	err := s.history().each(stderr, func(event *Event) {
		if !wanted[event.ID] {
			return
		}
		event.backfillBodySizes(s)
		copied := *event
		found[event.ID] = &copied
	})
	if err != nil {
		return nil, err
	}

	missing := make([]string, 0)
	for _, id := range order {
		if found[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, &NotFoundError{IDs: missing}
	}

	// Each request gets its own copy: two ids that resolve to the same line
	// (a repeated id) must not hand two callers one mutable event.
	events := make([]*Event, 0, len(ids))
	for _, id := range ids {
		copied := *found[id]
		events = append(events, &copied)
	}
	return events, nil
}

// each walks every complete event line and calls visit. All read operations
// share this scanner so the corrupt-line policy, the truncated-tail rule, and
// the line numbering cannot drift.
func (h historyLog) each(stderr io.Writer, visit func(*Event)) error {
	file, err := os.Open(h.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open history: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		atEOF := errors.Is(readErr, io.EOF)
		complete := len(line) > 0 && line[len(line)-1] == '\n'
		lineNumber++

		if len(line) > 0 && complete {
			line = line[:len(line)-1]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				var event Event
				if err := json.Unmarshal(line, &event); err != nil {
					warnCorrupt(stderr, lineNumber, err)
				} else if event.ID == "" || event.Kind == "" {
					warnCorrupt(stderr, lineNumber, errors.New("missing id or kind"))
				} else {
					visit(&event)
				}
			}
		}

		if atEOF {
			// A non-empty final fragment without a newline is a partial
			// append and is intentionally ignored. The next append removes it.
			break
		}
		if readErr != nil {
			return fmt.Errorf("read history: %w", readErr)
		}
	}
	return nil
}

func warnCorrupt(w io.Writer, line int, err error) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "mitt: warning: skipping corrupt history line %d: %v\n", line, err)
}

func (e *Event) backfillBodySizes(store *Store) {
	if e.ReqBodySize == nil {
		size := fileSize(store.RequestBodyPath(e))
		e.ReqBodySize = &size
	}
	if e.RespBodySize == nil {
		size := fileSize(store.ResponseBodyPath(e))
		e.RespBodySize = &size
	}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
