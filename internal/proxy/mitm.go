//go:build linux

package proxy

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/C1ph3rX13/mitt/internal/httpwire"
)

const connectDefaultPort = "443"

// mitmUpstream is the verified TLS connection to the origin server backing one
// CONNECT tunnel. The verified certificate fingerprint is copied onto every
// captured event in that tunnel.
type mitmUpstream struct {
	conn        net.Conn
	reader      *bufio.Reader
	origin      string
	fingerprint string
	// protocol is the ALPN protocol negotiated with the origin, normalized to
	// "http/1.1" when the server does not speak ALPN.
	protocol string
	// alpn is the raw ALPN result. An empty value means the origin did not
	// select a protocol, which is how an h2-only origin can accept an
	// HTTP/1.1 ClientHello before failing the first h1 request.
	alpn    string
	host    string
	address string
}

func (u *mitmUpstream) Close() error { return u.conn.Close() }

// newOrigin renders the absolute origin of a request-target received inside
// this tunnel. The tunnel owns the scheme and authority: CONNECT fixed both
// before the first tunnel byte was read, so a request cannot address another
// origin, and both protocol handlers take their URL from here.
func (u *mitmUpstream) newOrigin(target string) (httpwire.Origin, error) {
	return httpwire.NewOrigin("https", u.address, target)
}

// authorityMismatch reports whether a request names an authority other than the
// one this tunnel was opened for. HTTP/1.1 spells it as Host, HTTP/2 as
// :authority; both handlers ask this one question, so a tunnel cannot record a
// URL against the CONNECT authority while forwarding to a different host.
// An absent authority is not a mismatch: HTTP/1.0 clients may omit Host.
func (u *mitmUpstream) authorityMismatch(authority string) bool {
	if authority == "" {
		return false
	}
	return normalizeHost(authority) != u.host
}

