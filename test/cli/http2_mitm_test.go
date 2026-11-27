package cli_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// h2Target is a TLS origin that only speaks h2 and publishes each request it
// serves, including the ALPN it negotiated with mitt.
type h2Target struct {
	addr     string
	requests chan observedRequest
	alpn     chan string
}

func startH2Target(t *testing.T, cert tls.Certificate) *h2Target {
	return startH2TargetWithALPN(t, cert, false)
}

// startStrictH2OnlyTarget emulates an origin that aborts the TLS handshake
// when the client does not offer h2, which is the strict h2-only case.
func startStrictH2OnlyTarget(t *testing.T, cert tls.Certificate) *h2Target {
	return startH2TargetWithALPN(t, cert, true)
}

func startH2TargetWithALPN(t *testing.T, cert tls.Certificate, strict bool) *h2Target {
	t.Helper()
	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2"},
	}
	if strict {
		config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			for _, protocol := range hello.SupportedProtos {
				if protocol == "h2" {
					return nil, nil
				}
			}
			return nil, errors.New("no application protocol")
		}
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatalf("listen h2 target: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	target := &h2Target{
		addr:     listener.Addr().String(),
		requests: make(chan observedRequest, 32),
		alpn:     make(chan string, 32),
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go target.serveConn(t, conn)
		}
	}()
	return target
}

func (target *h2Target) serveConn(t *testing.T, conn net.Conn) {
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return
	}
	_ = tlsConn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	target.alpn <- tlsConn.ConnectionState().NegotiatedProtocol

	server := &http2.Server{}
	server.ServeConn(tlsConn, &http2.ServeConnOpts{
		BaseConfig: &http.Server{ErrorLog: log.New(io.Discard, "", 0)},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			target.requests <- observedRequest{
				line:    r.Method + " " + r.URL.RequestURI() + " HTTP/2.0",
				headers: sortedHeaderPairs(r.Header),
				body:    body,
			}
			if strings.HasPrefix(r.URL.Path, "/slow") {
				time.Sleep(25 * time.Millisecond)
			}
			w.Header().Set("X-Target", "h2")
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "h2:"+r.URL.RequestURI())
		}),
	})
}

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

func (target *h2Target) waitRequest(t *testing.T) observedRequest {
	t.Helper()
	select {
	case request := <-target.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("h2 target did not receive a request")
		return observedRequest{}
	}
}

func (target *h2Target) waitALPN(t *testing.T) string {
	t.Helper()
	select {
	case protocol := <-target.alpn:
		return protocol
	case <-time.After(5 * time.Second):
		t.Fatal("h2 target did not complete a handshake")
		return ""
	}
}

func (target *h2Target) port(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(target.addr)
	if err != nil {
		t.Fatalf("split target address %q: %v", target.addr, err)
	}
	return port
}

// newH2Client drives the MITM tunnel with x/net/http2. The tunnel's TLS
// connection is handed to the transport exactly once; h2 multiplexing happens
// on that connection.
func newH2Client(t *testing.T, tlsConn *tls.Conn) *http.Client {
	t.Helper()
	var mu sync.Mutex
	handed := false
	transport := &http2.Transport{
		StrictMaxConcurrentStreams: true,
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			if handed {
				return nil, errors.New("test h2 client only supports one tunnel connection")
			}
			handed = true
			return tlsConn, nil
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func doH2Request(t *testing.T, client *http.Client, method, rawURL, body, contextName string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		t.Fatalf("build h2 request %s %s: %v", method, rawURL, err)
	}
	if contextName != "" {
		request.Header.Set("X-Repeater-Context", contextName)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send h2 request %s %s: %v", method, rawURL, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read h2 response for %s %s: %v", method, rawURL, err)
	}
	return response.StatusCode, string(data)
}

