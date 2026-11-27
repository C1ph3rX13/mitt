package httpwire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

type requestCase struct {
	name            string
	raw             string
	wantBody        string
	wantHeadErr     error
	wantHeadText    string
	wantBodyErr     error
	wantBodyErrText string
}

func TestReadRequest(t *testing.T) {
	tests := []requestCase{
		{
			name:     "content-length body",
			raw:      "POST /submit HTTP/1.1\r\nHost: example.test\r\nContent-Length: 5\r\n\r\nhello",
			wantBody: "hello",
		},
		{
			name:     "content-length zero",
			raw:      "POST /submit HTTP/1.1\r\nHost: example.test\r\nContent-Length: 0\r\n\r\n",
			wantBody: "",
		},
		{
			name:     "no body headers",
			raw:      "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n",
			wantBody: "",
		},
		{
			name:     "chunked with trailer",
			raw:      "POST /submit HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\nX-Trailer: ignored\r\n\r\n",
			wantBody: "hello world",
		},
		{
			name:     "chunked empty",
			raw:      "POST /submit HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
			wantBody: "",
		},
		{
			name:     "transfer-encoding wins over content-length",
			raw:      "POST /submit HTTP/1.1\r\nTransfer-Encoding: chunked\r\nContent-Length: 999\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
			wantBody: "hello",
		},
		{
			name:     "lf line endings",
			raw:      "POST /submit HTTP/1.1\nHost: example.test\nContent-Length: 5\n\nhello",
			wantBody: "hello",
		},
		{
			name:     "obs-fold continuation",
			raw:      "GET / HTTP/1.1\r\nX-Test: one\r\n two\r\n\r\n",
			wantBody: "",
		},
		{
			name:        "conflicting content-length",
			raw:         "POST / HTTP/1.1\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n",
			wantHeadErr: ErrConflictingContentLength,
		},
		{
			name:        "invalid content-length",
			raw:         "POST / HTTP/1.1\r\nContent-Length: five\r\n\r\n",
			wantHeadErr: ErrInvalidContentLength,
		},
		{
			name:        "unsupported transfer-encoding",
			raw:         "POST / HTTP/1.1\r\nTransfer-Encoding: gzip\r\n\r\n",
			wantHeadErr: ErrUnsupportedTransferEncoding,
		},
		{
			name:        "stacked transfer-encoding",
			raw:         "POST / HTTP/1.1\r\nTransfer-Encoding: gzip, chunked\r\n\r\n",
			wantHeadErr: ErrUnsupportedTransferEncoding,
		},
		{
			name:         "wrong field count",
			raw:          "GET / HTTP/1.1 extra\r\n\r\n",
			wantHeadText: "invalid request line",
		},
		{
			name:         "invalid method token",
			raw:          "G@T / HTTP/1.1\r\n\r\n",
			wantHeadText: "invalid method",
		},
		{
			name:         "invalid protocol",
			raw:          "GET / SPDY/3\r\n\r\n",
			wantHeadText: "invalid protocol",
		},
		{
			name:         "space before colon",
			raw:          "GET / HTTP/1.1\r\nHost : example.test\r\n\r\n",
			wantHeadText: "invalid header name",
		},
		{
			name:         "invalid header name",
			raw:          "GET / HTTP/1.1\r\nBad Name: value\r\n\r\n",
			wantHeadText: "invalid header name",
		},
		{
			name:         "control character in value",
			raw:          "GET / HTTP/1.1\r\nX-Test: a\x00b\r\n\r\n",
			wantHeadText: "invalid header value",
		},
		{
			name:            "truncated fixed body",
			raw:             "POST / HTTP/1.1\r\nContent-Length: 5\r\n\r\nhi",
			wantBodyErr:     io.EOF,
			wantBodyErrText: "",
		},
		{
			name:            "invalid chunk size",
			raw:             "POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n",
			wantBodyErrText: "invalid chunk size",
		},
		{
			name:            "invalid chunk terminator",
			raw:             "POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n1\r\naXX\r\n",
			wantBodyErrText: "invalid chunk terminator",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(test.raw))
			request, err := ReadRequest(reader)
			if test.wantHeadErr != nil || test.wantHeadText != "" {
				if err == nil {
					t.Fatalf("ReadRequest() error = nil, want %v", test.wantHeadErr)
				}
				if test.wantHeadErr != nil && !errors.Is(err, test.wantHeadErr) {
					t.Fatalf("ReadRequest() error = %v, want errors.Is(%v)", err, test.wantHeadErr)
				}
				if test.wantHeadText != "" && !strings.Contains(err.Error(), test.wantHeadText) {
					t.Fatalf("ReadRequest() error = %v, want text %q", err, test.wantHeadText)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadRequest() error = %v", err)
			}
			var body bytes.Buffer
			_, bodyErr := request.ReadBody(reader, &body)
			if test.wantBodyErr != nil || test.wantBodyErrText != "" {
				if bodyErr == nil {
					t.Fatalf("ReadBody() error = nil, want %v", test.wantBodyErr)
				}
				if test.wantBodyErr != nil && !errors.Is(bodyErr, test.wantBodyErr) {
					t.Fatalf("ReadBody() error = %v, want errors.Is(%v)", bodyErr, test.wantBodyErr)
				}
				if test.wantBodyErrText != "" && !strings.Contains(bodyErr.Error(), test.wantBodyErrText) {
					t.Fatalf("ReadBody() error = %v, want text %q", bodyErr, test.wantBodyErrText)
				}
				return
			}
			if bodyErr != nil {
				t.Fatalf("ReadBody() error = %v", bodyErr)
			}
			if got := body.String(); got != test.wantBody {
				t.Fatalf("body = %q, want %q", got, test.wantBody)
			}
		})
	}
}

