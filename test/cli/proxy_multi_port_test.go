package cli_test

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProxyStartRejectsInvalidPortRangeFlags(t *testing.T) {
	cases := []struct {
		name    string
		flags   []string
		details map[string]any
	}{
		{name: "non numeric", flags: []string{"--port-range", "a-b"}, details: map[string]any{"port_range": "a-b"}},
		{name: "zero", flags: []string{"--port-range", "0-1"}, details: map[string]any{"port_range": "0-1"}},
		{name: "descending", flags: []string{"--port-range", "8082-8081"}, details: map[string]any{"port_range": "8082-8081"}},
		{name: "too high", flags: []string{"--port-range", "65535-65536"}, details: map[string]any{"port_range": "65535-65536"}},
		{name: "single port", flags: []string{"--port-range", "8080"}, details: map[string]any{"port_range": "8080"}},
		{name: "extra hyphen", flags: []string{"--port-range", "8080-8081-8082"}, details: map[string]any{"port_range": "8080-8081-8082"}},
		{name: "missing end", flags: []string{"--port-range", "8080-"}, details: map[string]any{"port_range": "8080-"}},
		{name: "empty", flags: []string{"--port-range", ""}, details: map[string]any{"port_range": ""}},
		{name: "space around hyphen", flags: []string{"--port-range", "8080 - 8081"}, details: map[string]any{"port_range": "8080 - 8081"}},
		// The flag combination is a declared constraint now, so the details
		// name the rule that rejected it instead of restating the plan error.
		{name: "session per port alone", flags: []string{"--session-per-port"}, details: map[string]any{
			"constraint": "session-per-port-needs-range",
			"requires":   "--port-range",
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			env := newTestEnv(t)
			args := append([]string{"--addr", "127.0.0.1:0"}, test.flags...)
			got := env.runWithStore(t, append([]string{"proxy", "start"}, args...)...)

			requireExit(t, got, 2)
			errObj := errorEnvelope(t, got)
			if errObj["code"] != "INVALID_REQUEST" {
				t.Errorf("error.code = %v, want INVALID_REQUEST", errObj["code"])
			}
			details, _ := errObj["details"].(map[string]any)
			for key, want := range test.details {
				if details[key] != want {
					t.Errorf("details.%s = %v, want %v", key, details[key], want)
				}
			}
			if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
				t.Errorf("invalid flags must not write proxy.json, stat err = %v", err)
			}
		})
	}
}

func TestProxyStartAcceptsPortRangeBeforeCAValidation(t *testing.T) {
	env := newTestEnv(t)
	start, _ := freePortRange(t, 2)

	got := env.runWithStore(t, "proxy", "start",
		"--addr", "127.0.0.1:0",
		"--port-range", fmt.Sprintf("%d-%d", start, start+1),
	)

	requireExit(t, got, 78)
	if code := errorEnvelope(t, got)["code"]; code != "CA_MISSING" {
		t.Errorf("error.code = %v, want CA_MISSING", code)
	}
}

func TestProxyStartRejectsInvalidAddrWithPortRange(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "proxy", "start",
		"--addr", "foo:bar:baz",
		"--port-range", "8080-8081",
	)

	requireExit(t, got, 2)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("error.code = %v, want INVALID_REQUEST", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["addr"] != "foo:bar:baz" {
		t.Errorf("details.addr = %v, want the rejected --addr", details["addr"])
	}
	if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
		t.Errorf("invalid --addr must not write proxy.json, stat err = %v", err)
	}
}