func TestHTTP2MITMCapturesConcurrentStreams(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, targetLeaf := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startH2Target(t, targetCert)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile}, "--http2-alpn", "h2,h1")

	authority := "localhost:" + target.port(t)
	clientTLS := connectMITMALPN(t, proxyAddr, authority, "localhost", mittRootPool(t, env), []string{"h2"})
	if got := clientTLS.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("client ALPN = %q, want h2", got)
	}
	if got := target.waitALPN(t); got != "h2" {
		t.Fatalf("upstream ALPN = %q, want h2", got)
	}
	client := newH2Client(t, clientTLS)
	baseURL := "https://" + authority

	// One request with a body, then concurrent bodyless streams. The warm-up
	// also establishes the single multiplexed connection before the fan-out.
	status, body := doH2Request(t, client, http.MethodPost, baseURL+"/warmup", "hello h2", "h2-flow")
	if status != http.StatusOK || body != "h2:/warmup" {
		t.Fatalf("warmup response = %d %q, want 200 %q", status, body, "h2:/warmup")
	}
	warmup := target.waitRequest(t)
	if warmup.line != "POST /warmup HTTP/2.0" {
		t.Errorf("target warmup request = %q, want POST /warmup HTTP/2.0", warmup.line)
	}
	if string(warmup.body) != "hello h2" {
		t.Errorf("target warmup body = %q, want %q", warmup.body, "hello h2")
	}

	const streams = 8
	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			requestURL := fmt.Sprintf("%s/items?i=%d", baseURL, index)
			request, err := http.NewRequest(http.MethodGet, requestURL, nil)
			if err != nil {
				errs <- err
				return
			}
			request.Header.Set("X-Repeater-Context", "h2-flow")
			response, err := client.Do(request)
			if err != nil {
				errs <- fmt.Errorf("request %d: %w", index, err)
				return
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil {
				errs <- fmt.Errorf("read %d: %w", index, err)
				return
			}
			if want := fmt.Sprintf("h2:/items?i=%d", index); string(data) != want {
				errs <- fmt.Errorf("stream %d body = %q, want %q", index, data, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
	if strings.Contains(strings.ToLower(proxy.stderr.String()), "warning") {
		t.Errorf("proxy emitted a warning for normal h2 traffic:\n%s", proxy.stderr.String())
	}

	history := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, history, 0)
	lines := historyLines(history.stdout)
	if len(lines) != streams+1 {
		t.Fatalf("history --filter captured returned %d lines, want %d:\n%s", len(lines), streams+1, history.stdout)
	}

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	seen := map[string]map[string]any{}
	for _, line := range historyLines(string(rawHistory)) {
		event := decodeJSONObject(t, line)
		if event["kind"] != "captured" {
			t.Errorf("event kind = %v, want captured", event["kind"])
			continue
		}
		url, _ := event["url"].(string)
		if _, duplicate := seen[url]; duplicate {
			t.Errorf("duplicate captured event for url %s: concurrent streams were merged", url)
		}
		seen[url] = event
	}
	if len(seen) != streams+1 {
		t.Fatalf("captured %d distinct URLs, want %d", len(seen), streams+1)
	}

	wantFingerprint := publicKeySHA256(t, targetLeaf)
	for i := 0; i < streams; i++ {
		url := fmt.Sprintf("%s/items?i=%d", baseURL, i)
		event := seen[url]
		if event == nil {
			t.Errorf("missing captured event for %s", url)
			continue
		}
		if event["http_version"] != "h2" {
			t.Errorf("event %s http_version = %v, want h2", url, event["http_version"])
		}
		if event["method"] != "GET" {
			t.Errorf("event %s method = %v, want GET", url, event["method"])
		}
		if event["status"] != float64(200) {
			t.Errorf("event %s status = %v, want 200", url, event["status"])
		}
		if event["context"] != "h2-flow" {
			t.Errorf("event %s context = %v, want h2-flow", url, event["context"])
		}
		if event["upstream_cert_sha256"] != wantFingerprint {
			t.Errorf("event %s upstream_cert_sha256 = %v, want %s", url, event["upstream_cert_sha256"], wantFingerprint)
		}
		if line, _ := event["request_line"].(string); !strings.HasPrefix(line, "GET /items?i=") || !strings.HasSuffix(line, " HTTP/2.0") {
			t.Errorf("event %s request_line = %q, want a synthesized HTTP/2.0 request line", url, line)
		}
		if line, _ := event["response_line"].(string); !strings.HasPrefix(line, "HTTP/2.0 200 ") {
			t.Errorf("event %s response_line = %q, want an HTTP/2.0 status line", url, line)
		}
		if event["req_body_size"] != float64(0) {
			t.Errorf("event %s req_body_size = %v, want 0", url, event["req_body_size"])
		}
		if event["resp_body_size"] != float64(len("h2:"+fmt.Sprintf("/items?i=%d", i))) {
			t.Errorf("event %s resp_body_size = %v, want %d", url, event["resp_body_size"], len("h2:"+fmt.Sprintf("/items?i=%d", i)))
		}
		headers, _ := event["request_headers"].([]any)
		if !headerPairsContain(headers, "Host", authority) {
			t.Errorf("event %s request_headers = %v, want Host %s", url, headers, authority)
		}
		if headerPairsContain(headers, "X-Repeater-Context", "h2-flow") {
			t.Errorf("event %s still contains X-Repeater-Context after capture", url)
		}
	}

	warmupEvent := seen[baseURL+"/warmup"]
	if warmupEvent == nil {
		t.Fatal("missing captured event for the warmup POST")
	}
	if warmupEvent["method"] != "POST" || warmupEvent["req_body_size"] != float64(len("hello h2")) {
		t.Errorf("warmup event = method %v req_body_size %v, want POST and %d", warmupEvent["method"], warmupEvent["req_body_size"], len("hello h2"))
	}

	// `show --raw` must render the stored h2 request body even though the
	// synthesized HTTP/2.0 request line has no wire-level framing of its own.
	rawEvent := env.runWithStore(t, "show", warmupEvent["id"].(string), "--raw")
	requireExit(t, rawEvent, 0)
	for _, want := range []string{"POST /warmup HTTP/2.0\r\n", "\r\n\r\nhello h2", "HTTP/2.0 200 OK\r\n", "h2:/warmup"} {
		if !strings.Contains(rawEvent.stdout, want) {
			t.Errorf("show --raw for h2 warmup = %q, want substring %q", rawEvent.stdout, want)
		}
	}
}

func headerPairsContain(raw []any, name, value string) bool {
	for _, entry := range raw {
		pair, ok := entry.([]any)
		if !ok || len(pair) != 2 {
			continue
		}
		if pair[0] == name && pair[1] == value {
			return true
		}
	}
	return false
}

func TestDefaultHTTP1ALPNWritesH2OnlyTargetError(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startH2Target(t, targetCert)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile})

	// Go's TLS server accepts an http/1.1 ClientHello against an h2-only
	// listener as an ALPN fallback, so the h2-only nature only shows up when
	// the first HTTP/1.1 request reaches the h2 stack. The proxy must classify
	// that failure instead of leaking a bare 502.
	authority := "localhost:" + target.port(t)
	clientTLS := connectMITM(t, proxyAddr, authority, "localhost", mittRootPool(t, env))
	request := "GET /h2-only HTTP/1.1\r\nHost: " + authority + "\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(clientTLS, request); err != nil {
		t.Fatalf("write h1 request to h2-only target: %v", err)
	}
	response, err := io.ReadAll(clientTLS)
	if err != nil {
		t.Fatalf("read h1 response from h2-only target: %v", err)
	}
	if !strings.HasPrefix(string(response), "HTTP/1.1 502 ") {
		t.Errorf("h1 response = %q, want a 502 for an h2-only target under the default ALPN", firstLine(string(response)))
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
	detail := showMITMError(t, env, "h2_only_target", "localhost")
	details, _ := detail["details"].(map[string]any)
	if hint, _ := details["hint"].(string); !strings.Contains(hint, "--http2-alpn") {
		t.Errorf("h2_only_target hint = %q, want an actionable --http2-alpn hint", hint)
	}

	captured := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, captured, 66)
	if code := errorEnvelope(t, captured)["code"]; code != "NO_MATCH" {
		t.Errorf("captured filter code = %v, want NO_MATCH", code)
	}
}

