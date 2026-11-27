//go:build linux

package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"

	"github.com/C1ph3rX13/mitt/internal/httpwire"
	"github.com/C1ph3rX13/mitt/internal/session"
)

const (
	// h2MaxConcurrentStreams bounds the number of simultaneous client streams
	// the MITM server accepts. Handlers run concurrently; each stream owns a
	// unique history entry and body files, so nothing is merged.
	h2MaxConcurrentStreams = 250

	// h2TapLimit is how many bytes of each direction are kept for defensive
	// troubleshooting when the h2 stack reports a protocol error.
	h2TapLimit = 64 << 10
)

// serveH2Tunnel terminates the client's HTTP/2 connection and maps every
// request stream to one captured event. Requests are forwarded either over a
// multiplexed h2 connection to the origin or over the already verified
// HTTP/1.1 connection when the origin does not speak h2.
func (c *Capture) serveH2Tunnel(client net.Conn, upstream *mitmUpstream, canIdle, markActive func() bool) {
	clientTap := newTapConn(client)
	upstreamTap := newTapConn(upstream.conn)
	upstream.conn = upstreamTap
	upstream.reader = bufio.NewReader(upstreamTap)

	tunnel := &h2Tunnel{
		capture:     c,
		clientAddr:  client.RemoteAddr().String(),
		host:        normalizeHost(upstream.host),
		upstream:    upstream,
		clientTap:   clientTap,
		upstreamTap: upstreamTap,
		canIdle:     canIdle,
		markActive:  markActive,
	}
	if upstream.protocol == "h2" {
		tunnel.h2Client = newH2UpstreamTransport(upstream, tunnel.upstreamProtocolError)
	}
	defer tunnel.close()

	server := &http2.Server{
		MaxConcurrentStreams: h2MaxConcurrentStreams,
		CountError:           tunnel.countProtocolError,
	}
	base := &http.Server{
		Handler:  tunnel,
		ErrorLog: log.New(h2WarningWriter{tunnel: tunnel}, "", 0),
	}
	if !canIdle() {
		return
	}
	server.ServeConn(clientTap, &http2.ServeConnOpts{
		BaseConfig: base,
		Context:    context.Background(),
	})
}

// h2Response is the origin response for one h2 stream.
type h2Response struct {
	status  int
	headers [][2]string
	// forwardHeaders is the client-facing header set. It can differ from the
	// history headers when an h1 upstream sent ambiguous framing that must be
	// sanitized before the h2 client sees it.
	forwardHeaders [][2]string
}

type h2Tunnel struct {
	capture    *Capture
	clientAddr string
	// host is the lowercased CONNECT host, used for diagnostics and
	// target_host; the upstream owns the authority requests are addressed to.
	host string

	upstream     *mitmUpstream
	upstreamMu   sync.Mutex
	upstreamDead bool
	h2Client     *http2.Transport

	clientTap   *tapConn
	upstreamTap *tapConn

	active     atomic.Int32
	markActive func() bool
	canIdle    func() bool

	warnOnce sync.Once
}

func (t *h2Tunnel) close() {
	if t.h2Client != nil {
		t.h2Client.CloseIdleConnections()
	}
	_ = t.upstream.conn.Close()
}

func (t *h2Tunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sink := h2ReceiptSink{writer: w}
	if strings.EqualFold(r.Method, "CONNECT") {
		t.capture.reject(sink, t.clientAddr, t.host, newFailure(failNestedConnect))
		return
	}
	if t.upstream.authorityMismatch(r.Host) {
		t.capture.reject(sink, t.clientAddr, t.host, newFailure(failTunnelAuthorityMismatch))
		return
	}

	t.active.Add(1)
	if !t.markActive() {
		t.active.Add(-1)
		t.capture.reject(sink, t.clientAddr, t.host, newFailure(failShuttingDown))
		return
	}
	defer func() {
		if t.active.Add(-1) == 0 && !t.canIdle() {
			_ = t.clientTap.Close()
		}
	}()
	t.handleStream(w, r)
}

