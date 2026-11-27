package httpwire

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseRequest builds a request head from an already-read start line and
// headers. It is the entry point for stored heads (`show --raw`, send
// encoding) and for the h2-to-h1 bridge; ReadRequest uses it for wire input.
func ParseRequest(startLine string, headers [][2]string) (*Request, error) {
	method, target, version, err := parseRequestLine(startLine)
	if err != nil {
		return nil, err
	}
	if err := validateHeaderPairs(headers); err != nil {
		return nil, err
	}
	framing, err := requestFraming(headers)
	if err != nil {
		return nil, err
	}
	return &Request{
		Message: Message{
			StartLine: startLine,
			Headers:   clonePairs(headers),
			framing:   framing,
		},
		Method:  method,
		Target:  target,
		Version: version,
	}, nil
}

// ParseResponse builds a response head from an already-read final status line
// and headers. Interim responses are rejected: callers that need to skip them
// use ReadResponse on the wire.
func ParseResponse(startLine string, headers [][2]string, method string) (*Response, error) {
	status, err := parseStatusLine(startLine)
	if err != nil {
		return nil, err
	}
	if status >= 100 && status < 200 {
		if status == 101 {
			return nil, ErrUpgradeUnsupported
		}
		return nil, fmt.Errorf("interim response %q is not a final response", startLine)
	}
	if err := validateHeaderPairs(headers); err != nil {
		return nil, err
	}
	framing, err := responseFraming(headers, method, status)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(startLine)
	return &Response{
		Message: Message{
			StartLine: startLine,
			Headers:   clonePairs(headers),
			framing:   framing,
		},
		Version: fields[0],
		Status:  status,
	}, nil
}

func validateHeaderPairs(headers [][2]string) error {
	for _, header := range headers {
		if !ValidToken(header[0]) {
			return fmt.Errorf("invalid header name %q", header[0])
		}
		if !ValidHeaderValue(header[1]) {
			return fmt.Errorf("invalid header value for %q: control character", header[0])
		}
	}
	return nil
}

func requestFraming(headers [][2]string) (bodyFraming, error) {
	transferEncoding := Values(headers, "Transfer-Encoding")
	if len(transferEncoding) > 0 {
		if err := requireChunked(transferEncoding); err != nil {
			return bodyFraming{}, err
		}
		// Transfer-Encoding wins over Content-Length (RFC 9112 §6.3).
		return bodyFraming{chunked: true, hasBody: true}, nil
	}
	length, present, err := contentLength(headers)
	if err != nil {
		return bodyFraming{}, err
	}
	if !present || length == 0 {
		return bodyFraming{}, nil
	}
	return bodyFraming{length: length, hasBody: true}, nil
}

func responseFraming(headers [][2]string, method string, status int) (bodyFraming, error) {
	if strings.EqualFold(method, "HEAD") || status == 204 || status == 304 {
		return bodyFraming{bodyForbidden: true}, nil
	}
	transferEncoding := Values(headers, "Transfer-Encoding")
	if len(transferEncoding) > 0 {
		if err := requireChunked(transferEncoding); err != nil {
			return bodyFraming{}, err
		}
		return bodyFraming{chunked: true, hasBody: true}, nil
	}
	length, present, err := contentLength(headers)
	if err != nil {
		return bodyFraming{}, err
	}
	if present {
		return bodyFraming{length: length, hasBody: length > 0}, nil
	}
	return bodyFraming{hasBody: true, closeDelimited: true}, nil
}

func requireChunked(values []string) error {
	tokens := []string{}
	for _, value := range values {
		tokens = append(tokens, splitTokens(value)...)
	}
	if len(tokens) != 1 || tokens[0] != "chunked" {
		return ErrUnsupportedTransferEncoding
	}
	return nil
}

func contentLength(headers [][2]string) (int64, bool, error) {
	values := Values(headers, "Content-Length")
	if len(values) == 0 {
		return 0, false, nil
	}
	var length int64 = -1
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			parsed, err := strconv.ParseInt(part, 10, 64)
			if err != nil || parsed < 0 {
				return 0, false, fmt.Errorf("%w %q", ErrInvalidContentLength, part)
			}
			if length >= 0 && length != parsed {
				return 0, false, ErrConflictingContentLength
			}
			length = parsed
		}
	}
	return length, true, nil
}

func copyBody(dst io.Writer, src *bufio.Reader, framing bodyFraming) (int64, error) {
	switch {
	case !framing.hasBody:
		return 0, nil
	case framing.chunked:
		return copyChunked(dst, src)
	case framing.closeDelimited:
		return io.Copy(dst, src)
	case framing.length == 0:
		return 0, nil
	default:
		return io.CopyN(dst, src, framing.length)
	}
}

func copyChunked(dst io.Writer, reader *bufio.Reader) (int64, error) {
	var total int64
	for {
		line, err := readLine(reader, maxLineBytes)
		if err != nil {
			return total, err
		}
		sizeText := line
		if index := strings.IndexByte(sizeText, ';'); index >= 0 {
			sizeText = sizeText[:index]
		}
		sizeText = strings.TrimSpace(sizeText)
		size, err := strconv.ParseUint(sizeText, 16, 63)
		if err != nil {
			return total, fmt.Errorf("invalid chunk size %q", sizeText)
		}
		if size == 0 {
			for {
				trailer, err := readLine(reader, maxLineBytes)
				if err != nil {
					return total, err
				}
				if trailer == "" {
					return total, nil
				}
			}
		}
		written, err := io.CopyN(dst, reader, int64(size))
		total += written
		if err != nil {
			return total, err
		}
		terminator, err := readLine(reader, maxLineBytes)
		if err != nil {
			return total, err
		}
		if terminator != "" {
			return total, fmt.Errorf("invalid chunk terminator %q", terminator)
		}
	}
}
