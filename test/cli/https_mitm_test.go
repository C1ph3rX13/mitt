package cli_test

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testUpstreamCA is a throwaway CA used to sign the local TLS targets. The
// proxy trusts it through SSL_CERT_FILE so upstream verification stays strict.
type testUpstreamCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestUpstreamCA(t *testing.T) *testUpstreamCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate upstream CA key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mitt test upstream CA", Organization: []string{"mitt-test"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create upstream CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse upstream CA: %v", err)
	}
	return &testUpstreamCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issueTargetCert signs a TLS server certificate with the test CA.
func (ca *testUpstreamCA) issueTargetCert(t *testing.T, dnsNames []string, ips []net.IP) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate target key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate target serial: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mitt test target"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create target certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse target certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, cert
}

type tlsTarget struct {
	addr     string
	requests chan observedRequest
	alpn     chan string
}

// startTLSTarget runs a raw one-request-per-connection HTTP/1.1 server over TLS
// and publishes each request plus the ALPN protocol negotiated with mitt.
func startTLSTarget(t *testing.T, cert tls.Certificate, response string) *tlsTarget {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"http/1.1"},
	})
	if err != nil {
		t.Fatalf("listen TLS target: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	target := &tlsTarget{
		addr:     listener.Addr().String(),
		requests: make(chan observedRequest, 16),
		alpn:     make(chan string, 16),
	}
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
				_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				target.alpn <- tlsConn.ConnectionState().NegotiatedProtocol
				reader := bufio.NewReader(tlsConn)
				request, err := readObservedRequestFrom(reader)
				if err != nil {
					return
				}
				target.requests <- request
				_, _ = io.WriteString(tlsConn, response)
			}(conn)
		}
	}()
	return target
}

func (target *tlsTarget) waitRequest(t *testing.T) observedRequest {
	t.Helper()
	select {
	case request := <-target.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("TLS target did not receive a request")
		return observedRequest{}
	}
}

func (target *tlsTarget) waitALPN(t *testing.T) string {
	t.Helper()
	select {
	case protocol := <-target.alpn:
		return protocol
	case <-time.After(5 * time.Second):
		t.Fatal("TLS target did not complete a handshake")
		return ""
	}
}

func (target *tlsTarget) port(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(target.addr)
	if err != nil {
		t.Fatalf("split target address %q: %v", target.addr, err)
	}
	return port
}

// startCaptureProxyWithEnv starts the proxy with extra environment entries,
// which https tests use to install the test upstream CA via SSL_CERT_FILE.
func (e *testEnv) startCaptureProxyWithEnv(t *testing.T, extraEnv []string, args ...string) (string, *runningProxy) {
	t.Helper()
	e.mustInitCA(t)
	full := append([]string{"--addr", "127.0.0.1:0"}, args...)
	proxy := e.startProxyWithEnv(t, extraEnv, full...)
	status := proxy.waitReady(t, e)
	addr, _ := status["addr"].(string)
	if addr == "" {
		t.Fatalf("proxy status has no addr: %v", status)
	}
	return addr, proxy
}

func mittRootPool(t *testing.T, env *testEnv) *x509.CertPool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.store, "ca", "ca.pem"))
	if err != nil {
		t.Fatalf("read mitt CA: %v", err)
	}
	return certPool(t, data)
}

func certPool(t *testing.T, pemData []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		t.Fatalf("append CA PEM failed:\n%s", pemData)
	}
	return pool
}

// readerConn preserves bytes a bufio.Reader already consumed from the raw
// connection while handing the connection to tls.Client.
type readerConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// connectMITM performs a CONNECT handshake and completes TLS with the proxy.
func connectMITM(t *testing.T, proxyAddr, authority, serverName string, roots *x509.CertPool) *tls.Conn {
	t.Helper()
	return connectMITMALPN(t, proxyAddr, authority, serverName, roots, []string{"h2", "http/1.1"})
}

