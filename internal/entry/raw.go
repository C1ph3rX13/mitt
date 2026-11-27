package entry

import (
	"fmt"
	"io"

	"github.com/C1ph3rX13/mitt/internal/httpwire"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// NoRawBytesError reports a history entry that has no request/response bytes
// to reconstruct. Today that is every mitm_error entry: it records a failure,
// not an exchange.
type NoRawBytesError struct {
	ID   string
	Kind session.Kind
}

func (e *NoRawBytesError) Error() string {
	return "history entry " + e.ID + " has no request or response bytes"
}

// WriteRaw reconstructs the request bytes sent upstream followed by the
// response bytes returned by upstream. No separator bytes are inserted between
// the two messages. Headers keep their stored order and duplicates; bodies are
// copied byte for byte. A message stored with Transfer-Encoding: chunked is
// re-encoded as one chunk plus the terminating chunk because chunk boundaries
// are not persisted.
//
// The request line written is the forwarded one: it is what the next hop
// actually received, and `show --raw` promises the bytes on the wire. The
// summary view keeps both lines, so the raw form and the JSON form answer
// different questions without either one lying about the other.
//
// Body paths come from the same Detail the structured form uses, so the raw
// and JSON shapes of one entry cannot disagree about which file holds its
// bytes.
func WriteRaw(detail *Detail, w io.Writer) error {
	if detail.Kind == session.KindMITMError {
		return &NoRawBytesError{ID: detail.ID, Kind: detail.Kind}
	}

	requestLine := detail.ForwardedRequestLine
	if requestLine == "" {
		requestLine = detail.RequestLine
	}
	if requestLine == "" {
		return fmt.Errorf("entry %s has no request line", detail.ID)
	}

	// Both bodies open as the record describes them: a body the entry says is
	// empty yields no reader, and a body it claims bytes for must exist at that
	// size. Opening both before writing anything means a damaged store cannot
	// leave a partial message on stdout.
	requestBody, requestSize, err := detail.RequestBodyHandle().Open()
	if err != nil {
		return err
	}
	defer closeStoredBody(requestBody)

	var responseBody io.ReadCloser
	var responseSize int64
	if detail.ResponseLine != "" {
		responseBody, responseSize, err = detail.ResponseBodyHandle().Open()
		if err != nil {
			return err
		}
		defer closeStoredBody(responseBody)
	}

	request, err := httpwire.ParseRequest(requestLine, detail.RequestHeaders)
	if err != nil {
		return fmt.Errorf("entry %s has an invalid request head: %w", detail.ID, err)
	}
	if err := writeStoredMessage(w, &request.Message, requestBody, requestSize); err != nil {
		return err
	}
	if detail.ResponseLine == "" {
		return nil
	}

	response, err := httpwire.ParseResponse(detail.ResponseLine, detail.ResponseHeaders, detail.Method)
	if err != nil {
		return fmt.Errorf("entry %s has an invalid response head: %w", detail.ID, err)
	}
	return writeStoredMessage(w, &response.Message, responseBody, responseSize)
}

func writeStoredMessage(w io.Writer, message *httpwire.Message, body io.Reader, bodySize int64) error {
	if body == nil {
		return message.Write(w, nil, bodySize)
	}
	return message.Write(w, body, bodySize)
}

// closeStoredBody releases a stored body reader; a nil reader is a body the
// record says is empty.
func closeStoredBody(body io.ReadCloser) {
	if body != nil {
		_ = body.Close()
	}
}
