package cli_test

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSendTLSProfileRegisteredInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	send, ok := doc.Command.find("mitt send")
	if !ok {
		t.Fatal("spec missing mitt send")
	}
	flag, ok := flagByName(send.Flags, "tls-profile")
	if !ok {
		t.Fatal("send --tls-profile is not registered in spec")
	}
	if flag.Type != "string" {
		t.Errorf("send --tls-profile type = %q, want string", flag.Type)
	}
	if flag.Default != "standard" {
		t.Errorf("send --tls-profile default = %q, want standard", flag.Default)
	}
	if want := []string{"standard", "chrome", "firefox"}; !reflect.DeepEqual(flag.Enum, want) {
		t.Errorf("send --tls-profile enum = %v, want %v", flag.Enum, want)
	}
}

func TestSendRejectsInvalidTLSProfile(t *testing.T) {
	env := newTestEnv(t)
	got := env.runWithStore(t, "send", "--url", "http://127.0.0.1:1/", "--tls-profile", "safari")
	requireExit(t, got, 2)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("code = %v, want INVALID_REQUEST", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["value"] != "safari" {
		t.Errorf("details.value = %v, want safari", details["value"])
	}
	if _, err := os.Stat(env.store); !os.IsNotExist(err) {
		t.Errorf("invalid --tls-profile touched the data dir: %v", err)
	}
}

func TestSendSchemaRejectsTLSProfile(t *testing.T) {
	env := newTestEnv(t)
	got := env.runWithStore(t, "send", "--schema", "--tls-profile", "chrome")
	requireExit(t, got, 2)
	if code := errorEnvelope(t, got)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("code = %v, want INVALID_REQUEST", code)
	}
}