func TestDefaultHTTP1ALPNClassifiesStrictH2OnlyHandshakeFailure(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startStrictH2OnlyTarget(t, targetCert)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile})

	response := sendPlainCONNECT(t, proxyAddr, "localhost:"+target.port(t))
	if !strings.HasPrefix(response, "HTTP/1.1 502 ") {
		t.Errorf("CONNECT response = %q, want 502 when the origin rejects the HTTP/1.1 ALPN", firstLine(response))
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	showMITMError(t, env, "h2_only_target", "localhost")
}

func TestHTTP2ALPNH2CapturesH2OnlyTarget(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startH2Target(t, targetCert)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile}, "--http2-alpn", "h2")

	authority := "localhost:" + target.port(t)
	clientTLS := connectMITMALPN(t, proxyAddr, authority, "localhost", mittRootPool(t, env), []string{"h2"})
	client := newH2Client(t, clientTLS)
	status, body := doH2Request(t, client, http.MethodGet, "https://"+authority+"/only-h2", "", "h2-only")
	if status != http.StatusOK || body != "h2:/only-h2" {
		t.Fatalf("response = %d %q, want 200 %q", status, body, "h2:/only-h2")
	}
	if got := target.waitALPN(t); got != "h2" {
		t.Errorf("upstream ALPN = %q, want h2", got)
	}
	target.waitRequest(t)

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	lines := historyLines(string(rawHistory))
	if len(lines) != 1 {
		t.Fatalf("history has %d lines, want 1:\n%s", len(lines), rawHistory)
	}
	event := decodeJSONObject(t, lines[0])
	if event["kind"] != "captured" || event["http_version"] != "h2" {
		t.Errorf("event kind/http_version = %v/%v, want captured/h2", event["kind"], event["http_version"])
	}

	errs := env.runWithStore(t, "history", "--filter", "mitm_error")
	requireExit(t, errs, 66)
	if code := errorEnvelope(t, errs)["code"]; code != "NO_MATCH" {
		t.Errorf("mitm_error filter code = %v, want NO_MATCH", code)
	}
}

