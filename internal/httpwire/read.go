package httpwire

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	maxLineBytes   = 64 << 10
	maxHeaderBytes = 1 << 20
)

// ReadRequest reads one request head. The body is left unread on br;
// call req.ReadBody to copy it. The returned Request carries the framing rule
// derived from the request headers.
func ReadRequest(br *bufio.Reader) (*Request, error) {
	line, err := readLine(br, maxLineBytes)
	if err != nil {
		return nil, err
	}
	headers, err := readHeaders(br)
	if err != nil {
		return nil, err
	}
	return ParseRequest(line, headers)
}

// ReadResponse reads one final response head, consuming interim 1xx responses.
// The body is left unread on br; call resp.ReadBody to copy it.
func ReadResponse(br *bufio.Reader, method string) (*Response, error) {
	for {
		line, err := readLine(br, maxLineBytes)
		if err != nil {
			return nil, err
		}
		status, err := parseStatusLine(line)
		if err != nil {
			return nil, err
		}
		headers, err := readHeaders(br)
		if err != nil {
			return nil, err
		}
		if status >= 100 && status < 200 {
			if status == 101 {
				return nil, ErrUpgradeUnsupported
			}
			continue
		}
		return ParseResponse(line, headers, method)
	}
}

// ReadBody copies the message body into dst using the framing rule the message
// was parsed with. It never reads past the body, so the next message on a
// keep-alive connection stays in br.
func (m *Message) ReadBody(br *bufio.Reader, dst io.Writer) (int64, error) {
	return copyBody(dst, br, m.framing)
}

func readHeaders(br *bufio.Reader) ([][2]string, error) {
	headers := [][2]string{}
	total := 0
	for {
		line, err := readLine(br, maxLineBytes)
		if err != nil {
			return nil, err
		}
		total += len(line) + 2
		if total > maxHeaderBytes {
			return nil, ErrHeadersTooLarge
		}
		if line == "" {
			return headers, nil
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(headers) == 0 {
				return nil, errors.New("header continuation without a header")
			}
			fragment := strings.TrimSpace(line)
			if !ValidHeaderValue(fragment) {
				return nil, fmt.Errorf("invalid header value for %q: control character", headers[len(headers)-1][0])
			}
			headers[len(headers)-1][1] += " " + fragment
			continue
		}
		index := strings.IndexByte(line, ':')
		if index <= 0 {
			return nil, fmt.Errorf("malformed header line %q", line)
		}
		name := line[:index]
		if !ValidToken(name) {
			return nil, fmt.Errorf("invalid header name %q", name)
		}
		value := strings.TrimSpace(line[index+1:])
		if !ValidHeaderValue(value) {
			return nil, fmt.Errorf("invalid header value for %q: control character", name)
		}
		headers = append(headers, [2]string{name, value})
	}
}

func readLine(br *bufio.Reader, limit int) (string, error) {
	var buf []byte
	for {
		fragment, err := br.ReadSlice('\n')
		if len(buf)+len(fragment) > limit {
			return "", ErrLineTooLong
		}
		buf = append(buf, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf) == 0 {
				return "", io.EOF
			}
			return "", io.ErrUnexpectedEOF
		}
		break
	}
	buf = bytes.TrimSuffix(buf, []byte("\n"))
	buf = bytes.TrimSuffix(buf, []byte("\r"))
	return string(buf), nil
}

func parseRequestLine(line string) (method, target, version string, err error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", "", "", fmt.Errorf("invalid request line %q", line)
	}
	if !ValidToken(fields[0]) {
		return "", "", "", fmt.Errorf("invalid method %q", fields[0])
	}
	if !strings.HasPrefix(fields[2], "HTTP/") {
		return "", "", "", fmt.Errorf("invalid protocol %q", fields[2])
	}
	return fields[0], fields[1], fields[2], nil
}

func parseStatusLine(line string) (int, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, fmt.Errorf("invalid status line %q", line)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil || status < 100 || status > 999 {
		return 0, fmt.Errorf("invalid status code in %q", line)
	}
	return status, nil
}