// bufferedConn hands the already-buffered CONNECT bytes to tls.Server without
// losing bytes that were read past the request head.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// handleConnect establishes and verifies the upstream TLS connection first,
// answers 200 only after verification succeeds, then terminates the client TLS
// handshake with a freshly signed leaf certificate. Any failure closes the
// tunnel; encrypted bytes are never relayed.
func (c *Capture) handleConnect(client net.Conn, reader *bufio.Reader, writer *bufio.Writer, head *httpwire.Request, canIdle, markActive func() bool) {
	clientAddr := client.RemoteAddr().String()
	sink := h1ReceiptSink{writer: writer}
	host, port, err := connectAuthority(head.Target)
	if err != nil {
		c.reject(sink, clientAddr, "", newFailure(failInvalidConnectTarget).withError(err))
		return
	}
	if decision := c.scope.Check(host); !decision.Allowed {
		c.reject(sink, clientAddr, host, newFailure(failScopeDenied).withDetails(scopeDetails(decision)))
		return
	}
	address := net.JoinHostPort(host, port)
	mode := c.http2ALPN
	if mode == "" {
		mode = ALPNModeH1
	}

	rawUpstream, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		c.warn("dial upstream %s for CONNECT: %v", address, err)
		c.reject(sink, clientAddr, host, newFailure(failUpstreamConnect).withDetails(map[string]any{
			"address": address,
			"error":   err.Error(),
		}))
		return
	}
	_ = rawUpstream.SetDeadline(time.Now().Add(upstreamTimeout))
	upstreamTLS := tls.Client(rawUpstream, &tls.Config{
		ServerName: host,
		NextProtos: mode.UpstreamProtocols(),
	})
	if err := upstreamTLS.Handshake(); err != nil {
		c.warn("verify upstream TLS for %s: %v", address, err)
		fail := newFailure(failUpstreamTLSHandshake).withError(err)
		if h2Only, ok := c.h2OnlyFailure(mode, "", host, address, err); ok {
			fail = h2Only
		}
		_ = upstreamTLS.Close()
		c.reject(sink, clientAddr, host, fail)
		return
	}
	state := upstreamTLS.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		c.warn("upstream TLS for %s presented no certificate", address)
		_ = upstreamTLS.Close()
		c.reject(sink, clientAddr, host, newFailure(failUpstreamNoCertificate).withDetails(map[string]any{
			"error": "upstream presented no certificate",
		}))
		return
	}
	if !mode.acceptsUpstreamProtocol(state.NegotiatedProtocol) {
		// Treat a protocol we never advertised as something we cannot capture
		// rather than proxying it blindly.
		c.warn("upstream %s negotiated unexpected ALPN %q", address, state.NegotiatedProtocol)
		_ = upstreamTLS.Close()
		c.reject(sink, clientAddr, host, newFailure(failUpstreamALPN).withDetails(map[string]any{
			"error": fmt.Sprintf("unexpected upstream ALPN %q", state.NegotiatedProtocol),
		}))
		return
	}
	fingerprint, err := publicKeyFingerprint(state.PeerCertificates[0])
	if err != nil {
		c.warn("fingerprint upstream certificate for %s: %v", address, err)
		_ = upstreamTLS.Close()
		c.reject(sink, clientAddr, host, newFailure(failUpstreamFingerprint).withError(err))
		return
	}

	leaf, err := c.leaves.Certificate(host)
	if err != nil {
		c.warn("issue leaf certificate for %s: %v", host, err)
		_ = upstreamTLS.Close()
		c.reject(sink, clientAddr, host, newFailure(failLeafCertificate).withError(err))
		return
	}

	if _, err := io.WriteString(writer, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = upstreamTLS.Close()
		return
	}
	if err := writer.Flush(); err != nil {
		_ = upstreamTLS.Close()
		return
	}
	// Past the 200 the client owns a TLS stream: failures can only close it.
	sink.afterConnect = true

	_ = client.SetDeadline(time.Now().Add(upstreamTimeout))
	header, peekErr := reader.Peek(3)
	if len(header) == 0 {
		// Browsers routinely open speculative tunnels and close them before
		// sending a ClientHello. A zero-byte abort is not a certificate
		// rejection, so it must not create a misleading history event.
		c.warn("client aborted TLS handshake for %s: %v", address, peekErr)
		_ = client.Close()
		_ = upstreamTLS.Close()
		return
	}
	if ok, detail := clientHelloIsTLS(header); !ok {
		c.warn("client TLS handshake for %s: %s", address, detail)
		_ = upstreamTLS.Close()
		c.reject(sink, clientAddr, host, newFailure(failClientNotTLS).withDetails(map[string]any{"error": detail}))
		return
	}
	clientTLS := tls.Server(&bufferedConn{Conn: client, reader: reader}, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   mode.ClientProtocols(),
	})
	if err := clientTLS.Handshake(); err != nil {
		c.warn("client TLS handshake for %s: %v", address, err)
		kind := failClientCertRejected
		if isNotTLSHandshakeError(err) {
			kind = failClientNotTLS
		}
		_ = clientTLS.Close()
		_ = upstreamTLS.Close()
		c.reject(sink, clientAddr, host, newFailure(kind).withError(err))
		return
	}
	_ = clientTLS.SetDeadline(time.Time{})
	_ = upstreamTLS.SetDeadline(time.Time{})

	clientProtocol := normalizeProtocol(clientTLS.ConnectionState().NegotiatedProtocol)
	upstreamProtocol := normalizeProtocol(state.NegotiatedProtocol)
	if clientProtocol != "h2" && upstreamProtocol == "h2" {
		// The verified upstream connection speaks h2 but the client chose
		// HTTP/1.1. Reconnect upstream with HTTP/1.1 so a single tunnel never
		// mixes protocols. An h2-only origin cannot serve this client.
		replacement, replacementState, redialErr := dialVerifiedUpstream(host, address, []string{"http/1.1"})
		if redialErr != nil {
			c.warn("reconnect upstream %s over HTTP/1.1: %v", address, redialErr)
			fail := newFailure(failUpstreamRedial).withError(redialErr)
			if h2Only, ok := c.h2OnlyFailure(mode, state.NegotiatedProtocol, host, address, redialErr); ok {
				fail = h2Only
			}
			_ = upstreamTLS.Close()
			_ = clientTLS.Close()
			c.reject(sink, clientAddr, host, fail)
			return
		}
		replacementFingerprint, fingerprintErr := publicKeyFingerprint(replacementState.PeerCertificates[0])
		if fingerprintErr != nil {
			c.warn("fingerprint reconnected upstream certificate for %s: %v", address, fingerprintErr)
			_ = replacement.Close()
			_ = upstreamTLS.Close()
			_ = clientTLS.Close()
			c.reject(sink, clientAddr, host, newFailure(failUpstreamRedialFingerprint).withError(fingerprintErr))
			return
		}
		_ = upstreamTLS.Close()
		upstreamTLS = replacement
		fingerprint = replacementFingerprint
		upstreamProtocol = "http/1.1"
		state = replacementState
	}

	upstream := &mitmUpstream{
		conn:        upstreamTLS,
		reader:      bufio.NewReader(upstreamTLS),
		origin:      "https://" + address,
		fingerprint: fingerprint,
		protocol:    upstreamProtocol,
		alpn:        state.NegotiatedProtocol,
		host:        host,
		address:     address,
	}
	defer upstream.Close()
	defer clientTLS.Close()
	if clientProtocol == "h2" {
		c.serveH2Tunnel(clientTLS, upstream, canIdle, markActive)
		return
	}
	c.serveTunnel(clientTLS, upstream, canIdle, markActive)
}