func TestHTTP2MITMBridgesHTTP1Upstream(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	targetResponse := "HTTP/1.1 200 OK\r\nX-Target: h1\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	target := startTLSTarget(t, targetCert, targetResponse)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile}, "--http2-alpn", "h2,h1")

	authority := "localhost:" + target.port(t)
	clientTLS := connectMITMALPN(t, proxyAddr, authority, "localhost", mittRootPool(t, env), []string{"h2"})
	client := newH2Client(t, clientTLS)
	status, body := doH2Request(t, client, http.MethodGet, "https://"+authority+"/bridge", "", "bridge")
	if status != http.StatusOK || body != "ok" {
		t.Fatalf("bridged response = %d %q, want 200 %q", status, body, "ok")
	}

	observed := target.waitRequest(t)
	if observed.line != "GET /bridge HTTP/1.1" {
		t.Errorf("upstream h1 request line = %q, want GET /bridge HTTP/1.1", observed.line)
	}
	if got := target.waitALPN(t); got != "http/1.1" {
		t.Errorf("upstream ALPN = %q, want http/1.1", got)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	lines := historyLines(string(rawHistory))
	if len(lines) != 1 {
		t.Fatalf("history has %d lines, want 1:\n%s", len(lines), rawHistory)
	}
	event := decodeJSONObject(t, lines[0])
	if event["kind"] != "captured" {
		t.Errorf("event kind = %v, want captured", event["kind"])
	}
	if event["http_version"] != "h2" {
		t.Errorf("event http_version = %v, want h2 (the downstream protocol)", event["http_version"])
	}
	// The bridge writes an HTTP/1.1 line to the origin, so the recorded
	// forwarded line must name HTTP/1.1 even though the client spoke h2.
	// Claiming HTTP/2.0 here is the drift an identity value prevents.
	if got := event["forwarded_request_line"]; got != "GET /bridge HTTP/1.1" {
		t.Errorf("event forwarded_request_line = %v, want the line the h1 origin received", got)
	}
	if got := event["request_line"]; got != "GET /bridge HTTP/2.0" {
		t.Errorf("event request_line = %v, want the synthesized HTTP/2.0 line", got)
	}
	if got := event["url"]; got != "https://"+authority+"/bridge" {
		t.Errorf("event url = %v, want the tunnel origin plus the request target", got)
	}
	if event["status"] != float64(200) || event["resp_body_size"] != float64(2) {
		t.Errorf("event status/body = %v/%v, want 200/2", event["status"], event["resp_body_size"])
	}
}

// A tunnel is addressed by its CONNECT authority. An HTTP/1.1 request naming a
// different Host cannot be forwarded without either lying about the URL or
// crossing hosts, so it is rejected before anything is recorded or dialed.
func TestHTTPSMITMRejectsTunnelRequestWithForeignHost(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	target := startTLSTarget(t, targetCert, targetResponse)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile})

	authority := "localhost:" + target.port(t)
	tlsConn := connectMITM(t, proxyAddr, authority, "localhost", mittRootPool(t, env))
	request := "GET /cross HTTP/1.1\r\nHost: other.example\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(tlsConn, request); err != nil {
		t.Fatalf("write cross-host tunneled request: %v", err)
	}
	_ = tlsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	response, err := io.ReadAll(tlsConn)
	if err != nil {
		t.Fatalf("read cross-host response: %v", err)
	}
	if !strings.HasPrefix(string(response), "HTTP/1.1 400 ") {
		t.Errorf("cross-host response = %q, want a 400 receipt", firstLine(string(response)))
	}

	// The upstream must never have seen the request.
	select {
	case observed := <-target.requests:
		t.Errorf("upstream received the cross-host request: %+v", observed)
	case <-time.After(250 * time.Millisecond):
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	captured := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, captured, 66)
	if code := errorEnvelope(t, captured)["code"]; code != "NO_MATCH" {
		t.Errorf("captured filter code = %v, want NO_MATCH for a rejected cross-host request", code)
	}
}