// TestProxyStartPortRangeReportsAddrInUseForSinglePort pins the range rule:
// every bind failure in a --port-range run is ADDR_IN_USE, even when a == b.
// The listen plan has to remember where it came from to keep that distinction.
func TestProxyStartPortRangeReportsAddrInUseForSinglePort(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	defer blocked.Close()
	port := blocked.Addr().(*net.TCPAddr).Port
	blockedAddr := fmt.Sprintf("127.0.0.1:%d", port)

	got := env.runWithStore(t, "proxy", "start",
		"--addr", "127.0.0.1:0",
		"--port-range", fmt.Sprintf("%d-%d", port, port),
	)

	requireExit(t, got, 69)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "ADDR_IN_USE" {
		t.Errorf("error.code = %v, want ADDR_IN_USE", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["addr"] != blockedAddr {
		t.Errorf("details.addr = %v, want the busy port %s", details["addr"], blockedAddr)
	}
}

func TestProxyStartListensOnPortRangeWithSharedStore(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)
	start, addrs := freePortRange(t, 2)

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	targetAddr, requests := startRawTarget(t, targetResponse)

	proxy := env.startProxy(t,
		"--addr", "127.0.0.1:0",
		"--port-range", fmt.Sprintf("%d-%d", start, start+1),
	)
	status := proxy.waitReady(t, env)
	listeners := statusListeners(t, status)
	requireListenerPorts(t, listeners, addrs)
	for i, listener := range listeners {
		if got := listenerStore(t, listener); got != env.store {
			t.Errorf("listener[%d].store = %q, want shared root %q", i, got, env.store)
		}
	}
	if status["addr"] != addrs[0] {
		t.Errorf("status.addr = %v, want first listener %q", status["addr"], addrs[0])
	}

	for i, addr := range addrs {
		path := fmt.Sprintf("/shared-%d", i)
		request := "GET http://" + targetAddr + path + " HTTP/1.1\r\n" +
			"Host: " + targetAddr + "\r\n" +
			"Connection: close\r\n\r\n"
		if response := sendRaw(t, addr, request); response != targetResponse {
			t.Fatalf("response through %s = %q, want %q", addr, response, targetResponse)
		}
		waitObserved(t, requests)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	if exit := proxy.waitExit(t, 5*time.Second); exit != 0 {
		t.Errorf("proxy exit code = %d, want 0\n%s", exit, proxy.failState(t))
	}
	for _, addr := range addrs {
		assertPortFree(t, addr)
	}

	got := env.runWithStore(t, "history")
	requireExit(t, got, 0)
	lines := historyLines(got.stdout)
	if len(lines) != 2 {
		t.Fatalf("history returned %d lines, want 2 (one per port):\n%s", len(lines), got.stdout)
	}
	for i, line := range lines {
		summary := decodeJSONObject(t, line)
		wantURL := fmt.Sprintf("http://%s/shared-%d", targetAddr, i)
		if summary["url"] != wantURL {
			t.Errorf("history[%d].url = %v, want %s", i, summary["url"], wantURL)
		}
	}
}

// freePortRange finds a consecutive run of free TCP ports on 127.0.0.1 and
// returns its first port plus the listener addresses.
func freePortRange(t *testing.T, count int) (int, []string) {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		first, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve first port: %v", err)
		}
		start := first.Addr().(*net.TCPAddr).Port
		listeners := []net.Listener{first}
		free := true
		for port := start + 1; port < start+count; port++ {
			candidate, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				free = false
				break
			}
			listeners = append(listeners, candidate)
		}
		for _, listener := range listeners {
			_ = listener.Close()
		}
		if !free {
			continue
		}
		addrs := make([]string, 0, count)
		for port := start; port < start+count; port++ {
			addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", port))
		}
		return start, addrs
	}
	t.Fatalf("could not reserve %d consecutive free ports", count)
	return 0, nil
}

func assertPortFree(t *testing.T, addr string) {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s is still bound after the proxy exited: %v", addr, err)
	}
	_ = listener.Close()
}

func statusListeners(t *testing.T, status map[string]any) []map[string]any {
	t.Helper()
	raw, ok := status["listeners"].([]any)
	if !ok {
		t.Fatalf("status.listeners = %#v, want an array", status["listeners"])
	}
	listeners := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		listener, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("status listener = %#v, want an object", item)
		}
		listeners = append(listeners, listener)
	}
	return listeners
}

