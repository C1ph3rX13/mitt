package cli_test

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sendPlainCONNECT(t *testing.T, proxyAddr, authority string) string {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	response, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return string(response)
}

func showMITMError(t *testing.T, env *testEnv, reason, targetHost string) map[string]any {
	t.Helper()
	history := env.runWithStore(t, "history", "--filter", "mitm_error")
	requireExit(t, history, 0)
	lines := historyLines(history.stdout)
	if len(lines) != 1 {
		t.Fatalf("history --filter mitm_error returned %d lines, want 1:\n%s", len(lines), history.stdout)
	}
	summary := decodeJSONObject(t, lines[0])
	if summary["kind"] != "mitm_error" {
		t.Errorf("history kind = %v, want mitm_error", summary["kind"])
	}
	if summary["status"] != nil {
		t.Errorf("mitm_error history status = %v, want null", summary["status"])
	}
	id, _ := summary["id"].(string)
	if id == "" {
		t.Fatalf("mitm_error history entry has no id: %v", summary)
	}

	shown := env.runWithStore(t, "show", id)
	requireExit(t, shown, 0)
	detail := decodeJSONObject(t, shown.stdout)
	if detail["kind"] != "mitm_error" {
		t.Errorf("show kind = %v, want mitm_error", detail["kind"])
	}
	if detail["reason"] != reason {
		t.Errorf("show reason = %v, want %s", detail["reason"], reason)
	}
	if detail["target_host"] != targetHost {
		t.Errorf("show target_host = %v, want %s", detail["target_host"], targetHost)
	}
	return detail
}

func TestScopeDeniedCONNECTWritesMITMError(t *testing.T) {
	env := newTestEnv(t)
	proxyAddr, proxy := env.startCaptureProxy(t,
		"--allow-targets", "127.0.0.1",
		"--deny-targets", "127.0.0.1",
	)

	response := sendPlainCONNECT(t, proxyAddr, "127.0.0.1:1")
	if !strings.HasPrefix(response, "HTTP/1.1 403 ") {
		t.Errorf("CONNECT response = %q, want 403 for a denied target", firstLine(response))
	}
	if strings.HasPrefix(response, "HTTP/1.1 200 ") {
		t.Fatalf("CONNECT succeeded for a denied target:\n%s", response)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	detail := showMITMError(t, env, "scope_denied", "127.0.0.1")
	details, _ := detail["details"].(map[string]any)
	if details["matched"] != "deny" {
		t.Errorf("scope_denied details = %v, want matched=deny (deny must win over allow)", detail["details"])
	}

	captured := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, captured, 66)
	if code := errorEnvelope(t, captured)["code"]; code != "NO_MATCH" {
		t.Errorf("captured filter code = %v, want NO_MATCH", code)
	}
}

func readCONNECTOK(t *testing.T, reader *bufio.Reader) {
	t.Helper()
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
			return
		}
	}
}