func (t *h2Tunnel) handleStream(w http.ResponseWriter, r *http.Request) {
	sink := h2ReceiptSink{writer: w}
	started := time.Now()

	// An HTTP/2 request carries no request line; :path and :authority are the
	// parts a line would have held. The tunnel owns the authority, and both the
	// recorded URL and the target written upstream come from the same origin.
	origin, err := t.upstream.newOrigin(r.URL.RequestURI())
	if err != nil {
		t.capture.reject(sink, t.clientAddr, t.host, newFailure(failOriginFormRequired).withError(err))
		return
	}

	exchange, err := t.capture.store.BeginExchange(session.ExchangeSpec{
		Kind:       session.KindCaptured,
		Started:    started,
		ClientAddr: t.clientAddr,
	})
	if err != nil {
		t.capture.warn("create h2 capture event: %v", err)
		t.capture.reject(sink, t.clientAddr, t.host, newFailure(failCreateEntry))
		return
	}
	defer exchange.Abort()
	event := exchange.Event()

	headers := h2RequestHeaderPairs(r)
	var context *string
	headers, context = extractContext(headers)
	if context != nil {
		event.Context = context
	}
	headers = httpwire.Strip(headers, "Expect")
	headers = httpwire.WithHost(headers, origin.Authority)

	requestBody, err := exchange.Request()
	if err != nil {
		t.capture.warn("create h2 request body for %s: %v", event.ID, err)
		t.capture.reject(sink, t.clientAddr, t.host, newFailure(failStoreRequestBody))
		return
	}
	requestSize, err := copyH2Body(requestBody, r.Body)
	if err != nil {
		t.capture.warn("read h2 request body for %s: %v", event.ID, err)
		t.capture.reject(sink, t.clientAddr, t.host, newFailure(failReadRequestBody).withError(err))
		return
	}

	// An HTTP/2 request never carries Transfer-Encoding, and the h2 server does
	// not guarantee Content-Length matches the bytes it handed us. Re-derive
	// the framing from the body actually stored so the h1 bridge cannot emit a
	// mismatched frame. A stream that declared Content-Length: 0 keeps that
	// field when the body really is empty; anything else declares nothing,
	// which keeps the recorded head free of a field the client never sent.
	declaredEmpty := requestSize == 0 && httpwire.Has(headers, "Content-Length")
	headers = httpwire.Strip(headers, "Transfer-Encoding")
	headers = httpwire.Strip(headers, "Content-Length")
	if requestSize > 0 || declaredEmpty {
		headers = httpwire.WithBodyLength(headers, requestSize)
	}

	upstreamVersion := httpwire.HTTP11
	if t.h2Client != nil {
		upstreamVersion = httpwire.HTTP2
	}
	identity := session.StreamIdentity(r.Method, origin, upstreamVersion)
	identity.UpstreamCertSHA256 = t.upstream.fingerprint
	exchange.IdentifyRequest(identity)
	event.RequestHeaders = headers

	var response *h2Response
	if t.h2Client != nil {
		response, err = t.forwardH2Stream(exchange, r, origin, headers, requestBody, requestSize)
	} else {
		response, err = t.forwardH1Stream(exchange, r.Method, origin, headers, requestBody, requestSize)
	}
	if err != nil {
		t.capture.warn("forward h2 stream for %s: %v", origin.URL(), err)
		// The tunnel already settled the protocol question during CONNECT, so
		// there is no h2-only diagnosis left to make for this stream.
		t.capture.reject(sink, t.clientAddr, t.host, newFailure(failH2Upstream))
		return
	}

	// The response handle is opened before delivery because it may read the
	// staged file; commit then decides the record's fate from what the stream
	// actually received. A storage failure is a diagnostic, not a reason to
	// fail a stream whose bytes were already fetched.
	responseReader, readerSize, readerErr := exchange.ResponseReader()
	if readerErr != nil {
		t.capture.warn("open h2 response body for %s: %v", event.ID, readerErr)
		t.capture.reject(sink, t.clientAddr, t.host, newFailure(failReadStoredBody))
		return
	}
	defer responseReader.Close()

	// Delivery is observed here, not inferred by Commit: a write or flush that
	// the stream rejected means the client did not receive the exchange, so the
	// record must not exist. x/net/http2 reports a reset stream this way.
	delivery := writeH2Response(w, r.Method, response, responseReader, readerSize)
	if delivery != nil {
		t.capture.warn("deliver h2 response for %s: %v", event.ID, delivery)
	}

	result := session.ResponseResult{
		Status:  response.status,
		Line:    h2ResponseLine(response.status),
		Headers: response.headers,
	}
	if err := exchange.Commit(result, delivery, t.capture.publish); err != nil && delivery == nil {
		// A delivery failure was already reported above; anything else that
		// refuses the commit is a storage event worth its own warning.
		t.capture.warn("commit h2 capture for %s: %v", event.ID, err)
	}
}

