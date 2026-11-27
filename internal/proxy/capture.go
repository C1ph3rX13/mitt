//go:build linux

package proxy

import (
	"fmt"
	"io"
	"sync"

	"github.com/C1ph3rX13/mitt/internal/session"
)

// eventEmitter serializes NDJSON capture events on stdout for `proxy start
// --events`. A nil sink is a no-op, keeping the default proxy silent.
type eventEmitter struct {
	mu   sync.Mutex
	sink io.Writer
}

func newEventEmitter(sink io.Writer) *eventEmitter {
	return &eventEmitter{sink: sink}
}

func (e *eventEmitter) emit(event *session.Event) error {
	if e == nil || e.sink == nil {
		return nil
	}
	data, err := session.MarshalEvent(event)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.sink.Write(data); err != nil {
		return fmt.Errorf("write capture event: %w", err)
	}
	return nil
}

// publish mirrors a committed record to the NDJSON event sink. Failure is a
// diagnostic only: the record is already durable in history.jsonl.
func (c *Capture) publish(event *session.Event) {
	if c == nil || c.events == nil {
		return
	}
	if err := c.events.emit(event); err != nil {
		c.warn("emit event for %s: %v", event.ID, err)
	}
}

func (c *Capture) warn(format string, args ...any) {
	if c == nil || c.stderr == nil {
		return
	}
	fmt.Fprintf(c.stderr, "mitt: warning: "+format+"\n", args...)
}