// A tunnel is addressed by its CONNECT authority. An HTTP/2 stream whose
// :authority names another host is rejected the same way the HTTP/1.1 path
// rejects a foreign Host, so both protocols share one authority rule.
func TestHTTP2MITMRejectsStreamWithForeignAuthority(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startH2Target(t, targetCert)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile}, "--http2-alpn", "h2,h1")

	authority := "localhost:" + target.port(t)
	clientTLS := connectMITMALPN(t, proxyAddr, authority, "localhost", mittRootPool(t, env), []string{"h2"})
	client := newH2Client(t, clientTLS)
	status, body := doH2Request(t, client, http.MethodGet, "https://other.example/cross", "", "cross")
	if status != http.StatusBadRequest {
		t.Errorf("cross-authority status = %d (%q), want 400", status, body)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	captured := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, captured, 66)
	if code := errorEnvelope(t, captured)["code"]; code != "NO_MATCH" {
		t.Errorf("captured filter code = %v, want NO_MATCH for a rejected stream", code)
	}
}

// A stream that declares Content-Length: 0 is forwarded with that field, the
// same way an HTTP/1.1 request is: the h1 origin sees the head the client
// described, and the event records the head that was sent.
func TestHTTP2MITMBridgeKeepsDeclaredEmptyBodyFraming(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	target := startTLSTarget(t, targetCert, targetResponse)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile}, "--http2-alpn", "h2,h1")

	authority := "localhost:" + target.port(t)
	clientTLS := connectMITMALPN(t, proxyAddr, authority, "localhost", mittRootPool(t, env), []string{"h2"})
	client := newH2Client(t, clientTLS)
	status, body := doH2Request(t, client, http.MethodPost, "https://"+authority+"/empty", "", "empty")
	if status != http.StatusOK || body != "ok" {
		t.Fatalf("response = %d %q, want 200 %q", status, body, "ok")
	}

	observed := target.waitRequest(t)
	if headerValue(observed.headers, "Content-Length") != "0" {
		t.Errorf("upstream headers = %v, want the declared Content-Length: 0", observed.headers)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	lines := historyLines(string(rawHistory))
	if len(lines) != 1 {
		t.Fatalf("history has %d lines, want 1:\n%s", len(lines), rawHistory)
	}
	event := decodeJSONObject(t, lines[0])
	if pairs, ok := event["request_headers"].([]any); !ok || !headerPairsContain(pairs, "Content-Length", "0") {
		t.Errorf("event request_headers = %v, want the recorded Content-Length: 0", event["request_headers"])
	}
}