// clientHelloIsTLS inspects the first client bytes without consuming them so
// plaintext sent through a CONNECT tunnel is classified as not_tls instead of
// being misreported as a rejected certificate. A zero-length header is handled
// by the caller as an aborted tunnel, not as a TLS handshake.
func clientHelloIsTLS(header []byte) (bool, string) {
	if len(header) == 0 {
		return true, ""
	}
	if header[0] != 0x16 {
		return false, fmt.Sprintf("first byte 0x%02x is not a TLS handshake record", header[0])
	}
	if len(header) >= 2 && header[1] != 0x03 {
		return false, fmt.Sprintf("TLS record version %x is not supported", header[:2])
	}
	if len(header) >= 3 && header[2] > 0x04 {
		return false, fmt.Sprintf("TLS record version %x is not supported", header[:3])
	}
	return true, ""
}

func isNotTLSHandshakeError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "first record does not look like a tls handshake")
}

// normalizeProtocol maps an absent ALPN result to the HTTP/1.1 default that a
// server without ALPN support implicitly speaks.
func normalizeProtocol(value string) string {
	if value == "" {
		return "http/1.1"
	}
	return value
}

// acceptsUpstreamProtocol reports whether an upstream ALPN result can be
// captured with the configured mode.
func (m ALPNMode) acceptsUpstreamProtocol(value string) bool {
	switch value {
	case "", "http/1.1":
		return true
	case "h2":
		return m.AllowsH2()
	default:
		return false
	}
}

// dialVerifiedUpstream opens a fresh upstream TLS connection with the standard
// certificate verification and returns its state. It is used when a tunnel has
// to switch protocol families after the client handshake.
func dialVerifiedUpstream(host, address string, nextProtos []string) (*tls.Conn, tls.ConnectionState, error) {
	raw, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		return nil, tls.ConnectionState{}, err
	}
	_ = raw.SetDeadline(time.Now().Add(upstreamTimeout))
	conn := tls.Client(raw, &tls.Config{
		ServerName: host,
		NextProtos: nextProtos,
	})
	if err := conn.Handshake(); err != nil {
		_ = conn.Close()
		return nil, tls.ConnectionState{}, err
	}
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		_ = conn.Close()
		return nil, tls.ConnectionState{}, errors.New("upstream presented no certificate")
	}
	return conn, state, nil
}

// serveTunnel runs the inner HTTP/1.1 exchange loop over the terminated client
// TLS connection and the verified upstream TLS connection.
func (c *Capture) serveTunnel(client net.Conn, upstream *mitmUpstream, canIdle, markActive func() bool) {
	reader := bufio.NewReader(client)
	writer := bufio.NewWriter(client)
	sink := h1ReceiptSink{writer: writer}
	clientAddr := client.RemoteAddr().String()

	for {
		if !canIdle() {
			return
		}
		head, err := httpwire.ReadRequest(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.reject(sink, clientAddr, upstream.host, newFailure(failMalformedRequest).withError(err))
			}
			return
		}
		if !markActive() {
			return
		}
		if strings.EqualFold(head.Method, "CONNECT") {
			c.reject(sink, clientAddr, upstream.host, newFailure(failNestedConnect))
			return
		}
		if !c.handleRequest(client, reader, writer, head, upstream) {
			return
		}
	}
}

// connectAuthority parses the authority-form CONNECT target. A missing port
// defaults to 443, including for bracketed IPv6 literals.
func connectAuthority(target string) (host, port string, err error) {
	if target == "" {
		return "", "", errors.New("empty CONNECT target")
	}
	if strings.ContainsAny(target, "/?#@") {
		return "", "", errors.New("CONNECT target must be host:port")
	}

	host, port, err = net.SplitHostPort(target)
	if err != nil {
		switch {
		case strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]"):
			host, port = target[1:len(target)-1], connectDefaultPort
		case !strings.Contains(target, ":"):
			host, port = target, connectDefaultPort
		default:
			return "", "", fmt.Errorf("parse host:port: %w", err)
		}
	}
	if host == "" {
		return "", "", errors.New("CONNECT target has an empty host")
	}
	if port == "" {
		port = connectDefaultPort
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", "", fmt.Errorf("invalid port %q", port)
	}
	return host, port, nil
}

// publicKeyFingerprint hashes the certificate's SubjectPublicKeyInfo with
// SHA-256 and returns lowercase hex.
func publicKeyFingerprint(cert *x509.Certificate) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}
