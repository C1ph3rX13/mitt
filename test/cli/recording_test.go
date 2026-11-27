package cli_test

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startClosingTarget accepts a connection, reads the request, and closes
// without sending a response. It exercises the failure path where the request
// reached the upstream but no complete exchange can be recorded.
func startClosingTarget(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closing target: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				buffer := make([]byte, 64<<10)
				_, _ = conn.Read(buffer)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func startTruncatedTarget(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen truncated target: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if _, err := readObservedRequest(conn); err != nil {
					return
				}
				_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort"))
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func requireNoStoredRecords(t *testing.T, store string) {
	t.Helper()
	historyPath := filepath.Join(store, "history.jsonl")
	history, err := os.ReadFile(historyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read history: %v", err)
	}
	if strings.TrimSpace(string(history)) != "" {
		t.Errorf("failed exchange left a history line:\n%s", history)
	}

	bodiesPath := filepath.Join(store, "bodies")
	entries, err := os.ReadDir(bodiesPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read bodies: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("failed exchange left body files: %v", names)
	}
}

func TestSendUpstreamFailureLeavesNoRecord(t *testing.T) {
	env := newTestEnv(t)
	target := startClosingTarget(t)

	result := env.runWithStore(t, "send", "--url", "http://"+target+"/path", "--patch", `{"method":"POST","body":"hello"}`)
	requireExit(t, result, 75)
	if code := errorEnvelope(t, result)["code"]; code != "TEMP_FAILURE" {
		t.Errorf("error code = %v, want TEMP_FAILURE", code)
	}
	requireNoStoredRecords(t, env.store)
}

func TestSendTruncatedResponseLeavesNoRecord(t *testing.T) {
	env := newTestEnv(t)
	target := startTruncatedTarget(t)

	result := env.runWithStore(t, "send", "--url", "http://"+target+"/path", "--patch", `{"method":"POST","body":"hello"}`)
	requireExit(t, result, 75)
	if code := errorEnvelope(t, result)["code"]; code != "TEMP_FAILURE" {
		t.Errorf("error code = %v, want TEMP_FAILURE", code)
	}
	requireNoStoredRecords(t, env.store)
}

func TestProxyUpstreamFailureLeavesNoRecord(t *testing.T) {
	env := newTestEnv(t)
	target := startClosingTarget(t)
	proxyAddr, proxy := env.startCaptureProxy(t)

	request := "GET http://" + target + "/path HTTP/1.1\r\n" +
		"Host: " + target + "\r\n" +
		"Connection: close\r\n\r\n"
	response := sendRaw(t, proxyAddr, request)
	if !strings.HasPrefix(response, "HTTP/1.1 502 ") {
		t.Fatalf("proxy response = %q, want 502", firstLine(response))
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
	requireNoStoredRecords(t, env.store)

	if !strings.Contains(proxy.stderr.String(), "warning") {
		t.Errorf("proxy stderr missing the upstream failure warning:\n%s", proxy.stderr.String())
	}
}

// startDelayedRawTarget accepts one connection, reads the request, waits delay,
// then writes response. The delay exists so a test can tear the client side
// down before the proxy has a complete response to deliver: without it the
// proxy could race ahead and finish the write before the abort is observable.
// The write carries a deadline so a client that never reads cannot hold the
// helper goroutine open.
func startDelayedRawTarget(t *testing.T, delay time.Duration, response string) (string, <-chan observedRequest) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen delayed target: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	requests := make(chan observedRequest, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		request, err := readObservedRequest(conn)
		if err != nil {
			return
		}
		requests <- request
		select {
		case <-time.After(delay):
		case <-t.Context().Done():
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(conn, response)
	}()
	return listener.Addr().String(), requests
}

// TestProxyClientDeliveryFailureLeavesNoRecord is the report's acceptance case
// for the commit rule: the origin answered completely, but the client tore the
// connection down before the response arrived. The exchange is not a complete
// transfer, so neither a history line nor a body file may remain, and the
// proxy must say it saw the delivery fail.
func TestProxyClientDeliveryFailureLeavesNoRecord(t *testing.T) {
	env := newTestEnv(t)

	// A body larger than any buffer the write can absorb, so a closed socket
	// is guaranteed to surface as a write error rather than a silent success.
	body := strings.Repeat("x", 1<<20)
	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: close\r\n" +
		"\r\n" + body
	targetAddr, requests := startDelayedRawTarget(t, 200*time.Millisecond, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("proxy connection is %T, want *net.TCPConn", conn)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	request := "GET http://" + targetAddr + "/abandoned HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Connection: close\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// Close with RST instead of a FIN so the proxy's write fails immediately
	// instead of being acknowledged by a half-closed peer.
	if err := tcpConn.SetLinger(0); err != nil {
		t.Fatalf("set linger: %v", err)
	}
	if err := tcpConn.Close(); err != nil {
		t.Fatalf("abort client connection: %v", err)
	}
	waitObserved(t, requests)

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	requireNoStoredRecords(t, env.store)
	if stderr := proxy.stderr.String(); !strings.Contains(stderr, "deliver response") {
		t.Errorf("proxy stderr missing the delivery failure warning:\n%s", stderr)
	}
}

// TestProxyDeliversCompletedResponseWhenCommitFails pins the other half of the
// commit rule: the client is the first concern. A storage failure removes the
// record, but a response that was fully obtained still reaches the client.
func TestProxyDeliversCompletedResponseWhenCommitFails(t *testing.T) {
	env := newTestEnv(t)
	// A directory where history.jsonl belongs makes the append fail after the
	// bodies were published, which is the worst ordering for a commit.
	if err := os.MkdirAll(filepath.Join(env.store, "history.jsonl"), 0o700); err != nil {
		t.Fatalf("create history directory: %v", err)
	}

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t)

	request := "GET http://" + targetAddr + "/persist HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Connection: close\r\n\r\n"
	if response := sendRaw(t, proxyAddr, request); response != targetResponse {
		t.Fatalf("client response = %q, want the complete upstream response %q", response, targetResponse)
	}
	waitObserved(t, requests)

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	if stderr := proxy.stderr.String(); !strings.Contains(stderr, "commit capture") {
		t.Errorf("proxy stderr missing the commit failure warning:\n%s", stderr)
	}
	requireNoBodyFiles(t, env.store)
}

// TestSendReportsPersistenceFailure pins the send side of the same rule: unlike
// the proxy, a send's result is addressed by id afterwards, so a record that
// never became durable is an error even though the exchange completed.
func TestSendReportsPersistenceFailure(t *testing.T) {
	env := newTestEnv(t)
	if err := os.MkdirAll(filepath.Join(env.store, "history.jsonl"), 0o700); err != nil {
		t.Fatalf("create history directory: %v", err)
	}

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	targetAddr, requests := startRawTarget(t, targetResponse)

	result := env.runWithStore(t, "send", "--url", "http://"+targetAddr+"/persist")
	requireExit(t, result, 1)
	if code := errorEnvelope(t, result)["code"]; code != "INTERNAL_ERROR" {
		t.Errorf("error code = %v, want INTERNAL_ERROR", code)
	}
	waitObserved(t, requests)
	requireNoBodyFiles(t, env.store)
}

// requireNoBodyFiles asserts that no final or staged body survived. It is the
// body half of requireNoStoredRecords, usable when history.jsonl itself is
// deliberately unusable.
func requireNoBodyFiles(t *testing.T, store string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store, "bodies"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read bodies: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("failed exchange left body files: %v", names)
	}
}
