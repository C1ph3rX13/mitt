package httpwire

import (
	"errors"
	"fmt"
	"io"
)

// Write emits the start line, headers, and body according to the framing rule
// computed when the message was parsed. Header order and duplicates are kept.
//
// size is the entity body length. A chunked message is re-encoded as one
// chunk, because chunk boundaries are not stored. If size is greater than zero
// but the head declared no body (for example an h2 request without
// Content-Length), the bytes are emitted as an identity body so `show --raw`
// stays faithful to the stored body file. HEAD / 204 / 304 responses never
// emit a body.
func (m *Message) Write(w io.Writer, body io.Reader, size int64) error {
	if size < 0 {
		return fmt.Errorf("invalid body size %d", size)
	}
	if size > 0 && body == nil {
		return errors.New("body is nil")
	}
	if _, err := io.WriteString(w, m.StartLine+"\r\n"); err != nil {
		return err
	}
	for _, header := range m.Headers {
		if _, err := fmt.Fprintf(w, "%s: %s\r\n", header[0], header[1]); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\r\n"); err != nil {
		return err
	}

	switch {
	case m.framing.bodyForbidden:
		return nil
	case m.framing.chunked:
		return writeChunked(w, body, size)
	case m.framing.hasBody || size > 0:
		return writeIdentity(w, body, size)
	default:
		return nil
	}
}

func writeChunked(w io.Writer, body io.Reader, size int64) error {
	if size == 0 {
		_, err := io.WriteString(w, "0\r\n\r\n")
		return err
	}
	if _, err := fmt.Fprintf(w, "%x\r\n", size); err != nil {
		return err
	}
	if body == nil {
		return errors.New("body is nil")
	}
	if _, err := io.CopyN(w, body, size); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\r\n0\r\n\r\n")
	return err
}

func writeIdentity(w io.Writer, body io.Reader, size int64) error {
	if size == 0 {
		return nil
	}
	if body == nil {
		return errors.New("body is nil")
	}
	_, err := io.CopyN(w, body, size)
	return err
}