type responseCase struct {
	name            string
	raw             string
	method          string
	wantBody        string
	wantStatus      int
	wantHeadErr     error
	wantHeadText    string
	wantBodyErr     error
	wantBodyErrText string
}

func TestReadResponse(t *testing.T) {
	tests := []responseCase{
		{
			name:       "content-length body",
			raw:        "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
			method:     "GET",
			wantBody:   "hello",
			wantStatus: 200,
		},
		{
			name:       "chunked body",
			raw:        "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n",
			method:     "GET",
			wantBody:   "abcde",
			wantStatus: 200,
		},
		{
			name:       "close-delimited body",
			raw:        "HTTP/1.0 200 OK\r\nConnection: close\r\n\r\nclose-body",
			method:     "GET",
			wantBody:   "close-body",
			wantStatus: 200,
		},
		{
			name:       "head response has no body",
			raw:        "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
			method:     "HEAD",
			wantBody:   "",
			wantStatus: 200,
		},
		{
			name:       "204 has no body",
			raw:        "HTTP/1.1 204 No Content\r\nContent-Length: 5\r\n\r\nhello",
			method:     "GET",
			wantBody:   "",
			wantStatus: 204,
		},
		{
			name:       "304 has no body",
			raw:        "HTTP/1.1 304 Not Modified\r\nContent-Length: 5\r\n\r\nhello",
			method:     "GET",
			wantBody:   "",
			wantStatus: 304,
		},
		{
			name:       "interim response is skipped",
			raw:        "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok",
			method:     "GET",
			wantBody:   "ok",
			wantStatus: 200,
		},
		{
			name:        "101 upgrade is rejected",
			raw:         "HTTP/1.1 101 Switching Protocols\r\n\r\n",
			method:      "GET",
			wantHeadErr: ErrUpgradeUnsupported,
		},
		{
			name:        "interim only reaches eof",
			raw:         "HTTP/1.1 100 Continue\r\n\r\n",
			method:      "GET",
			wantHeadErr: io.EOF,
		},
		{
			name:       "transfer-encoding wins over content-length",
			raw:        "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Length: 999\r\n\r\n2\r\nok\r\n0\r\n\r\n",
			method:     "GET",
			wantBody:   "ok",
			wantStatus: 200,
		},
		{
			name:        "unsupported transfer-encoding",
			raw:         "HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip\r\n\r\n",
			method:      "GET",
			wantHeadErr: ErrUnsupportedTransferEncoding,
		},
		{
			name:         "invalid status line",
			raw:          "HTTP/1.1\r\n\r\n",
			method:       "GET",
			wantHeadText: "invalid status line",
		},
		{
			name:         "invalid status code",
			raw:          "HTTP/1.1 99 Nope\r\n\r\n",
			method:       "GET",
			wantHeadText: "invalid status code",
		},
		{
			name:        "truncated close-delimited body",
			raw:         "HTTP/1.1 200 OK\r\n\r\npartial",
			method:      "GET",
			wantBody:    "partial",
			wantStatus:  200,
			wantBodyErr: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(test.raw))
			response, err := ReadResponse(reader, test.method)
			if test.wantHeadErr != nil || test.wantHeadText != "" {
				if err == nil {
					t.Fatalf("ReadResponse() error = nil, want %v", test.wantHeadErr)
				}
				if test.wantHeadErr != nil && !errors.Is(err, test.wantHeadErr) {
					t.Fatalf("ReadResponse() error = %v, want errors.Is(%v)", err, test.wantHeadErr)
				}
				if test.wantHeadText != "" && !strings.Contains(err.Error(), test.wantHeadText) {
					t.Fatalf("ReadResponse() error = %v, want text %q", err, test.wantHeadText)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadResponse() error = %v", err)
			}
			if response.Status != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Status, test.wantStatus)
			}
			var body bytes.Buffer
			_, bodyErr := response.ReadBody(reader, &body)
			if bodyErr != nil {
				t.Fatalf("ReadBody() error = %v", bodyErr)
			}
			if got := body.String(); got != test.wantBody {
				t.Fatalf("body = %q, want %q", got, test.wantBody)
			}
		})
	}
}