func TestHTTP2ParseErrorWritesWarningWithRawBytes(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startH2Target(t, targetCert)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile}, "--http2-alpn", "h2,h1")

	authority := "localhost:" + target.port(t)
	clientTLS := connectMITMALPN(t, proxyAddr, authority, "localhost", mittRootPool(t, env), []string{"h2"})

	// A PING before the required initial SETTINGS is a protocol error. It
	// deliberately stays readable on the tap so the warning event can point at
	// the raw client frames.
	preface := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	ping := []byte{0x00, 0x00, 0x08, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	if _, err := clientTLS.Write(append(preface, ping...)); err != nil {
		t.Fatalf("write malformed h2 frames: %v", err)
	}
	_ = clientTLS.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.ReadAll(clientTLS)

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	detail := showMITMError(t, env, "h2_parse_error", "localhost")
	details, _ := detail["details"].(map[string]any)
	rawPath, _ := details["client_read_path"].(string)
	if rawPath == "" {
		t.Fatalf("h2_parse_error details = %v, want a client_read_path with the raw frames", details)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("read raw h2 frames %s: %v", rawPath, err)
	}
	if !strings.Contains(string(raw), "PRI * HTTP/2.0") {
		t.Errorf("raw h2 frames = %q, want the client preface", raw)
	}
	if _, ok := details["client_read_size"].(float64); !ok {
		t.Errorf("h2_parse_error details = %v, want client_read_size", details)
	}
	captured := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, captured, 66)
	if code := errorEnvelope(t, captured)["code"]; code != "NO_MATCH" {
		t.Errorf("captured filter code = %v, want NO_MATCH", code)
	}
}

func TestProxyHTTP2ALPNFlagRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	start, ok := doc.Command.find("mitt proxy start")
	if !ok {
		t.Fatal("spec missing mitt proxy start")
	}
	flag, ok := flagByName(start.Flags, "http2-alpn")
	if !ok {
		t.Fatal("proxy start missing --http2-alpn")
	}
	if flag.Type != "string" {
		t.Errorf("--http2-alpn type = %q, want string", flag.Type)
	}
	if flag.Default != "h1" {
		t.Errorf("--http2-alpn default = %q, want h1", flag.Default)
	}
	if flag.Inherited {
		t.Error("--http2-alpn must be a proxy start flag, not inherited")
	}
	if flag.Description == "" {
		t.Error("--http2-alpn description must not be empty")
	}
	wantEnum := []string{"h1", "h2", "h2,h1"}
	if len(flag.Enum) != len(wantEnum) {
		t.Fatalf("--http2-alpn enum = %v, want %v", flag.Enum, wantEnum)
	}
	for i := range wantEnum {
		if flag.Enum[i] != wantEnum[i] {
			t.Errorf("--http2-alpn enum = %v, want %v", flag.Enum, wantEnum)
			break
		}
	}
}