// connectMITMALPN is connectMITM with an explicit client ALPN advertisement,
// which h2 tests use to force the downstream protocol.
func connectMITMALPN(t *testing.T, proxyAddr, authority, serverName string, roots *x509.CertPool, nextProtos []string) *tls.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	request := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT status: %v", err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200 ") {
		t.Fatalf("CONNECT status = %q, want 200", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read CONNECT headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	tlsConn := tls.Client(&readerConn{Conn: conn, reader: reader}, &tls.Config{
		RootCAs:    roots,
		ServerName: serverName,
		NextProtos: nextProtos,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	return tlsConn
}

func publicKeySHA256(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func TestHTTPSConnectMITMCapturesEvent(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, targetLeaf := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})

	targetResponse := "HTTP/1.1 200 OK\r\n" +
		"X-Target: tls\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"hello"
	target := startTLSTarget(t, targetCert, targetResponse)

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile})

	authority := "localhost:" + target.port(t)
	tlsConn := connectMITM(t, proxyAddr, authority, "localhost", mittRootPool(t, env))
	if got := tlsConn.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Errorf("client ALPN = %q, want http/1.1", got)
	}

	requestLine := "GET /api/items?q=1 HTTP/1.1"
	request := requestLine + "\r\n" +
		"Host: " + authority + "\r\n" +
		"X-Repeater-Context: flow-https\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	if _, err := io.WriteString(tlsConn, request); err != nil {
		t.Fatalf("write tunneled request: %v", err)
	}
	response, err := io.ReadAll(tlsConn)
	if err != nil {
		t.Fatalf("read tunneled response: %v", err)
	}
	if string(response) != targetResponse {
		t.Fatalf("client response = %q, want %q", response, targetResponse)
	}

	observed := target.waitRequest(t)
	if observed.line != requestLine {
		t.Errorf("upstream request line = %q, want origin-form %q", observed.line, requestLine)
	}
	wantHeaders := [][2]string{
		{"Host", authority},
		{"Connection", "close"},
	}
	if !reflect.DeepEqual(observed.headers, wantHeaders) {
		t.Errorf("upstream headers = %#v, want %#v", observed.headers, wantHeaders)
	}
	if got := target.waitALPN(t); got != "http/1.1" {
		t.Errorf("upstream ALPN = %q, want http/1.1", got)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
	if strings.Contains(strings.ToLower(proxy.stderr.String()), "warning") {
		t.Errorf("proxy emitted a warning for a normal HTTPS capture:\n%s", proxy.stderr.String())
	}

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	event := decodeJSONObject(t, string(rawHistory))
	if event["kind"] != "captured" {
		t.Errorf("event kind = %v, want captured", event["kind"])
	}
	if event["method"] != "GET" {
		t.Errorf("event method = %v, want GET", event["method"])
	}
	wantURL := "https://" + authority + "/api/items?q=1"
	if event["url"] != wantURL {
		t.Errorf("event url = %v, want %s", event["url"], wantURL)
	}
	if event["http_version"] != "http/1.1" {
		t.Errorf("event http_version = %v, want http/1.1", event["http_version"])
	}
	if event["status"] != float64(200) {
		t.Errorf("event status = %v, want 200", event["status"])
	}
	if event["context"] != "flow-https" {
		t.Errorf("event context = %v, want flow-https", event["context"])
	}
	wantFingerprint := publicKeySHA256(t, targetLeaf)
	if event["upstream_cert_sha256"] != wantFingerprint {
		t.Errorf("event upstream_cert_sha256 = %v, want %s", event["upstream_cert_sha256"], wantFingerprint)
	}
	if event["request_line"] != requestLine {
		t.Errorf("event request_line = %q, want %q", event["request_line"], requestLine)
	}
	if event["forwarded_request_line"] != requestLine {
		t.Errorf("event forwarded_request_line = %q, want %q", event["forwarded_request_line"], requestLine)
	}
	if event["response_line"] != "HTTP/1.1 200 OK" {
		t.Errorf("event response_line = %v, want the upstream status line", event["response_line"])
	}
	requireHeaderPairs(t, event["request_headers"], wantHeaders)
	requireHeaderPairs(t, event["response_headers"], [][2]string{
		{"X-Target", "tls"},
		{"Content-Length", "5"},
		{"Connection", "close"},
	})

	history := env.runWithStore(t, "history", "--context", "flow-https")
	requireExit(t, history, 0)
	summary := decodeJSONObject(t, historyLines(history.stdout)[0])
	if summary["url"] != wantURL {
		t.Errorf("history url = %v, want %s", summary["url"], wantURL)
	}
	id, _ := summary["id"].(string)
	shown := env.runWithStore(t, "show", id, "--detail")
	requireExit(t, shown, 0)
	detail := decodeJSONObject(t, shown.stdout)
	if detail["upstream_cert_sha256"] != wantFingerprint {
		t.Errorf("show upstream_cert_sha256 = %v, want %s", detail["upstream_cert_sha256"], wantFingerprint)
	}
}

