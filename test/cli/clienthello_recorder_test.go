package cli_test

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// clientHelloRecord is one recorded ClientHello. ja3 is a JA3-style
// fingerprint built from the legacy version, cipher suites, extension types,
// supported groups, and EC point formats; random bytes, session ids, key
// shares, and GREASE values are excluded so repeated connections from the same
// TLS stack compare equal.
type clientHelloRecord struct {
	ja3  string
	alpn []string
}

// helloRecordingTarget is a one-request-per-connection TLS HTTP/1.1 target that
// records the raw ClientHello of every inbound connection before handing the
// preserved bytes to crypto/tls.
type helloRecordingTarget struct {
	addr     string
	hellos   chan clientHelloRecord
	requests chan observedRequest
}

func startHelloRecordingTarget(t *testing.T, cert tls.Certificate, response string) *helloRecordingTarget {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen hello recorder: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	target := &helloRecordingTarget{
		addr:     listener.Addr().String(),
		hellos:   make(chan clientHelloRecord, 16),
		requests: make(chan observedRequest, 16),
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go target.serve(conn, cert, response)
		}
	}()
	return target
}

func (target *helloRecordingTarget) serve(conn net.Conn, cert tls.Certificate, response string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	rawRecords, handshake, err := readTLSClientHello(conn)
	if err != nil {
		return
	}
	record, err := parseClientHello(handshake)
	if err != nil {
		return
	}
	target.hellos <- record

	// Replay the consumed TLS records into crypto/tls so the connection still
	// completes a real handshake.
	server := tls.Server(&prefixedConn{Conn: conn, prefix: bytes.NewReader(rawRecords)}, &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"http/1.1"},
	})
	if err := server.Handshake(); err != nil {
		return
	}
	request, err := readObservedRequestFrom(bufio.NewReader(server))
	if err != nil {
		return
	}
	target.requests <- request
	_, _ = io.WriteString(server, response)
}

func (target *helloRecordingTarget) waitHello(t *testing.T) clientHelloRecord {
	t.Helper()
	select {
	case record := <-target.hellos:
		return record
	case <-time.After(5 * time.Second):
		t.Fatal("hello recorder did not receive a ClientHello")
		return clientHelloRecord{}
	}
}

func (target *helloRecordingTarget) waitRequest(t *testing.T) observedRequest {
	t.Helper()
	select {
	case request := <-target.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("hello recorder did not receive a request")
		return observedRequest{}
	}
}

// prefixedConn returns bytes consumed while recording before reading further
// bytes from the underlying connection.
type prefixedConn struct {
	net.Conn
	prefix *bytes.Reader
}

func (c *prefixedConn) Read(p []byte) (int, error) {
	if c.prefix.Len() > 0 {
		return c.prefix.Read(p)
	}
	return c.Conn.Read(p)
}

// readTLSClientHello consumes TLS handshake records until the complete
// ClientHello message is available. It returns the raw record bytes for replay
// into crypto/tls and the handshake message without record headers for parsing.
func readTLSClientHello(conn net.Conn) (rawRecords, handshake []byte, err error) {
	for len(handshake) < 4 {
		payload, record, err := readTLSHandshakeRecord(conn)
		if err != nil {
			return nil, nil, err
		}
		rawRecords = append(rawRecords, record...)
		handshake = append(handshake, payload...)
	}
	if handshake[0] != 1 {
		return nil, nil, fmt.Errorf("handshake type %d is not ClientHello", handshake[0])
	}
	size := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
	for len(handshake) < 4+size {
		payload, record, err := readTLSHandshakeRecord(conn)
		if err != nil {
			return nil, nil, err
		}
		rawRecords = append(rawRecords, record...)
		handshake = append(handshake, payload...)
	}
	return rawRecords, handshake[:4+size], nil
}