func listenerAddr(t *testing.T, listener map[string]any) string {
	t.Helper()
	addr, _ := listener["addr"].(string)
	if addr == "" {
		t.Fatalf("listener.addr is missing: %v", listener)
	}
	return addr
}

func listenerStore(t *testing.T, listener map[string]any) string {
	t.Helper()
	store, _ := listener["store"].(string)
	if store == "" {
		t.Fatalf("listener.store is missing: %v", listener)
	}
	return store
}

func requireListenerPorts(t *testing.T, listeners []map[string]any, addrs []string) {
	t.Helper()
	if len(listeners) != len(addrs) {
		t.Fatalf("listeners = %v, want %d", listeners, len(addrs))
	}
	for i, addr := range addrs {
		if got := listenerAddr(t, listeners[i]); got != addr {
			t.Errorf("listener[%d].addr = %q, want %q", i, got, addr)
		}
	}
}

func TestProxyStartSessionPerPortIsolatesStores(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)
	start, addrs := freePortRange(t, 2)
	ports := []int{start, start + 1}
	portStores := []string{
		filepath.Join(env.store, "ports", strconv.Itoa(ports[0])),
		filepath.Join(env.store, "ports", strconv.Itoa(ports[1])),
	}

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	targetAddr, requests := startRawTarget(t, targetResponse)

	proxy := env.startProxy(t,
		"--addr", "127.0.0.1:0",
		"--port-range", fmt.Sprintf("%d-%d", ports[0], ports[1]),
		"--session-per-port",
	)
	status := proxy.waitReady(t, env)
	listeners := statusListeners(t, status)
	requireListenerPorts(t, listeners, addrs)
	for i, listener := range listeners {
		if got := listenerStore(t, listener); got != portStores[i] {
			t.Errorf("listener[%d].store = %q, want %q", i, got, portStores[i])
		}
	}

	disk := env.readProxyState(t)
	diskListeners, _ := disk["listeners"].([]any)
	if len(diskListeners) != 2 {
		t.Fatalf("proxy.json listeners = %v, want 2", disk["listeners"])
	}
	for i, item := range diskListeners {
		listener, _ := item.(map[string]any)
		if got := listenerStore(t, listener); got != portStores[i] {
			t.Errorf("proxy.json listener[%d].store = %q, want %q", i, got, portStores[i])
		}
	}

	requestViaProxy(t, addrs[0], targetAddr, "/first-port")
	waitObserved(t, requests)

	assertHistoryURLs(t, env, portStores[0], []string{"http://" + targetAddr + "/first-port"})
	assertNoHistory(t, env, portStores[1])
	assertNoHistory(t, env, env.store)
	if got := len(bodyFileNames(t, portStores[0])); got != 2 {
		t.Errorf("first port bodies = %d files, want request and response", got)
	}
	if got := bodyFileNames(t, portStores[1]); len(got) != 0 {
		t.Errorf("second port bodies before traffic = %v, want none", got)
	}

	requestViaProxy(t, addrs[1], targetAddr, "/second-port")
	waitObserved(t, requests)

	assertHistoryURLs(t, env, portStores[0], []string{"http://" + targetAddr + "/first-port"})
	assertHistoryURLs(t, env, portStores[1], []string{"http://" + targetAddr + "/second-port"})
	assertNoHistory(t, env, env.store)
	if got := len(bodyFileNames(t, portStores[1])); got != 2 {
		t.Errorf("second port bodies = %d files, want request and response", got)
	}

	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	if exit := proxy.waitExit(t, 5*time.Second); exit != 0 {
		t.Errorf("proxy exit code = %d, want 0\n%s", exit, proxy.failState(t))
	}
	for _, addr := range addrs {
		assertPortFree(t, addr)
	}
}

