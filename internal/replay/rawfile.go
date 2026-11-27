package replay

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/httpwire"
)

// rawHTTPRequest is a parsed raw HTTP/1.x request. Body is the decoded entity
// body used for history persistence; the caller keeps the original bytes for
// verbatim dispatch. head is the parsed message those bytes render, so the
// recorded identity is read from the same head that is sent.
type rawHTTPRequest struct {
	line      string
	method    string
	target    string
	version   string
	headers   [][2]string
	body      []byte
	targetURL *url.URL
	head      *httpwire.Request
}

func parseRawHTTPRequest(data []byte) (*rawHTTPRequest, error) {
	reader := bufio.NewReader(bytes.NewReader(data))
	request, err := httpwire.ReadRequest(reader)
	if err != nil {
		if errors.Is(err, io.EOF) {
			if len(data) == 0 {
				return nil, errors.New("empty HTTP request")
			}
			return nil, errors.New("HTTP request has no header terminator")
		}
		return nil, err
	}

	var body bytes.Buffer
	if _, err := request.ReadBody(reader, &body); err != nil {
		return nil, fmt.Errorf("decode request body: %w", err)
	}
	if _, err := reader.Peek(1); err == nil {
		return nil, errors.New("unexpected bytes after request body; add Content-Length or Transfer-Encoding")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}

	targetURL, err := deriveRequestURL(request.Target, request.Headers)
	if err != nil {
		return nil, err
	}
	return &rawHTTPRequest{
		line:      request.StartLine,
		method:    request.Method,
		target:    request.Target,
		version:   request.Version,
		headers:   request.Headers,
		body:      body.Bytes(),
		targetURL: targetURL,
		head:      request,
	}, nil
}

func deriveRequestURL(target string, headers [][2]string) (*url.URL, error) {
	if parsed, err := url.Parse(target); err == nil && parsed.IsAbs() {
		if err := validateURL(parsed); err != nil {
			return nil, err
		}
		return parsed, nil
	}

	if !strings.HasPrefix(target, "/") && target != "*" {
		return nil, fmt.Errorf("request target %q is neither an absolute URL nor origin-form", target)
	}
	authority := httpwire.First(headers, "Host")
	if authority == "" {
		return nil, errors.New("origin-form request target has no Host header")
	}
	scheme := "http"
	base, err := url.Parse(scheme + "://" + authority)
	if err != nil || base.Host == "" || base.Hostname() == "" {
		return nil, fmt.Errorf("invalid Host header %q", authority)
	}
	if base.Port() == "443" {
		base.Scheme = "https"
	}
	if target == "*" {
		base.Path = "*"
		if err := validateURL(base); err != nil {
			return nil, err
		}
		return base, nil
	}
	reference, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("invalid request target %q: %w", target, err)
	}
	base.Path = reference.Path
	base.RawPath = reference.RawPath
	base.RawQuery = reference.RawQuery
	base.ForceQuery = reference.ForceQuery
	if err := validateURL(base); err != nil {
		return nil, err
	}
	return base, nil
}

func validateURL(parsed *url.URL) error {
	if parsed == nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" {
		return errors.New("URL must be absolute and include a host")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("URL scheme %q is not http or https", parsed.Scheme)
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("invalid URL port %q", port)
		}
	}
	return nil
}