func readTLSHandshakeRecord(conn net.Conn) (payload, raw []byte, err error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, nil, err
	}
	if header[0] != 0x16 {
		return nil, nil, fmt.Errorf("TLS record type %d is not handshake", header[0])
	}
	length := int(binary.BigEndian.Uint16(header[3:5]))
	payload = make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, nil, err
	}
	raw = append(header, payload...)
	return payload, raw, nil
}

// parseClientHello derives the JA3-style fingerprint and advertised ALPN
// protocols from one ClientHello handshake message.
func parseClientHello(handshake []byte) (clientHelloRecord, error) {
	var record clientHelloRecord
	if len(handshake) < 4 || handshake[0] != 1 {
		return record, fmt.Errorf("not a ClientHello message")
	}
	size := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
	body := handshake[4:]
	if len(body) < size {
		return record, fmt.Errorf("ClientHello body is %d bytes, want %d", len(body), size)
	}
	reader := &helloReader{data: body[:size]}
	version := reader.uint16()
	reader.take(32)                // random
	reader.take(reader.uint8())    // session id
	ciphers := reader.uint16List() // cipher suites
	reader.take(reader.uint8())    // compression methods
	extensionReader := &helloReader{data: reader.take(int(reader.uint16()))}
	var extensions, curves []uint16
	var points []byte
	for extensionReader.remaining() >= 4 {
		extensionType := extensionReader.uint16()
		data := extensionReader.take(int(extensionReader.uint16()))
		extensions = append(extensions, extensionType)
		switch extensionType {
		case 10: // supported_groups
			curves = (&helloReader{data: data}).uint16List()
		case 11: // ec_point_formats
			count := (&helloReader{data: data}).uint8()
			points = append([]byte(nil), (&helloReader{data: data}).take(count)...)
		case 16: // application_layer_protocol_negotiation
			record.alpn = readALPNProtocols(data)
		}
	}
	record.ja3 = strings.Join([]string{
		strconv.Itoa(int(version)),
		joinUint16(ciphers),
		joinUint16(extensions),
		joinUint16(curves),
		joinBytes(points),
	}, ",")
	return record, nil
}

func readALPNProtocols(data []byte) []string {
	reader := &helloReader{data: data}
	listLength := int(reader.uint16())
	list := &helloReader{data: reader.take(listLength)}
	var protocols []string
	for list.remaining() > 0 {
		length := list.uint8()
		protocol := list.take(length)
		if len(protocol) != length {
			break
		}
		protocols = append(protocols, string(protocol))
	}
	return protocols
}

type helloReader struct {
	data []byte
	pos  int
}

func (r *helloReader) remaining() int { return len(r.data) - r.pos }

func (r *helloReader) take(n int) []byte {
	if n < 0 || r.remaining() < n {
		r.pos = len(r.data)
		return nil
	}
	value := r.data[r.pos : r.pos+n]
	r.pos += n
	return value
}

func (r *helloReader) uint8() int {
	value := r.take(1)
	if value == nil {
		return 0
	}
	return int(value[0])
}

func (r *helloReader) uint16() uint16 {
	value := r.take(2)
	if value == nil {
		return 0
	}
	return binary.BigEndian.Uint16(value)
}

// uint16List reads a uint16-length-prefixed list of uint16 values.
func (r *helloReader) uint16List() []uint16 {
	list := &helloReader{data: r.take(int(r.uint16()))}
	values := make([]uint16, 0, list.remaining()/2)
	for list.remaining() >= 2 {
		values = append(values, list.uint16())
	}
	return values
}

// isGREASEValue reports whether v is one of the TLS GREASE reservations
// (0x0a0a, 0x1a1a, ..., 0xfafa), which vary per connection and are excluded
// from the fingerprint.
func isGREASEValue(value uint16) bool {
	return value&0x0f0f == 0x0a0a && value>>8 == value&0x00ff
}

func joinUint16(values []uint16) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if isGREASEValue(value) {
			continue
		}
		parts = append(parts, strconv.Itoa(int(value)))
	}
	return strings.Join(parts, "-")
}

func joinBytes(values []byte) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(int(value)))
	}
	return strings.Join(parts, "-")
}
