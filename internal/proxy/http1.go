//go:build linux

package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/C1ph3rX13/mitt/internal/httpwire"
	"github.com/C1ph3rX13/mitt/internal/session"
)

const (
	upstreamTimeout = 2 * time.Minute

	// repeaterContextHeader is the Playwright-injected traffic marker. It is
	// matched case-insensitively, recorded on the event, and never forwarded.
	repeaterContextHeader = "X-Repeater-Context"
)

// serveConnection owns one client connection. Requests are handled serially so
// header order and body boundaries stay unambiguous.
func (c *Capture) serveConnection(conn net.Conn, canIdle func() bool, markActive func() bool) {
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	sink := h1ReceiptSink{writer: writer}
	clientAddr := conn.RemoteAddr().String()

	for {
		if !canIdle() {
			return
		}
		head, err := httpwire.ReadRequest(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			c.reject(sink, clientAddr, "", newFailure(failMalformedRequest).withError(err))
			return
		}
		if !markActive() {
			return
		}
		if strings.EqualFold(head.Method, "CONNECT") {
			c.handleConnect(conn, reader, writer, head, canIdle, markActive)
			return
		}
		if !c.handleRequest(conn, reader, writer, head, nil) {
			return
		}
	}
}

func (c *Capture) handleRequest(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer, head *httpwire.Request, tunnel *mitmUpstream) bool {
	started := time.Now()
	sink := h1ReceiptSink{writer: writer}
	clientAddr := conn.RemoteAddr().String()

	var origin httpwire.Origin
	var parsed *url.URL
	targetHost := ""
	if tunnel == nil {
		var err error
		parsed, err = url.Parse(head.Target)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" {
			c.reject(sink, clientAddr, "", newFailure(failNonAbsoluteTarget))
			return false
		}
		if !strings.EqualFold(parsed.Scheme, "http") {
			kind := failUnsupportedTarget
			if strings.EqualFold(parsed.Scheme, "https") {
				kind = failHTTPSAbsoluteTarget
			}
			c.reject(sink, clientAddr, parsed.Hostname(), newFailure(kind))
			return false
		}
		targetHost = parsed.Hostname()

		// A proxy client addresses the origin in the request line; the URL
		// recorded for history and the origin-form target written upstream are
		// both derived from those parts, in one place.
		origin = httpwire.OriginOfURL(parsed)

		if decision := c.scope.Check(targetHost); !decision.Allowed {
			c.reject(sink, clientAddr, targetHost, newFailure(failScopeDenied).withDetails(scopeDetails(decision)))
			return false
		}
	} else {
		targetHost = tunnel.host
		// Inside a CONNECT tunnel the authority is fixed by the tunnel, so only
		// origin-form targets are meaningful. Absolute-form would silently
		// attempt to cross hosts.
		if !strings.HasPrefix(head.Target, "/") && head.Target != "*" {
			c.reject(sink, clientAddr, targetHost, newFailure(failOriginFormRequired))
			return false
		}
		// The wire authority is the tunnel's, so a request that names another
		// one is rejected rather than forwarded with a URL recorded against the
		// tunnel. HTTP/2 asks the same question of :authority.
		if tunnel.authorityMismatch(httpwire.First(head.Headers, "Host")) {
			c.reject(sink, clientAddr, targetHost, newFailure(failTunnelAuthorityMismatch))
			return false
		}
		var err error
		origin, err = tunnel.newOrigin(head.Target)
		if err != nil {
			c.reject(sink, clientAddr, targetHost, newFailure(failOriginFormRequired).withError(err))
			return false
		}
	}

	exchange, err := c.store.BeginExchange(session.ExchangeSpec{
		Kind:       session.KindCaptured,
		Started:    started,
		ClientAddr: clientAddr,
	})
	if err != nil {
		c.warn("create capture event: %v", err)
		c.reject(sink, clientAddr, targetHost, newFailure(failCreateEntry))
		return false
	}
	defer exchange.Abort()
	event := exchange.Event()

	if expectsContinue(head.Headers) {
		_, _ = io.WriteString(writer, "HTTP/1.1 100 Continue\r\n\r\n")
		if err := writer.Flush(); err != nil {
			return false
		}
	}

	requestBody, err := exchange.Request()
	if err != nil {
		c.warn("create request body for %s: %v", event.ID, err)
		c.reject(sink, clientAddr, targetHost, newFailure(failStoreRequestBody))
		return false
	}
	requestSize, err := head.ReadBody(reader, requestBody)
	if err != nil {
		c.reject(sink, clientAddr, targetHost, newFailure(failReadRequestBody).withError(err))
		return false
	}

	// The head sent upstream differs from the received one in exactly three
	// ways: the marker and Expect are dropped, a chunked head loses its
	// Content-Length, and Host names the origin being addressed. Only the first
	// two are this caller's business; Forwarded owns the framing filter and the
	// rule, and returns the header set to record. The Host rule is shared with
	// the HTTP/2 path, which addresses the same origin through :authority.
	forwardedHeaders, context := extractContext(head.Headers)
	if context != nil {
		event.Context = context
	}
	forwardedHeaders = httpwire.Strip(forwardedHeaders, "Expect")
	if tunnel == nil {
		// The request-target authority wins over a client Host, per RFC 9112.
		forwardedHeaders = httpwire.WithHost(forwardedHeaders, origin.Authority)
	} else if httpwire.First(forwardedHeaders, "Host") == "" {
		// Inside a tunnel the received Host already names the origin — a
		// foreign one was rejected above — so it is forwarded as written and
		// only a missing one is filled in.
		forwardedHeaders = httpwire.WithHost(forwardedHeaders, origin.Authority)
	}
	forwardedRequest := head.Forwarded(head.Method+" "+origin.WireTarget()+" "+head.Version, forwardedHeaders)

	identity := session.MessageIdentity(head, origin, forwardedRequest.StartLine)
	if tunnel != nil {
		identity.UpstreamCertSHA256 = tunnel.fingerprint
	}
	exchange.IdentifyRequest(identity)
	event.RequestHeaders = forwardedRequest.Headers

	upstreamName := origin.Authority
	if tunnel != nil {
		upstreamName = tunnel.origin
	}
	var upstream io.Writer
	var upstreamReader *bufio.Reader
	if tunnel == nil {
		dialed, err := net.DialTimeout("tcp", upstreamAddress(parsed), dialTimeout)
		if err != nil {
			c.warn("dial upstream %s: %v", origin.Authority, err)
			c.reject(sink, clientAddr, targetHost, newFailure(failUpstreamDial))
			return false
		}
		defer dialed.Close()
		_ = dialed.SetDeadline(time.Now().Add(upstreamTimeout))
		upstream = dialed
		upstreamReader = bufio.NewReader(dialed)
	} else {
		_ = tunnel.conn.SetDeadline(time.Now().Add(upstreamTimeout))
		upstream = tunnel.conn
		upstreamReader = tunnel.reader
	}

	if err := writeBodyMessage(upstream, &forwardedRequest, requestBody, requestSize); err != nil {
		c.warn("forward request to %s: %v", upstreamName, err)
		c.reject(sink, clientAddr, targetHost, c.classifyTunnelFailure(tunnel, failForwardRequest, err))
		return false
	}

	response, err := httpwire.ReadResponse(upstreamReader, head.Method)
	if err != nil {
		fail := c.classifyTunnelFailure(tunnel, failReadUpstreamResponse, err)
		if fail.is(failH2OnlyTarget) {
			c.warn("upstream %s refused the HTTP/1.1 exchange and completed an h2 handshake: %v", upstreamName, err)
		} else {
			c.warn("read response from %s: %v", upstreamName, err)
		}
		c.reject(sink, clientAddr, targetHost, fail)
		return false
	}

	responseBody, err := exchange.Response()
	if err != nil {
		c.warn("create response body for %s: %v", event.ID, err)
		c.reject(sink, clientAddr, targetHost, newFailure(failStoreResponseBody))
		return false
	}
	if _, err := response.ReadBody(upstreamReader, responseBody); err != nil {
		c.warn("read response body from %s: %v", upstreamName, err)
		c.reject(sink, clientAddr, targetHost, newFailure(failReadUpstreamBody))
		return false
	}
	// The response handle is opened before delivery because it may read the
	// staged file; commit then decides the record's fate from what the client
	// actually received. A storage failure is a diagnostic here, never a
	// reason to break a request whose bytes were already fetched.
	responseReader, readerSize, readerErr := exchange.ResponseReader()
	if readerErr != nil {
		c.warn("open response body for %s: %v", event.ID, readerErr)
		c.reject(sink, clientAddr, targetHost, newFailure(failReadStoredBody))
		return false
	}
	defer responseReader.Close()

	keepAlive := !wantsClose(head.Version, head.Headers) &&
		!wantsClose(response.Version, response.Headers) &&
		!response.CloseDelimited()

	forwardedResponse := *response
	forwardedResponse.Headers = response.FramingHeaders()
	// Delivery is observed here, not inferred by Commit: the write and the
	// flush are the two steps where the client can disappear. A failure means
	// the exchange was not a complete transfer, so the record must not exist.
	delivery := forwardedResponse.Write(writer, responseReader, readerSize)
	if delivery == nil {
		delivery = writer.Flush()
	}
	if delivery != nil {
		c.warn("deliver response for %s: %v", event.ID, delivery)
	}

	result := session.ResponseResult{
		Status:  response.Status,
		Line:    response.StartLine,
		Headers: response.Headers,
	}
	if err := exchange.Commit(result, delivery, c.publish); err != nil && delivery == nil {
		// A delivery failure was already reported above; anything else that
		// refuses the commit is a storage event worth its own warning.
		c.warn("commit capture for %s: %v", event.ID, err)
	}
	return delivery == nil && keepAlive
}