func TestScopeDeniedPlainHTTPReturns403AndWritesMITMError(t *testing.T) {
	env := newTestEnv(t)
	proxyAddr, proxy := env.startCaptureProxy(t, "--deny-targets", "blocked.example")

	response := sendRaw(t, proxyAddr,
		"GET http://blocked.example/private HTTP/1.1\r\n"+
			"Host: blocked.example\r\n"+
			"Connection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 403 ") {
		t.Errorf("plain HTTP response = %q, want 403 for a denied target", firstLine(response))
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	detail := showMITMError(t, env, "scope_denied", "blocked.example")
	details, _ := detail["details"].(map[string]any)
	if details["matched"] != "deny" || details["pattern"] != "blocked.example" {
		t.Errorf("scope_denied details = %v, want matched=deny pattern=blocked.example", detail["details"])
	}

	captured := env.runWithStore(t, "history", "--filter", "captured")
	requireExit(t, captured, 66)
	if code := errorEnvelope(t, captured)["code"]; code != "NO_MATCH" {
		t.Errorf("captured filter code = %v, want NO_MATCH", code)
	}
}

func TestMITMNotTLSWritesErrorAndProxyKeepsRunning(t *testing.T) {
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

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	readCONNECTOK(t, reader)
	if _, err := io.WriteString(conn, "GET /not-tls HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write non-TLS tunnel bytes: %v", err)
	}
	_, _ = io.ReadAll(conn)
	_ = conn.Close()

	// A second, healthy tunnel proves the failed connection did not stop the
	// proxy process and that no plaintext was relayed.
	tlsConn := connectMITM(t, proxyAddr, authority, "localhost", mittRootPool(t, env))
	if _, err := io.WriteString(tlsConn, "GET /after-failure HTTP/1.1\r\nHost: "+authority+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write healthy tunneled request: %v", err)
	}
	if _, err := io.ReadAll(tlsConn); err != nil {
		t.Fatalf("read healthy tunneled response: %v", err)
	}
	observed := target.waitRequest(t)
	if observed.line != "GET /after-failure HTTP/1.1" {
		t.Errorf("upstream request line = %q, want only the healthy request", observed.line)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	detail := showMITMError(t, env, "not_tls", "localhost")
	details, _ := detail["details"].(map[string]any)
	if details["error"] == nil {
		t.Errorf("not_tls details = %v, want the handshake error", detail["details"])
	}
}

func TestProxyJSONEmitsMITMErrorNDJSON(t *testing.T) {
	env := newTestEnv(t)
	proxyAddr, proxy := env.startCaptureProxy(t, "--events", "--deny-targets", "blocked.example")

	response := sendRaw(t, proxyAddr,
		"GET http://blocked.example/private HTTP/1.1\r\n"+
			"Host: blocked.example\r\n"+
			"Connection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 403 ") {
		t.Fatalf("plain HTTP response = %q, want 403", firstLine(response))
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	lines := historyLines(proxy.stdout.String())
	if len(lines) != 1 {
		t.Fatalf("proxy stdout has %d NDJSON lines, want 1:\n%s", len(lines), proxy.stdout.String())
	}
	event := decodeJSONObject(t, lines[0])
	if event["kind"] != "mitm_error" {
		t.Errorf("event kind = %v, want mitm_error", event["kind"])
	}
	if event["reason"] != "scope_denied" {
		t.Errorf("event reason = %v, want scope_denied", event["reason"])
	}
	if event["target_host"] != "blocked.example" {
		t.Errorf("event target_host = %v, want blocked.example", event["target_host"])
	}
	details, _ := event["details"].(map[string]any)
	if details["matched"] != "deny" {
		t.Errorf("event details = %v, want matched=deny", event["details"])
	}
}

func TestMITMUpstreamConnectFailureWritesError(t *testing.T) {
	env := newTestEnv(t)
	proxyAddr, proxy := env.startCaptureProxy(t)

	response := sendPlainCONNECT(t, proxyAddr, "127.0.0.1:1")
	if !strings.HasPrefix(response, "HTTP/1.1 502 ") {
		t.Errorf("CONNECT response = %q, want 502 for an unreachable upstream", firstLine(response))
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	detail := showMITMError(t, env, "upstream_connect", "127.0.0.1")
	details, _ := detail["details"].(map[string]any)
	if details["error"] == nil {
		t.Errorf("upstream_connect details = %v, want the dial error", detail["details"])
	}
}

func TestScopeFlagsRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	start, ok := doc.Command.find("mitt proxy start")
	if !ok {
		t.Fatal("spec missing mitt proxy start")
	}
	for _, name := range []string{"allow-targets", "deny-targets"} {
		flag, ok := flagByName(start.Flags, name)
		if !ok {
			t.Errorf("proxy start missing --%s", name)
			continue
		}
		if flag.Type != "string" {
			t.Errorf("--%s type = %q, want string", name, flag.Type)
		}
		if flag.Inherited {
			t.Errorf("--%s must be a proxy start flag, not inherited", name)
		}
		if flag.Description == "" {
			t.Errorf("--%s description must not be empty", name)
		}
	}
}