func TestReadLimits(t *testing.T) {
	t.Run("line too long", func(t *testing.T) {
		raw := strings.Repeat("a", maxLineBytes+1) + "\r\n\r\n"
		_, err := ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("ReadRequest() error = %v, want ErrLineTooLong", err)
		}
	})
	t.Run("header line too long", func(t *testing.T) {
		raw := "GET / HTTP/1.1\r\nX-Test: " + strings.Repeat("a", maxLineBytes) + "\r\n\r\n"
		_, err := ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("ReadRequest() error = %v, want ErrLineTooLong", err)
		}
	})
	t.Run("headers too large", func(t *testing.T) {
		var builder strings.Builder
		builder.WriteString("GET / HTTP/1.1\r\n")
		for index := 0; index < 20; index++ {
			builder.WriteString("X-Test: ")
			builder.WriteString(strings.Repeat("a", 60<<10))
			builder.WriteString("\r\n")
		}
		builder.WriteString("\r\n")
		_, err := ReadRequest(bufio.NewReader(strings.NewReader(builder.String())))
		if !errors.Is(err, ErrHeadersTooLarge) {
			t.Fatalf("ReadRequest() error = %v, want ErrHeadersTooLarge", err)
		}
	})
}

func TestWrite(t *testing.T) {
	tests := []struct {
		name       string
		startLine  string
		headers    [][2]string
		body       string
		size       int64
		want       string
		wantErr    error
		isResponse bool
		method     string
	}{
		{
			name:      "content-length body",
			startLine: "POST /submit HTTP/1.1",
			headers:   [][2]string{{"Host", "example.test"}, {"Content-Length", "5"}},
			body:      "hello",
			size:      5,
			want:      "POST /submit HTTP/1.1\r\nHost: example.test\r\nContent-Length: 5\r\n\r\nhello",
		},
		{
			name:      "no body",
			startLine: "GET / HTTP/1.1",
			headers:   [][2]string{{"Host", "example.test"}},
			want:      "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n",
		},
		{
			name:      "chunked single chunk",
			startLine: "POST /submit HTTP/1.1",
			headers:   [][2]string{{"Transfer-Encoding", "chunked"}},
			body:      "hello",
			size:      5,
			want:      "POST /submit HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
		},
		{
			name:      "chunked empty",
			startLine: "POST /submit HTTP/1.1",
			headers:   [][2]string{{"Transfer-Encoding", "chunked"}},
			size:      0,
			want:      "POST /submit HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
		},
		{
			name:      "stored h2 request without content-length",
			startLine: "POST /submit HTTP/2.0",
			headers:   [][2]string{{"Host", "example.test"}},
			body:      "hello",
			size:      5,
			want:      "POST /submit HTTP/2.0\r\nHost: example.test\r\n\r\nhello",
		},
		{
			name:       "head response never writes a body",
			startLine:  "HTTP/1.1 200 OK",
			headers:    [][2]string{{"Content-Length", "5"}},
			body:       "hello",
			size:       5,
			isResponse: true,
			method:     "HEAD",
			want:       "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n",
		},
		{
			name:       "204 response never writes a body",
			startLine:  "HTTP/1.1 204 No Content",
			headers:    [][2]string{{"Content-Length", "5"}},
			body:       "hello",
			size:       5,
			isResponse: true,
			method:     "GET",
			want:       "HTTP/1.1 204 No Content\r\nContent-Length: 5\r\n\r\n",
		},
		{
			name:       "close-delimited response",
			startLine:  "HTTP/1.0 200 OK",
			headers:    [][2]string{{"Connection", "close"}},
			body:       "close-body",
			size:       int64(len("close-body")),
			isResponse: true,
			method:     "GET",
			want:       "HTTP/1.0 200 OK\r\nConnection: close\r\n\r\nclose-body",
		},
		{
			name:      "short body errors",
			startLine: "POST /submit HTTP/1.1",
			headers:   [][2]string{{"Content-Length", "5"}},
			body:      "hi",
			size:      5,
			wantErr:   io.EOF,
		},
		{
			name:      "negative size errors",
			startLine: "POST /submit HTTP/1.1",
			headers:   [][2]string{{"Content-Length", "5"}},
			body:      "hello",
			size:      -1,
			wantErr:   errors.New("invalid body size"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var message *Message
			if test.isResponse {
				response, err := ParseResponse(test.startLine, test.headers, test.method)
				if err != nil {
					t.Fatalf("ParseResponse() error = %v", err)
				}
				message = &response.Message
			} else {
				request, err := ParseRequest(test.startLine, test.headers)
				if err != nil {
					t.Fatalf("ParseRequest() error = %v", err)
				}
				message = &request.Message
			}
			var output bytes.Buffer
			err := message.Write(&output, strings.NewReader(test.body), test.size)
			if test.wantErr != nil {
				if err == nil {
					t.Fatalf("Write() error = nil, want %v", test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			if got := output.String(); got != test.want {
				t.Fatalf("Write() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWritePreservesHeaderOrderAndDuplicates(t *testing.T) {
	request, err := ParseRequest("GET / HTTP/1.1", [][2]string{
		{"X-A", "one"},
		{"X-B", "two"},
		{"X-A", "three"},
	})
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	var output bytes.Buffer
	if err := request.Write(&output, nil, 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	want := "GET / HTTP/1.1\r\nX-A: one\r\nX-B: two\r\nX-A: three\r\n\r\n"
	if got := output.String(); got != want {
		t.Fatalf("Write() = %q, want %q", got, want)
	}
}

func TestWriteNormalizesLFToCRLF(t *testing.T) {
	request, err := ReadRequest(bufio.NewReader(strings.NewReader("GET / HTTP/1.1\nHost: example.test\n\n")))
	if err != nil {
		t.Fatalf("ReadRequest() error = %v", err)
	}
	var output bytes.Buffer
	if err := request.Write(&output, nil, 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	want := "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"
	if got := output.String(); got != want {
		t.Fatalf("Write() = %q, want %q", got, want)
	}
}

func TestFramingHeaders(t *testing.T) {
	request, err := ParseRequest("POST / HTTP/1.1", [][2]string{
		{"Host", "example.test"},
		{"Transfer-Encoding", "chunked"},
		{"Content-Length", "999"},
		{"X-Test", "value"},
	})
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	safe := request.FramingHeaders()
	if Has(safe, "Content-Length") {
		t.Fatalf("FramingHeaders() kept Content-Length: %v", safe)
	}
	if !Has(request.Headers, "Content-Length") {
		t.Fatal("FramingHeaders() modified the message headers")
	}
	if got := First(safe, "X-Test"); got != "value" {
		t.Fatalf("FramingHeaders() X-Test = %q, want value", got)
	}

	safe[0][1] = "mutated"
	if got := First(request.Headers, "Host"); got != "example.test" {
		t.Fatalf("FramingHeaders() aliased the message headers: Host = %q", got)
	}
}

func TestHeaderHelpers(t *testing.T) {
	headers := [][2]string{
		{"X-Test", "one"},
		{"Host", "example.test"},
		{"x-test", "two"},
	}
	if got := Values(headers, "X-Test"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("Values() = %v, want [one two]", got)
	}
	if !Has(headers, "host") {
		t.Fatal("Has() = false, want true")
	}
	if got := First(headers, "x-test"); got != "one" {
		t.Fatalf("First() = %q, want one", got)
	}
	stripped := Strip(headers, "X-Test")
	if len(stripped) != 1 || stripped[0][0] != "Host" {
		t.Fatalf("Strip() = %v, want only Host", stripped)
	}
	if len(headers) != 3 {
		t.Fatalf("Strip() modified the input: %v", headers)
	}
	if ValidToken("X-Test_1") != true || ValidToken("Bad Name") != false {
		t.Fatal("ValidToken() did not enforce the RFC token rule")
	}
	if ValidHeaderValue("one\ttwo") != true || ValidHeaderValue("one\x01two") != false {
		t.Fatal("ValidHeaderValue() did not enforce the header value rule")
	}
}

func TestForwardedFiltersFramingAndKeepsTheRule(t *testing.T) {
	request, err := ParseRequest("POST /in HTTP/1.1", [][2]string{
		{"Transfer-Encoding", "chunked"},
		{"Content-Length", "999"},
		{"X-Marker", "drop-me"},
	})
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}

	// The caller filters its own fields and then hands over the finished set.
	// Forwarded must filter framing headers on THAT set and keep the received
	// rule, so a chunked request stays chunked without a stale Content-Length.
	filtered := [][2]string{
		{"Host", "example.test"},
		{"Transfer-Encoding", "chunked"},
		{"Content-Length", "999"},
	}
	forwarded := request.Forwarded("POST /out HTTP/1.1", filtered)

	wantHeaders := [][2]string{
		{"Host", "example.test"},
		{"Transfer-Encoding", "chunked"},
	}
	if !reflect.DeepEqual(forwarded.Headers, wantHeaders) {
		t.Fatalf("Forwarded() headers = %v, want %v", forwarded.Headers, wantHeaders)
	}
	if got := First(forwarded.Headers, "X-Marker"); got != "" {
		t.Fatalf("Forwarded() resurrected a field the caller dropped: %q", got)
	}
	if got := forwarded.StartLine; got != "POST /out HTTP/1.1" {
		t.Fatalf("Forwarded() start line = %q", got)
	}
	if !forwarded.framing.chunked {
		t.Fatal("Forwarded() dropped the received chunked rule")
	}

	// Forwarded.Headers is exactly what Write emits, so recording them cannot
	// disagree with the bytes on the wire.
	var output bytes.Buffer
	if err := forwarded.Write(&output, strings.NewReader("hi"), 2); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	want := "POST /out HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nhi\r\n0\r\n\r\n"
	if got := output.String(); got != want {
		t.Fatalf("Write() = %q, want %q", got, want)
	}
}

func TestWithBodyLengthReplacesChunkedInPlace(t *testing.T) {
	request, err := ParseRequest("POST /submit HTTP/1.1", [][2]string{
		{"Host", "example.test"},
		{"Transfer-Encoding", "chunked"},
		{"Connection", "close"},
	})
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	headers := WithBodyLength(request.Headers, 11)

	// The replacement keeps the position Transfer-Encoding occupied, which is
	// what the on-wire header order contract requires.
	want := [][2]string{
		{"Host", "example.test"},
		{"Content-Length", "11"},
		{"Connection", "close"},
	}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("WithBodyLength() headers = %v, want %v", headers, want)
	}
	if !Has(request.Headers, "Transfer-Encoding") {
		t.Fatal("WithBodyLength() modified the input")
	}

	framed, err := ParseRequest(request.StartLine, headers)
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	var output bytes.Buffer
	if err := framed.Write(&output, strings.NewReader("hello world"), 11); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	wantWire := "POST /submit HTTP/1.1\r\nHost: example.test\r\nContent-Length: 11\r\nConnection: close\r\n\r\nhello world"
	if got := output.String(); got != wantWire {
		t.Fatalf("Write() = %q, want %q", got, wantWire)
	}
}

func TestWithBodyLengthCollapsesContentLength(t *testing.T) {
	request, err := ParseRequest("POST / HTTP/1.1", [][2]string{
		{"Content-Length", "5"},
		{"X-Test", "value"},
		{"Content-Length", "5"},
	})
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	headers := WithBodyLength(request.Headers, 0)
	want := [][2]string{
		{"Content-Length", "0"},
		{"X-Test", "value"},
	}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("WithBodyLength(0) headers = %v, want %v", headers, want)
	}
	// A zero length must still be framed: the head declares the body size.
	framed, err := ParseRequest(request.StartLine, headers)
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	var output bytes.Buffer
	if err := framed.Write(&output, nil, 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	wantWire := "POST / HTTP/1.1\r\nContent-Length: 0\r\nX-Test: value\r\n\r\n"
	if got := output.String(); got != wantWire {
		t.Fatalf("Write() = %q, want %q", got, wantWire)
	}
}

func TestWithBodyLengthAppendsWhenUnframed(t *testing.T) {
	request, err := ParseRequest("GET / HTTP/1.1", [][2]string{{"Host", "example.test"}})
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	headers := WithBodyLength(request.Headers, 4)
	want := [][2]string{
		{"Host", "example.test"},
		{"Content-Length", "4"},
	}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("WithBodyLength() headers = %v, want %v", headers, want)
	}
	framed, err := ParseRequest(request.StartLine, headers)
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	var output bytes.Buffer
	if err := framed.Write(&output, strings.NewReader("body"), 4); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !strings.HasSuffix(output.String(), "\r\n\r\nbody") {
		t.Fatalf("Write() = %q, want an identity body", output.String())
	}
}

func TestWriteSuppressesBodyForForbiddenResponses(t *testing.T) {
	for _, test := range []struct {
		method string
		status string
	}{
		{"HEAD", "HTTP/1.1 200 OK"},
		{"GET", "HTTP/1.1 204 No Content"},
		{"GET", "HTTP/1.1 304 Not Modified"},
	} {
		response, err := ParseResponse(test.status, [][2]string{{"Content-Length", "10"}}, test.method)
		if err != nil {
			t.Fatalf("ParseResponse(%q) error = %v", test.status, err)
		}
		var output bytes.Buffer
		if err := response.Write(&output, strings.NewReader("ignored"), 7); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if got := output.String(); got != test.status+"\r\nContent-Length: 10\r\n\r\n" {
			t.Fatalf("%s %s: Write() = %q, want no body", test.method, test.status, got)
		}
	}
}

func TestWithHostRewritesAndAppends(t *testing.T) {
	request, err := ParseRequest("GET / HTTP/1.1", [][2]string{
		{"X-A", "one"},
		{"Host", "old.test"},
		{"Host", "second.test"},
	})
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	rewritten := WithHost(request.Headers, "new.test:8443")
	want := [][2]string{
		{"X-A", "one"},
		{"Host", "new.test:8443"},
		{"Host", "new.test:8443"},
	}
	if !reflect.DeepEqual(rewritten, want) {
		t.Fatalf("WithHost() headers = %v, want %v (position and multiplicity kept)", rewritten, want)
	}
	if got := First(request.Headers, "Host"); got != "old.test" {
		t.Fatalf("WithHost() modified the input: Host = %q", got)
	}

	noHost, err := ParseRequest("GET / HTTP/1.1", [][2]string{{"X-A", "one"}})
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	appended := WithHost(noHost.Headers, "added.test")
	if got := First(appended, "Host"); got != "added.test" {
		t.Fatalf("WithHost() appended Host = %q, want added.test", got)
	}
	if len(appended) != 2 {
		t.Fatalf("WithHost() headers = %v, want one appended Host", appended)
	}

	kept := WithHost(noHost.Headers, "")
	if !reflect.DeepEqual(kept, noHost.Headers) {
		t.Fatalf("WithHost(\"\") headers = %v, want unchanged", kept)
	}
}

func TestParseResponseRejectsInterim(t *testing.T) {
	_, err := ParseResponse("HTTP/1.1 100 Continue", nil, "GET")
	if err == nil || !strings.Contains(err.Error(), "not a final response") {
		t.Fatalf("ParseResponse() error = %v, want an interim-response error", err)
	}
}
