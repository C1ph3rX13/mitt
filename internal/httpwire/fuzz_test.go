package httpwire

import (
	"bufio"
	"bytes"
	"io"
	"testing"
)

func FuzzReadRequest(f *testing.F) {
	seeds := []string{
		"GET / HTTP/1.1\r\nHost: example.test\r\n\r\n",
		"POST / HTTP/1.1\r\nContent-Length: 5\r\n\r\nhello",
		"POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
		"POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\nContent-Length: 5\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		reader := bufio.NewReader(bytes.NewReader(data))
		request, err := ReadRequest(reader)
		if err != nil {
			return
		}
		_, _ = request.ReadBody(reader, io.Discard)
	})
}

func FuzzReadResponse(f *testing.F) {
	seeds := []string{
		"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\n\r\n",
		"HTTP/1.0 200 OK\r\nConnection: close\r\n\r\nclose-body",
		"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok",
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		reader := bufio.NewReader(bytes.NewReader(data))
		response, err := ReadResponse(reader, "GET")
		if err != nil {
			return
		}
		_, _ = response.ReadBody(reader, io.Discard)
	})
}