func describeCertificate(cert *x509.Certificate) string {
	return fmt.Sprintf("CN=%q DNSNames=%v IPAddresses=%v", cert.Subject.CommonName, cert.DNSNames, cert.IPAddresses)
}

// Inside a tunnel the received Host already names the CONNECT origin, so it is
// forwarded exactly as the client wrote it. Rewriting it would add a port the
// client never sent and change the head recorded in history.
func TestHTTPSMITMForwardsTunnelHostAsWritten(t *testing.T) {
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

	// The tunnel is opened for localhost:<port>, but the request's Host omits
	// the port. Both address the same host, so the request is accepted, and the
	// omission itself must survive the round trip.
	authority := "localhost:" + target.port(t)
	tlsConn := connectMITM(t, proxyAddr, authority, "localhost", mittRootPool(t, env))
	request := "GET /host-form HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(tlsConn, request); err != nil {
		t.Fatalf("write tunneled request: %v", err)
	}
	if _, err := io.ReadAll(tlsConn); err != nil {
		t.Fatalf("read tunneled response: %v", err)
	}

	observed := target.waitRequest(t)
	if got := headerValue(observed.headers, "Host"); got != "localhost" {
		t.Errorf("upstream Host = %q, want the client's spelling localhost", got)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	event := decodeJSONObject(t, string(rawHistory))
	if got := event["url"]; got != "https://"+authority+"/host-form" {
		t.Errorf("event url = %v, want the tunnel origin", got)
	}
	pairs, _ := event["request_headers"].([]any)
	if !headerPairsContain(pairs, "Host", "localhost") {
		t.Errorf("event request_headers = %v, want Host localhost as sent", event["request_headers"])
	}
}

func TestHTTPSMITMLeafCertificatesAreHostScopedAndCached(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startTLSTarget(t, targetCert, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile})
	roots := mittRootPool(t, env)

	dnsAuthority := "localhost:" + target.port(t)
	ipAuthority := "127.0.0.1:" + target.port(t)

	first := connectMITM(t, proxyAddr, dnsAuthority, "localhost", roots)
	dnsLeaf := first.ConnectionState().PeerCertificates[0]
	second := connectMITM(t, proxyAddr, dnsAuthority, "localhost", roots)
	secondDNSLeaf := second.ConnectionState().PeerCertificates[0]
	ipConn := connectMITM(t, proxyAddr, ipAuthority, "127.0.0.1", roots)
	ipLeaf := ipConn.ConnectionState().PeerCertificates[0]

	if dnsLeaf.Subject.CommonName != "localhost" {
		t.Errorf("DNS leaf CN = %q, want localhost", dnsLeaf.Subject.CommonName)
	}
	if len(dnsLeaf.DNSNames) != 1 || dnsLeaf.DNSNames[0] != "localhost" {
		t.Errorf("DNS leaf SAN = %v, want [localhost]", dnsLeaf.DNSNames)
	}
	if len(dnsLeaf.IPAddresses) != 0 {
		t.Errorf("DNS leaf IP SANs = %v, want none", dnsLeaf.IPAddresses)
	}
	if ipLeaf.Subject.CommonName != "127.0.0.1" {
		t.Errorf("IP leaf CN = %q, want 127.0.0.1", ipLeaf.Subject.CommonName)
	}
	if len(ipLeaf.IPAddresses) != 1 || !ipLeaf.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("IP leaf IP SANs = %v, want [127.0.0.1]", ipLeaf.IPAddresses)
	}
	if len(ipLeaf.DNSNames) != 0 {
		t.Errorf("IP leaf DNS SANs = %v, want none", ipLeaf.DNSNames)
	}
	for _, cert := range []*x509.Certificate{dnsLeaf, ipLeaf} {
		key, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || key.Curve != elliptic.P256() {
			t.Errorf("%s public key = %T, want ECDSA P-256", describeCertificate(cert), cert.PublicKey)
		}
		notAfterWant := time.Now().AddDate(1, 0, 0)
		if delta := cert.NotAfter.Sub(notAfterWant); delta > 48*time.Hour || delta < -48*time.Hour {
			t.Errorf("%s not_after = %v, want about one year from now (%v)", describeCertificate(cert), cert.NotAfter, notAfterWant)
		}
		if cert.NotBefore.After(time.Now().Add(time.Minute)) {
			t.Errorf("%s not_before = %v, want it already valid", describeCertificate(cert), cert.NotBefore)
		}
		for _, name := range append([]string{cert.Subject.CommonName}, cert.DNSNames...) {
			if strings.Contains(name, "*") {
				t.Errorf("%s issued a wildcard certificate", describeCertificate(cert))
			}
		}
	}

	caPEM, err := os.ReadFile(filepath.Join(env.store, "ca", "ca.pem"))
	if err != nil {
		t.Fatalf("read mitt CA: %v", err)
	}
	mittCA := parseCertificatePEM(t, string(caPEM))
	if err := dnsLeaf.CheckSignatureFrom(mittCA); err != nil {
		t.Errorf("DNS leaf is not signed by the mitt CA: %v", err)
	}
	if err := ipLeaf.CheckSignatureFrom(mittCA); err != nil {
		t.Errorf("IP leaf is not signed by the mitt CA: %v", err)
	}

	if !certificatesEqual(dnsLeaf, secondDNSLeaf) {
		t.Error("two CONNECT tunnels to the same host received different leaf certificates; the leaf cache is not reused")
	}
	if certificatesEqual(dnsLeaf, ipLeaf) {
		t.Error("localhost and 127.0.0.1 received the same leaf certificate; leaves are not host-scoped")
	}

	for _, tlsConn := range []*tls.Conn{first, second, ipConn} {
		_ = tlsConn.Close()
	}
	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)
}