func copyH2Body(body *session.Body, reader io.Reader) (int64, error) {
	if _, err := io.Copy(body, reader); err != nil {
		return 0, err
	}
	return body.Size()
}

// forwardH2Stream forwards one stream over the multiplexed upstream h2
// connection. RoundTrip blocks until the complete response body is read, which
// keeps the capture non-streaming and byte-complete like the h1 path.
func (t *h2Tunnel) forwardH2Stream(exchange *session.Exchange, r *http.Request, origin httpwire.Origin, headers [][2]string, body *session.Body, bodySize int64) (*h2Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()

	upstreamURL := &url.URL{
		Scheme:     origin.Scheme,
		Host:       origin.Authority,
		Path:       r.URL.Path,
		RawPath:    r.URL.RawPath,
		RawQuery:   r.URL.RawQuery,
		ForceQuery: r.URL.ForceQuery,
	}
	if origin.Target == "*" {
		upstreamURL.Opaque = "*"
	}
	upstreamRequest := &http.Request{
		Method:        r.Method,
		URL:           upstreamURL,
		Host:          origin.Authority,
		Header:        h2HeaderMap(headers),
		Body:          http.NoBody,
		ContentLength: bodySize,
		Proto:         httpwire.HTTP2.Line,
		ProtoMajor:    2,
	}
	if bodySize > 0 {
		file, err := body.Open()
		if err != nil {
			return nil, err
		}
		defer file.Close()
		upstreamRequest.Body = file
	}
	// The h2 transport appends a default User-Agent when the header map has no
	// entry at all. A nil entry keeps the client's absence of that header
	// faithful to what the event records.
	if !httpwire.Has(headers, "User-Agent") {
		upstreamRequest.Header["User-Agent"] = nil
	}

	response, err := t.h2Client.RoundTrip(upstreamRequest.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	responseBody, err := exchange.Response()
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(responseBody, response.Body); err != nil {
		return nil, err
	}
	responseHeaders := h2ResponseHeaderPairs(response.Header)
	return &h2Response{
		status:         response.StatusCode,
		headers:        responseHeaders,
		forwardHeaders: responseHeaders,
	}, nil
}

// forwardH1Stream bridges an h2 stream onto the verified HTTP/1.1 upstream
// connection. Streams are serialized because one h1 connection handles one
// exchange at a time; each still receives its own history event.
func (t *h2Tunnel) forwardH1Stream(exchange *session.Exchange, method string, origin httpwire.Origin, headers [][2]string, body *session.Body, bodySize int64) (*h2Response, error) {
	t.upstreamMu.Lock()
	defer t.upstreamMu.Unlock()
	if t.upstreamDead {
		return nil, errors.New("upstream connection is closed")
	}

	line := method + " " + origin.WireTarget() + " " + httpwire.HTTP11.Line
	head, err := httpwire.ParseRequest(line, headers)
	if err != nil {
		return nil, err
	}

	_ = t.upstream.conn.SetDeadline(time.Now().Add(upstreamTimeout))
	defer func() { _ = t.upstream.conn.SetDeadline(time.Time{}) }()

	if err := writeBodyMessage(t.upstream.conn, &head.Message, body, bodySize); err != nil {
		t.closeUpstream()
		return nil, err
	}
	response, err := httpwire.ReadResponse(t.upstream.reader, method)
	if err != nil {
		t.closeUpstream()
		return nil, err
	}
	responseBody, err := exchange.Response()
	if err != nil {
		t.closeUpstream()
		return nil, err
	}
	if _, err := response.ReadBody(t.upstream.reader, responseBody); err != nil {
		t.closeUpstream()
		return nil, err
	}
	keepAlive := !wantsClose("HTTP/1.1", headers) &&
		!wantsClose(response.Version, response.Headers) &&
		!response.CloseDelimited()
	if !keepAlive {
		t.closeUpstream()
	}
	return &h2Response{
		status:         response.Status,
		headers:        response.Headers,
		forwardHeaders: response.FramingHeaders(),
	}, nil
}

func (t *h2Tunnel) closeUpstream() {
	if t.upstreamDead {
		return
	}
	t.upstreamDead = true
	_ = t.upstream.conn.Close()
}

// writeH2Response emits the recorded response to the stream and reports whether
// the stream received it. bodySize comes from the body handle that serves the
// bytes, so the decision to write nothing cannot disagree with what is actually
// there. The final flush is part of delivery: x/net/http2 buffers header and
// data frames, so only FlushError proves the bytes left the handler and
// exposes a reset stream or closed connection.
func writeH2Response(w http.ResponseWriter, method string, response *h2Response, body io.Reader, bodySize int64) error {
	for _, header := range response.forwardHeaders {
		if forbiddenH2Header(header[0]) {
			continue
		}
		w.Header().Add(header[0], header[1])
	}
	w.WriteHeader(response.status)
	if !strings.EqualFold(method, "HEAD") && bodySize > 0 && body != nil {
		if _, err := io.Copy(w, body); err != nil {
			return err
		}
	}
	return http.NewResponseController(w).Flush()
}

func forbiddenH2Header(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

// h2RequestHeaderPairs flattens a parsed h2 request into the ordered pairs the
// event model uses. HTTP/2 has no meaningful header order, so names are sorted
// to keep output deterministic; repeated values keep their relative order.
func h2RequestHeaderPairs(r *http.Request) [][2]string {
	pairs := make([][2]string, 0, len(r.Header)+1)
	if r.Host != "" {
		pairs = append(pairs, [2]string{"Host", r.Host})
	}
	return append(pairs, sortedHeaderPairs(r.Header)...)
}

func h2ResponseHeaderPairs(header http.Header) [][2]string {
	return sortedHeaderPairs(header)
}

// sortedHeaderPairs flattens an http.Header into deterministic ordered pairs.
// HTTP/2 does not define a meaningful header order; sorting by lowercased name
// keeps event output stable while repeated values keep their relative order.
func sortedHeaderPairs(header http.Header) [][2]string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	pairs := make([][2]string, 0, len(header))
	for _, name := range names {
		for _, value := range header[name] {
			pairs = append(pairs, [2]string{name, value})
		}
	}
	return pairs
}

func h2HeaderMap(pairs [][2]string) http.Header {
	header := make(http.Header, len(pairs))
	for _, pair := range pairs {
		if strings.EqualFold(pair[0], "Host") {
			continue
		}
		header.Add(pair[0], pair[1])
	}
	return header
}

func h2ResponseLine(status int) string {
	text := http.StatusText(status)
	if text == "" {
		text = "Unknown Status"
	}
	return fmt.Sprintf("HTTP/2.0 %d %s", status, text)
}

// h2UpstreamDialer returns the already verified TLS connection to the first
// caller and re-dials a verified h2 connection only if the transport ever needs
// a second one. The tunnel address is fixed, so the dialer never follows a
// request's :authority to a different origin.
type h2UpstreamDialer struct {
	mu      sync.Mutex
	host    string
	address string
	first   net.Conn
}

func (d *h2UpstreamDialer) DialTLSContext(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.first != nil {
		conn := d.first
		d.first = nil
		return conn, nil
	}
	raw, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", d.address)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, &tls.Config{
		ServerName: d.host,
		NextProtos: []string{"h2"},
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if protocol := conn.ConnectionState().NegotiatedProtocol; protocol != "h2" {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream re-dial negotiated %q, want h2", protocol)
	}
	return conn, nil
}

func newH2UpstreamTransport(upstream *mitmUpstream, countError func(string)) *http2.Transport {
	return &http2.Transport{
		StrictMaxConcurrentStreams: true,
		DisableCompression:         true,
		CountError:                 countError,
		DialTLSContext: (&h2UpstreamDialer{
			host:    upstream.host,
			address: upstream.address,
			first:   upstream.conn,
		}).DialTLSContext,
	}
}

// h2WarningWriter receives x/net/http2 server diagnostics through
// http.Server.ErrorLog and turns the first one into a warning history event.
type h2WarningWriter struct {
	tunnel *h2Tunnel
}

func (w h2WarningWriter) Write(p []byte) (int, error) {
	w.tunnel.recordParseWarning(strings.TrimSpace(string(p)))
	return len(p), nil
}

// countProtocolError records frame and stream protocol violations that the h2
// server counts but does not route through ErrorLog. Only the first anomaly in
// a tunnel becomes a warning event; the rest stay as stderr diagnostics.
func (t *h2Tunnel) countProtocolError(kind string) {
	t.capture.warn("h2 protocol error (%s) on %s", kind, t.host)
	t.recordParseWarning("protocol error: " + kind)
}

// upstreamProtocolError is the transport-side counterpart. A normal tunnel
// teardown surfaces as a read-loop close (EOF, closed connection), which is not
// a parse anomaly and must not create a warning event.
func (t *h2Tunnel) upstreamProtocolError(kind string) {
	if benignH2Teardown(kind) {
		return
	}
	t.capture.warn("upstream h2 protocol error (%s) on %s", kind, t.host)
	t.recordParseWarning("upstream protocol error: " + kind)
}

func benignH2Teardown(kind string) bool {
	switch kind {
	case "read_frame_eof", "read_frame_unexpected_eof", "read_frame_other", "read_frame_conn_error_NO_ERROR":
		return true
	default:
		return false
	}
}

// recordParseWarning persists one defensive mitm_error for the tunnel. Raw
// frames captured by the taps are written to body files so the failure can be
// inspected without reproducing it. Normal client disconnects are filtered out
// by x/net/http2 before they reach ErrorLog.
func (t *h2Tunnel) recordParseWarning(message string) {
	if message == "" {
		return
	}
	t.warnOnce.Do(func() {
		t.capture.warn("h2 protocol warning for %s: %s", t.host, message)
		rec, err := t.capture.store.Begin(session.RecordSpec{
			Kind:       session.KindMITMError,
			Started:    time.Now(),
			ClientAddr: t.clientAddr,
			TargetHost: t.host,
			Reason:     MITMReasonH2ParseError,
		})
		if err != nil {
			t.capture.warn("create h2 warning event for %s: %v", t.host, err)
			return
		}
		defer rec.Abort()
		details := map[string]any{"error": message}
		t.storeTap(rec, details, "client_read", t.clientTap.readBuffer)
		t.storeTap(rec, details, "client_write", t.clientTap.writeBuffer)
		t.storeTap(rec, details, "upstream_read", t.upstreamTap.readBuffer)
		t.storeTap(rec, details, "upstream_write", t.upstreamTap.writeBuffer)
		rec.Event().Details = details
		if err := rec.Commit(t.capture.publish); err != nil {
			t.capture.warn("commit h2 warning for %s: %v", t.host, err)
		}
	})
}

func (t *h2Tunnel) storeTap(rec *session.Recorder, details map[string]any, name string, buffer *tailBuffer) {
	data := buffer.Snapshot()
	if len(data) == 0 {
		return
	}
	direction := "h2-" + name
	file, err := rec.Body(direction)
	if err != nil {
		t.capture.warn("create h2 %s body for %s: %v", name, rec.Event().ID, err)
		return
	}
	if _, err := file.Write(data); err != nil {
		t.capture.warn("write h2 %s body for %s: %v", name, rec.Event().ID, err)
		return
	}
	size, err := file.Size()
	if err != nil {
		t.capture.warn("stat h2 %s body for %s: %v", name, rec.Event().ID, err)
		return
	}
	details[name+"_path"] = t.capture.store.BodyPath(rec.Event().ID, direction)
	details[name+"_size"] = size
}

// tapConn records the tail of both directions without changing stream
// semantics. ConnectionState is forwarded so x/net/http2 still sees the
// terminated TLS session.
type tapConn struct {
	net.Conn
	readBuffer  *tailBuffer
	writeBuffer *tailBuffer
}

func newTapConn(conn net.Conn) *tapConn {
	return &tapConn{
		Conn:        conn,
		readBuffer:  newTailBuffer(h2TapLimit),
		writeBuffer: newTailBuffer(h2TapLimit),
	}
}

func (c *tapConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.readBuffer.Write(p[:n])
	}
	return n, err
}

func (c *tapConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.writeBuffer.Write(p[:n])
	}
	return n, err
}

func (c *tapConn) ConnectionState() tls.ConnectionState {
	if stater, ok := c.Conn.(interface{ ConnectionState() tls.ConnectionState }); ok {
		return stater.ConnectionState()
	}
	return tls.ConnectionState{}
}

// tailBuffer retains the most recent limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{limit: limit}
}

func (b *tailBuffer) Write(p []byte) {
	if b.limit <= 0 || len(p) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return
	}
	if overflow := len(b.data) + len(p) - b.limit; overflow > 0 {
		b.data = append(b.data[:0], b.data[overflow:]...)
	}
	b.data = append(b.data, p...)
}

func (b *tailBuffer) Snapshot() []byte {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out
}