func TestProxyHTTP2ALPNRejectsInvalidValue(t *testing.T) {
	env := newTestEnv(t)

	got := env.run(t, "proxy", "start", "--http2-alpn", "http3", "--addr", "127.0.0.1:0")

	requireExit(t, got, 2)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("error.code = %v, want INVALID_REQUEST", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["value"] != "http3" {
		t.Errorf("error.details.value = %v, want http3", details["value"])
	}
	allowed, _ := details["allowed"].([]any)
	if len(allowed) != 3 {
		t.Errorf("error.details.allowed = %v, want the three ALPN modes", details["allowed"])
	}
	if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
		t.Fatalf("invalid --http2-alpn must not start the proxy, stat err = %v", err)
	}
}

// startLargeBodyH2Target is an h2 origin whose response body is larger than any
// single HTTP/2 flow-control window. A client that stops reading therefore
// blocks the proxy mid-response, which is the state needed to observe a
// stream reset during delivery instead of after the record was committed.
func startLargeBodyH2Target(t *testing.T, cert tls.Certificate, bodyLen int) (*h2Target, <-chan struct{}) {
	t.Helper()
	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2"},
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatalf("listen large h2 target: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	body := make([]byte, bodyLen)
	for i := range body {
		body[i] = 'x'
	}
	target := &h2Target{
		addr:     listener.Addr().String(),
		requests: make(chan observedRequest, 4),
		alpn:     make(chan string, 4),
	}
	served := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				tlsConn, ok := conn.(*tls.Conn)
				if !ok {
					return
				}
				_ = tlsConn.SetDeadline(time.Now().Add(15 * time.Second))
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				target.alpn <- tlsConn.ConnectionState().NegotiatedProtocol
				server := &http2.Server{}
				server.ServeConn(tlsConn, &http2.ServeConnOpts{
					BaseConfig: &http.Server{ErrorLog: log.New(io.Discard, "", 0)},
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						target.requests <- observedRequest{line: r.Method + " " + r.URL.RequestURI() + " HTTP/2.0"}
						w.Header().Set("Content-Length", strconv.Itoa(len(body)))
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write(body)
						served <- struct{}{}
					}),
				})
			}(conn)
		}
	}()
	return target, served
}

// TestHTTP2MITMClientAbortDuringDeliveryLeavesNoRecord pins the h2 half of the
// commit rule. The old adapter committed before writing the response and threw
// away io.Copy's error, so a stream the client reset still became a history
// entry. The record must instead follow the delivery: a reset mid-response
// leaves neither a line nor body files.
func TestHTTP2MITMClientAbortDuringDeliveryLeavesNoRecord(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target, served := startLargeBodyH2Target(t, targetCert, 4<<20)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile}, "--http2-alpn", "h2,h1")

	authority := "localhost:" + target.port(t)
	clientTLS := connectMITMALPN(t, proxyAddr, authority, "localhost", mittRootPool(t, env), []string{"h2"})
	client := newH2Client(t, clientTLS)

	// The request is cancelled as soon as the response head arrives. The body
	// is never read, so the proxy cannot finish the 4 MiB write: the stream is
	// reset while delivery is still in progress.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+authority+"/abandoned", nil)
	if err != nil {
		t.Fatalf("build h2 request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("h2 request: %v", err)
	}
	target.waitRequest(t)
	cancel()
	_ = response.Body.Close()

	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream never finished the response the proxy abandoned delivering")
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	requireNoStoredRecords(t, env.store)
	if stderr := proxy.stderr.String(); !strings.Contains(stderr, "deliver h2 response") {
		t.Errorf("proxy stderr missing the h2 delivery failure warning:\n%s", stderr)
	}
}