func TestSendTLSProfileCompletesHandshake(t *testing.T) {
	for _, profile := range []string{"standard", "chrome", "firefox"} {
		t.Run(profile, func(t *testing.T) {
			env := newTestEnv(t)
			ca := newTestUpstreamCA(t)
			cert, _ := ca.issueTargetCert(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
			target := startTLSTarget(t, cert, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			caPath := filepath.Join(t.TempDir(), "upstream-ca.pem")
			if err := os.WriteFile(caPath, ca.pem, 0o600); err != nil {
				t.Fatalf("write upstream CA: %v", err)
			}

			got := env.runWithEnv(t, []string{"SSL_CERT_FILE=" + caPath},
				"--store", env.store, "send", "--url", "https://"+target.addr+"/profile", "--tls-profile", profile)
			requireExit(t, got, 0)
			if out := decodeJSONObject(t, got.stdout); out["status"] != float64(200) {
				t.Errorf("status = %v, want 200", out["status"])
			}
			if observed := target.waitRequest(t); observed.line != "GET /profile HTTP/1.1" {
				t.Errorf("request line = %q, want GET /profile HTTP/1.1", observed.line)
			}
			if alpn := target.waitALPN(t); alpn != "http/1.1" {
				t.Errorf("negotiated ALPN = %q, want http/1.1", alpn)
			}
		})
	}
}

// startTLSProfileTarget starts a hello-recording TLS target and returns an
// isolated mitt environment plus the environment entries that trust the test
// CA through SSL_CERT_FILE.
func startTLSProfileTarget(t *testing.T) (*testEnv, *helloRecordingTarget, []string) {
	t.Helper()
	env := newTestEnv(t)
	ca := newTestUpstreamCA(t)
	cert, _ := ca.issueTargetCert(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	target := startHelloRecordingTarget(t, cert, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	caPath := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if err := os.WriteFile(caPath, ca.pem, 0o600); err != nil {
		t.Fatalf("write upstream CA: %v", err)
	}
	return env, target, []string{"SSL_CERT_FILE=" + caPath}
}

// sendAndRecordHello sends one request and returns the ClientHello the target
// recorded. An empty profile leaves --tls-profile unset.
func sendAndRecordHello(t *testing.T, env *testEnv, envVars []string, target *helloRecordingTarget, path, profile string) clientHelloRecord {
	t.Helper()
	args := []string{"--store", env.store, "send", "--url", "https://" + target.addr + path}
	if profile != "" {
		args = append(args, "--tls-profile", profile)
	}
	got := env.runWithEnv(t, envVars, args...)
	requireExit(t, got, 0)
	hello := target.waitHello(t)
	target.waitRequest(t)
	return hello
}

func TestSendTLSProfileChangesClientHelloFingerprint(t *testing.T) {
	env, target, envVars := startTLSProfileTarget(t)

	fingerprints := map[string]string{}
	for _, profile := range []string{"standard", "chrome", "firefox"} {
		hello := sendAndRecordHello(t, env, envVars, target, "/"+profile, profile)
		if want := []string{"http/1.1"}; !reflect.DeepEqual(hello.alpn, want) {
			t.Errorf("%s advertised ALPN = %v, want %v", profile, hello.alpn, want)
		}
		fingerprints[profile] = hello.ja3
		t.Logf("%s ClientHello fingerprint: %s", profile, hello.ja3)
	}

	if fingerprints["standard"] == fingerprints["chrome"] {
		t.Errorf("standard and chrome share a ClientHello fingerprint: %s", fingerprints["standard"])
	}
	if fingerprints["standard"] == fingerprints["firefox"] {
		t.Errorf("standard and firefox share a ClientHello fingerprint: %s", fingerprints["standard"])
	}
	if fingerprints["chrome"] == fingerprints["firefox"] {
		t.Errorf("chrome and firefox share a ClientHello fingerprint: %s", fingerprints["chrome"])
	}
}

func TestSendTLSProfileDefaultsToStandard(t *testing.T) {
	env, target, envVars := startTLSProfileTarget(t)

	standard := sendAndRecordHello(t, env, envVars, target, "/standard", "standard")
	defaulted := sendAndRecordHello(t, env, envVars, target, "/default", "")
	if defaulted.ja3 != standard.ja3 {
		t.Errorf("default ClientHello fingerprint = %s, want standard %s", defaulted.ja3, standard.ja3)
	}
}

func TestProxyCaptureUsesStandardTLSRegardlessOfSendProfile(t *testing.T) {
	env, target, envVars := startTLSProfileTarget(t)

	standard := sendAndRecordHello(t, env, envVars, target, "/standard", "standard")
	chrome := sendAndRecordHello(t, env, envVars, target, "/chrome", "chrome")

	proxyAddr, _ := env.startCaptureProxyWithEnv(t, envVars)
	tlsConn := connectMITM(t, proxyAddr, target.addr, "127.0.0.1", mittRootPool(t, env))
	request := "GET /via-proxy HTTP/1.1\r\nHost: " + target.addr + "\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(tlsConn, request); err != nil {
		t.Fatalf("write tunneled request: %v", err)
	}
	if _, err := io.ReadAll(tlsConn); err != nil {
		t.Fatalf("read tunneled response: %v", err)
	}

	proxyHello := target.waitHello(t)
	if observed := target.waitRequest(t); observed.line != "GET /via-proxy HTTP/1.1" {
		t.Errorf("upstream request line = %q, want GET /via-proxy HTTP/1.1", observed.line)
	}
	if proxyHello.ja3 != standard.ja3 {
		t.Errorf("proxy upstream ClientHello fingerprint = %s, want the standard library fingerprint %s", proxyHello.ja3, standard.ja3)
	}
	if proxyHello.ja3 == chrome.ja3 {
		t.Errorf("proxy upstream ClientHello fingerprint matches chrome: %s", proxyHello.ja3)
	}
	if want := []string{"http/1.1"}; !reflect.DeepEqual(proxyHello.alpn, want) {
		t.Errorf("proxy upstream advertised ALPN = %v, want %v", proxyHello.alpn, want)
	}
}

func TestSendTLSProfileMapsHandshakeFailureToUpstreamTLS(t *testing.T) {
	for _, profile := range []string{"chrome", "firefox"} {
		t.Run(profile, func(t *testing.T) {
			env := newTestEnv(t)
			targetCA := newTestUpstreamCA(t)
			cert, _ := targetCA.issueTargetCert(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
			target := startTLSTarget(t, cert, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

			unrelatedCA := newTestUpstreamCA(t)
			unrelatedPath := filepath.Join(t.TempDir(), "unrelated-ca.pem")
			if err := os.WriteFile(unrelatedPath, unrelatedCA.pem, 0o600); err != nil {
				t.Fatalf("write unrelated CA: %v", err)
			}
			got := env.runWithEnv(t, []string{"SSL_CERT_FILE=" + unrelatedPath},
				"--store", env.store, "send", "--url", "https://"+target.addr+"/tls", "--tls-profile", profile)
			requireExit(t, got, 1)
			errObj := errorEnvelope(t, got)
			if errObj["code"] != "UPSTREAM_TLS" {
				t.Errorf("code = %v, want UPSTREAM_TLS", errObj["code"])
			}
			if errObj["retryable"] != false {
				t.Errorf("retryable = %v, want false", errObj["retryable"])
			}
		})
	}
}
