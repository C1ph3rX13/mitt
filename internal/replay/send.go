package replay

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/httpwire"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/session"
)

const (
	dialTimeout     = 10 * time.Second
	exchangeTimeout = 2 * time.Minute
)

// SendRequest describes one direct dispatch: a data store, exactly one base,
// an optional request-document patch, and an outbound TLS profile. Diag
// carries the store's warnings, so a damaged history line is reported instead
// of being silently skipped by the entry read.
type SendRequest struct {
	Store *session.Store
	Base  Base
	Patch Patch

	// TLSProfile selects the outbound ClientHello fingerprint. Empty means
	// standard.
	TLSProfile string

	// Diag receives non-fatal warnings, such as a corrupt history line.
	Diag io.Writer
}

// SendResult is the persisted result of one direct dispatch.
type SendResult struct {
	Event *session.Event
}

type preparedRequest struct {
	// target is the address dialed and the URL form the request document
	// exposes.
	target *url.URL
	// origin is where the request is addressed. The recorded URL comes from
	// it, so it cannot disagree with the head the origin is sent.
	origin httpwire.Origin
	// head is the request head whose bytes are raw. The recorded lines and the
	// version token all come from the message that was actually written.
	head *httpwire.Request
	body []byte
	// raw is the exact byte sequence written to the connection. A from-file
	// request without a patch keeps its original bytes; rendered bases use
	// the normalized request line and headers.
	raw []byte
	// mutations is the raw --patch text applied to this request.
	mutations string
	// document is the final request as it will be sent, in the shared entry
	// document shape. DryRun reports it directly, so the previewed bytes and
	// the previewed document come from one rendering instead of two.
	document *entry.Document
}

// Send dispatches one request directly to its final URL, bypassing the capture
// proxy, and persists the actual request bytes and upstream response.
func Send(request SendRequest) (*SendResult, error) {
	if request.Store == nil {
		return nil, output.InternalErrorf("send requires a data store")
	}
	resolved, err := resolve(request)
	if err != nil {
		return nil, err
	}
	prepared := resolved.prepared
	profile := resolved.profile

	started := time.Now()

	kind := session.KindSent
	parentID := ""
	if base, ok := request.Base.(EntryBase); ok {
		kind = session.KindReplayed
		parentID = base.ID
	}
	exchange, err := request.Store.BeginExchange(session.ExchangeSpec{
		Kind:     kind,
		Started:  started,
		ParentID: parentID,
	})
	if err != nil {
		return nil, output.InternalErrorf("create history entry: %v", err)
	}
	defer exchange.Abort()

	// A send has no client: the line this program wrote is both the recorded
	// line and the line the origin receives.
	exchange.IdentifyRequest(session.MessageIdentity(prepared.head, prepared.origin, prepared.head.StartLine))
	event := exchange.Event()
	event.RequestHeaders = entry.ClonePairs(prepared.head.Headers)
	event.Mutations = prepared.mutations

	requestBody, err := exchange.Request()
	if err != nil {
		return nil, output.InternalErrorf("store request body: %v", err)
	}
	if _, err := requestBody.Write(prepared.body); err != nil {
		return nil, output.InternalErrorf("store request body: %v", err)
	}

	conn, reader, err := dialPrepared(prepared, profile)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := writeAll(conn, prepared.raw); err != nil {
		return nil, tempFailure(
			"write request to "+prepared.target.Host+": "+err.Error(),
			"check connectivity to the target and retry",
			map[string]any{"url": event.URL, "error": err.Error()},
		)
	}

	response, err := httpwire.ReadResponse(reader, prepared.head.Method)
	if err != nil {
		return nil, tempFailure(
			"read response from "+prepared.target.Host+": "+err.Error(),
			"check that the target sends a supported HTTP/1.1 response and retry",
			map[string]any{"url": event.URL, "error": err.Error()},
		)
	}

	responseBody, err := exchange.Response()
	if err != nil {
		return nil, output.InternalErrorf("store response body: %v", err)
	}
	if _, err := response.ReadBody(reader, responseBody); err != nil {
		return nil, tempFailure(
			"read response body from "+prepared.target.Host+": "+err.Error(),
			"retry the request; inspect the target if the failure persists",
			map[string]any{"url": event.URL, "status": response.Status, "error": err.Error()},
		)
	}

	// A send result is only useful if it is on record: the caller addresses
	// this exchange by id afterwards, so a missing history line is a failure
	// here even though the bytes did reach the target. There is no client
	// waiting on a delivery, so the delivery argument is nil.
	completed := session.ResponseResult{
		Status:  response.Status,
		Line:    response.StartLine,
		Headers: entry.ClonePairs(response.Headers),
	}
	if err := exchange.Commit(completed, nil, nil); err != nil {
		return nil, output.InternalErrorf("append history entry: %v", err)
	}

	result := &SendResult{Event: event}
	if response.Status >= 500 {
		return result, output.NewError(
			output.CodeTempFailure,
			fmt.Sprintf("upstream responded with HTTP %d", response.Status),
			fmt.Sprintf("retry the request; inspect the recorded exchange with `mitt show %s`", event.ID),
		).WithDetail("id", event.ID).
			WithDetail("status", response.Status).
			WithDetail("url", event.URL)
	}
	return result, nil
}

func dialPrepared(request *preparedRequest, profile TLSProfile) (net.Conn, *bufio.Reader, error) {
	address := dialAddress(request.target)
	rawConn, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		return nil, nil, tempFailure(
			"connect to "+address+": "+err.Error(),
			"check DNS, network reachability, and retry",
			map[string]any{"address": address, "error": err.Error()},
		)
	}
	_ = rawConn.SetDeadline(time.Now().Add(exchangeTimeout))

	if strings.EqualFold(request.target.Scheme, "https") {
		tlsConn, err := handshakeTLS(rawConn, request.target.Hostname(), profile)
		if err != nil {
			rawConn.Close()
			return nil, nil, output.NewError(
				output.CodeUpstreamTLS,
				"TLS handshake with "+address+" failed: "+err.Error(),
				"verify the target certificate and protocol; retrying the same request will not help",
			).WithDetail("address", address).WithDetail("error", err.Error())
		}
		return tlsConn, bufio.NewReader(tlsConn), nil
	}
	return rawConn, bufio.NewReader(rawConn), nil
}

func dialAddress(target *url.URL) string {
	port := target.Port()
	if port == "" {
		if strings.EqualFold(target.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(target.Hostname(), port)
}

func parseAbsoluteURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := validateURL(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// requestTarget is the origin-form target written for target. A URL with no
// path still needs a request-target, so it becomes "/".
func requestTarget(target *url.URL) string {
	value := target.RequestURI()
	if value == "" {
		return "/"
	}
	return value
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := w.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func tempFailure(message, hint string, details map[string]any) *output.AppError {
	appErr := output.NewError(output.CodeTempFailure, message, hint)
	for key, value := range details {
		appErr.WithDetail(key, value)
	}
	return appErr
}