func requestViaProxy(t *testing.T, proxyAddr, targetAddr, path string) {
	t.Helper()
	request := "GET http://" + targetAddr + path + " HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Connection: close\r\n\r\n"
	response := sendRaw(t, proxyAddr, request)
	if !strings.Contains(response, " 200 ") {
		t.Fatalf("response through %s = %q, want 200", proxyAddr, response)
	}
}

func assertHistoryURLs(t *testing.T, env *testEnv, store string, want []string) {
	t.Helper()
	got := env.run(t, "--store", store, "history")
	requireExit(t, got, 0)
	lines := historyLines(got.stdout)
	if len(lines) != len(want) {
		t.Fatalf("history in %s returned %d lines, want %d:\n%s", store, len(lines), len(want), got.stdout)
	}
	for i, line := range lines {
		summary := decodeJSONObject(t, line)
		if summary["url"] != want[i] {
			t.Errorf("history[%d].url in %s = %v, want %s", i, store, summary["url"], want[i])
		}
	}
}

func assertNoHistory(t *testing.T, env *testEnv, store string) {
	t.Helper()
	got := env.run(t, "--store", store, "history")
	requireExit(t, got, 66)
	if code := errorEnvelope(t, got)["code"]; code != "NO_MATCH" {
		t.Errorf("history in %s code = %v, want NO_MATCH", store, code)
	}
}

func TestProxyStartClosesBoundListenersWhenAnyPortFails(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	start, blocked := blockedSecondPort(t)
	defer blocked.Close()
	blockedAddr := fmt.Sprintf("127.0.0.1:%d", start+1)
	firstAddr := fmt.Sprintf("127.0.0.1:%d", start)

	got := env.runWithStore(t, "proxy", "start",
		"--addr", "127.0.0.1:0",
		"--port-range", fmt.Sprintf("%d-%d", start, start+1),
	)

	requireExit(t, got, 69)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "ADDR_IN_USE" {
		t.Errorf("error.code = %v, want ADDR_IN_USE", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["addr"] != blockedAddr {
		t.Errorf("details.addr = %v, want the busy port %s", details["addr"], blockedAddr)
	}
	if _, err := os.Stat(env.proxyStatePath()); !os.IsNotExist(err) {
		t.Errorf("failed multi-port start must not write proxy.json, stat err = %v", err)
	}
	assertPortFree(t, firstAddr)
}

// blockedSecondPort reserves a port and returns a free predecessor plus the
// still-held listener for addr+1.
func blockedSecondPort(t *testing.T) (int, net.Listener) {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		candidate, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve candidate port: %v", err)
		}
		start := candidate.Addr().(*net.TCPAddr).Port
		_ = candidate.Close()
		if start+1 > 65535 {
			continue
		}
		blocked, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", start+1))
		if err != nil {
			continue
		}
		probe, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", start))
		if err != nil {
			_ = blocked.Close()
			continue
		}
		_ = probe.Close()
		return start, blocked
	}
	t.Fatal("could not reserve a free/busy consecutive port pair")
	return 0, nil
}

func TestProxyStartRegistersPortFlagsInSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	start, ok := doc.Command.find("mitt proxy start")
	if !ok {
		t.Fatal("spec missing mitt proxy start")
	}
	portRange, ok := flagByName(start.Flags, "port-range")
	if !ok {
		t.Fatal("proxy start missing --port-range")
	}
	if portRange.Type != "string" {
		t.Errorf("--port-range type = %q, want string", portRange.Type)
	}
	if portRange.Default != "" {
		t.Errorf("--port-range default = %q, want empty", portRange.Default)
	}

	sessionPerPort, ok := flagByName(start.Flags, "session-per-port")
	if !ok {
		t.Fatal("proxy start missing --session-per-port")
	}
	if sessionPerPort.Type != "bool" {
		t.Errorf("--session-per-port type = %q, want bool", sessionPerPort.Type)
	}
	if sessionPerPort.Default != "false" {
		t.Errorf("--session-per-port default = %q, want false", sessionPerPort.Default)
	}
}