func certificatesEqual(a, b *x509.Certificate) bool {
	return bytes.Equal(a.Raw, b.Raw)
}

func TestHTTPSMITMUpstreamTLSFailureClosesTunnel(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	trustedCA := newTestUpstreamCA(t)
	untrustedCA := newTestUpstreamCA(t)
	targetCert, _ := untrustedCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startTLSTarget(t, targetCert, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")

	caFile := filepath.Join(t.TempDir(), "trusted-ca.pem")
	if err := os.WriteFile(caFile, trustedCA.pem, 0o600); err != nil {
		t.Fatalf("write trusted CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile})

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	authority := "localhost:" + target.port(t)
	if _, err := io.WriteString(conn, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	response, err := io.ReadAll(conn)
	_ = conn.Close()
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if strings.HasPrefix(string(response), "HTTP/1.1 200 ") {
		t.Fatalf("CONNECT succeeded despite an untrusted upstream certificate:\n%s", response)
	}
	if !strings.HasPrefix(string(response), "HTTP/1.1 502 ") {
		t.Errorf("CONNECT response = %q, want a 502 failure", firstLine(string(response)))
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	detail := showMITMError(t, env, "upstream_tls", "localhost")
	details, _ := detail["details"].(map[string]any)
	if details["error"] == nil {
		t.Errorf("upstream_tls details = %v, want the TLS error", detail["details"])
	}

	captured := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, captured, 66)
	if code := errorEnvelope(t, captured)["code"]; code != "NO_MATCH" {
		t.Errorf("captured filter code = %v, want NO_MATCH", code)
	}
}

func TestHTTPSMITMClientCertificateRejectionClosesTunnel(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startTLSTarget(t, targetCert, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile})

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	authority := "localhost:" + target.port(t)
	if _, err := io.WriteString(conn, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT status: %v", err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200 ") {
		t.Fatalf("CONNECT status = %q, want 200", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read CONNECT headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	rejecting := tls.Client(&readerConn{Conn: conn, reader: reader}, &tls.Config{
		RootCAs:    x509.NewCertPool(),
		ServerName: "localhost",
		NextProtos: []string{"http/1.1"},
	})
	if err := rejecting.Handshake(); err == nil {
		t.Fatal("TLS handshake succeeded while the client rejected the mitt CA")
	}
	_ = rejecting.Close()

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	detail := showMITMError(t, env, "client_cert_rejected", "localhost")
	details, _ := detail["details"].(map[string]any)
	if details["error"] == nil {
		t.Errorf("client_cert_rejected details = %v, want the handshake error", detail["details"])
	}

	captured := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, captured, 66)
	if code := errorEnvelope(t, captured)["code"]; code != "NO_MATCH" {
		t.Errorf("captured filter code = %v, want NO_MATCH", code)
	}
}

func TestHTTPSMITMClientAbortDoesNotWriteClientCertRejected(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startTLSTarget(t, targetCert, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile})
	authority := "localhost:" + target.port(t)

	// A browser may open the tunnel and close it before sending a
	// ClientHello. That is a transport abort, not a certificate rejection.
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	readCONNECTOK(t, bufio.NewReader(conn))
	if err := conn.Close(); err != nil {
		t.Fatalf("close aborted tunnel: %v", err)
	}

	// A second, healthy tunnel proves the abort did not stop the proxy.
	tlsConn := connectMITM(t, proxyAddr, authority, "localhost", mittRootPool(t, env))
	if _, err := io.WriteString(tlsConn, "GET /after-abort HTTP/1.1\r\nHost: "+authority+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write healthy tunneled request: %v", err)
	}
	if _, err := io.ReadAll(tlsConn); err != nil {
		t.Fatalf("read healthy tunneled response: %v", err)
	}
	observed := target.waitRequest(t)
	if observed.line != "GET /after-abort HTTP/1.1" {
		t.Errorf("upstream request line = %q, want only the healthy request", observed.line)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	history := env.runWithStore(t, "history", "--filter", "mitm_error")
	requireExit(t, history, 66)
	if code := errorEnvelope(t, history)["code"]; code != "NO_MATCH" {
		t.Errorf("mitm_error filter code = %v, want NO_MATCH for a client abort", code)
	}
	if stderr := proxy.stderr.String(); !strings.Contains(stderr, "client aborted TLS handshake") {
		t.Errorf("proxy stderr missing the client-abort warning:\n%s", stderr)
	}
}

func TestHTTPSMITMHonorsExplicitCA(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	altDir := filepath.Join(t.TempDir(), "alt-ca")
	requireExit(t, env.runWithStore(t, "ca", "init", "--dir", altDir), 0)
	altPEM := readFileString(t, filepath.Join(altDir, "ca.pem"))

	upstreamCA := newTestUpstreamCA(t)
	targetCert, _ := upstreamCA.issueTargetCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	target := startTLSTarget(t, targetCert, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	caFile := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caFile, upstreamCA.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	proxyAddr, proxy := env.startCaptureProxyWithEnv(t, []string{"SSL_CERT_FILE=" + caFile}, "--ca", altDir)

	status := env.readProxyState(t)
	if status["ca_fingerprint_sha256"] != sha256Hex(altPEM) {
		t.Errorf("proxy.json ca_fingerprint_sha256 = %v, want the fingerprint of the explicit --ca", status["ca_fingerprint_sha256"])
	}

	altRoots := certPool(t, []byte(altPEM))
	authority := "localhost:" + target.port(t)
	tlsConn := connectMITM(t, proxyAddr, authority, "localhost", altRoots)
	leaf := tlsConn.ConnectionState().PeerCertificates[0]
	altCA := parseCertificatePEM(t, altPEM)
	if err := leaf.CheckSignatureFrom(altCA); err != nil {
		t.Errorf("leaf is not signed by the explicit --ca: %v", err)
	}

	if _, err := io.WriteString(tlsConn, "GET /explicit-ca HTTP/1.1\r\nHost: "+authority+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write tunneled request: %v", err)
	}
	if _, err := io.ReadAll(tlsConn); err != nil {
		t.Fatalf("read tunneled response: %v", err)
	}
	target.waitRequest(t)

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	rawHistory, err := os.ReadFile(filepath.Join(env.store, "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	event := decodeJSONObject(t, string(rawHistory))
	if event["kind"] != "captured" {
		t.Errorf("event kind = %v, want captured", event["kind"])
	}
	if event["url"] != "https://"+authority+"/explicit-ca" {
		t.Errorf("event url = %v, want the explicit-CA capture URL", event["url"])
	}
}