func writeBodyMessage(w io.Writer, message *httpwire.Message, body *session.Body, bodySize int64) error {
	if bodySize == 0 {
		return message.Write(w, nil, 0)
	}
	file, err := body.Open()
	if err != nil {
		return err
	}
	defer file.Close()
	return message.Write(w, file, bodySize)
}

func writeSimpleResponse(w io.Writer, status int, reason, body string) error {
	if _, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n", status, reason); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "Content-Type: text/plain; charset=utf-8\r\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n", len(body)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "Connection: close\r\n\r\n"); err != nil {
		return err
	}
	_, err := io.WriteString(w, body)
	return err
}

// extractContext removes every X-Repeater-Context field regardless of header
// name casing and returns the first non-empty value for the history context
// field. Later duplicate values never override an earlier non-empty value.
func extractContext(headers [][2]string) ([][2]string, *string) {
	context := ""
	for _, value := range httpwire.Values(headers, repeaterContextHeader) {
		if context == "" {
			context = strings.TrimSpace(value)
		}
	}
	stripped := httpwire.Strip(headers, repeaterContextHeader)
	if context == "" {
		return stripped, nil
	}
	return stripped, &context
}

func expectsContinue(headers [][2]string) bool {
	for _, value := range httpwire.Values(headers, "Expect") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "100-continue") {
				return true
			}
		}
	}
	return false
}

func wantsClose(version string, headers [][2]string) bool {
	closeToken := false
	keepAliveToken := false
	for _, value := range httpwire.Values(headers, "Connection") {
		for _, token := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(token)) {
			case "close":
				closeToken = true
			case "keep-alive":
				keepAliveToken = true
			}
		}
	}
	if closeToken {
		return true
	}
	return strings.EqualFold(version, "HTTP/1.0") && !keepAliveToken
}

func upstreamAddress(target *url.URL) string {
	port := target.Port()
	if port == "" {
		port = "80"
	}
	return net.JoinHostPort(target.Hostname(), port)
}
