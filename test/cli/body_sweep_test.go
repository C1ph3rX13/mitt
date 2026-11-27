package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// seedTempWriter creates a staged file in dir and ages it past the quiet
// period, simulating what a crashed mitt process leaves behind. It goes through
// the real protocol so the residue has exactly the name, permissions and lock
// state a writer produces; holdLock keeps the stage open, standing in for a
// writer that is still running.
func seedTempWriter(t *testing.T, dir, base string, holdLock bool) string {
	t.Helper()
	stage, err := atomicfile.NewStage(filepath.Join(dir, base), atomicfile.Private)
	if err != nil {
		t.Fatalf("stage %s: %v", base, err)
	}
	if _, err := stage.Write([]byte("orphaned bytes")); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	if holdLock {
		t.Cleanup(stage.Abandon)
	} else {
		stage.Abandon()
	}
	old := time.Now().Add(-2 * time.Hour)
	temp := stage.TempPath()
	if err := os.Chtimes(temp, old, old); err != nil {
		t.Fatalf("age temp: %v", err)
	}
	return temp
}

// seedBodyTempWriter seeds crash residue in a store's bodies directory, where
// staged entity bodies live.
func seedBodyTempWriter(t *testing.T, store string, holdLock bool) string {
	t.Helper()
	return seedTempWriter(t, filepath.Join(store, "bodies"), "entry-req.bin", holdLock)
}

func requireExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s should exist: %v", path, err)
	}
}

func TestProxyStartSweepsCrashLeftBodyTemps(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	temp := seedBodyTempWriter(t, env.store, false)
	historyPath := filepath.Join(env.store, "history.jsonl")
	if err := os.WriteFile(historyPath, []byte("{\"id\":\"kept\",\"kind\":\"sent\"}\n"), 0o600); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	_, proxy := env.startCaptureProxy(t)
	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("proxy start left the crashed body temp behind: %v", err)
	}
	if !strings.Contains(proxy.stderr.String(), "removed 1 stale temporary file") {
		t.Errorf("proxy stderr does not report the sweep:\n%s", proxy.stderr.String())
	}
	history, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if string(history) != "{\"id\":\"kept\",\"kind\":\"sent\"}\n" {
		t.Errorf("sweep changed history: %q", history)
	}
}

func TestProxyStartKeepsBodyTempHeldByAnotherProcess(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	temp := seedBodyTempWriter(t, env.store, true)

	_, proxy := env.startCaptureProxy(t)
	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	requireExists(t, temp)
	if strings.Contains(proxy.stderr.String(), "removed") {
		t.Errorf("proxy stderr claims it removed a live temp file:\n%s", proxy.stderr.String())
	}
}

func TestSendSweepsCrashLeftBodyTemps(t *testing.T) {
	env := newTestEnv(t)
	targetAddr, requests := startRawTarget(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")

	temp := seedBodyTempWriter(t, env.store, false)

	result := env.runWithStore(t, "send", "--url", "http://"+targetAddr+"/sweep")
	requireExit(t, result, 0)
	waitObserved(t, requests)
	if !strings.Contains(result.stderr, "removed 1 stale temporary file") {
		t.Errorf("send stderr does not report the sweep:\n%s", result.stderr)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("send left the crashed body temp behind: %v", err)
	}

	// The clean record still lands: a sweep must not disturb a real commit.
	if got := env.runWithStore(t, "history"); got.exit != 0 {
		t.Fatalf("history after sweep failed: %s", got.stderr)
	}
}

// TestProxyStartSweepsRootStoreUnderSessionPerPort pins the layout split: with
// --session-per-port captures land in ports/<port>/ but `mitt send` still
// writes the root store, so a per-port run must sweep both.
func TestProxyStartSweepsRootStoreUnderSessionPerPort(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	rootTemp := seedBodyTempWriter(t, env.store, false)
	portStore := filepath.Join(env.store, "ports")
	if err := os.MkdirAll(portStore, 0o700); err != nil {
		t.Fatalf("create ports dir: %v", err)
	}

	start, _ := freePortRange(t, 2)
	_, proxy := env.startCaptureProxy(t,
		"--port-range", fmt.Sprintf("%d-%d", start, start+1),
		"--session-per-port",
	)
	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	if _, err := os.Stat(rootTemp); !os.IsNotExist(err) {
		t.Fatalf("per-port proxy start left the root crash residue behind: %v", err)
	}
	if !strings.Contains(proxy.stderr.String(), "removed 1 stale temporary file") {
		t.Errorf("proxy stderr does not report the root sweep:\n%s", proxy.stderr.String())
	}
}

// TestProxyStartSweepsStoreRootAndCADirectory pins the protocol's reach beyond
// bodies: proxy.json and the CA are written through the same staged protocol,
// so a crash there leaves residue the same sweep must clear.
func TestProxyStartSweepsStoreRootAndCADirectory(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	rootTemp := seedTempWriter(t, env.store, "proxy.json", false)
	caTemp := seedTempWriter(t, filepath.Join(env.store, "ca"), "ca.key.pem", false)
	historyPath := filepath.Join(env.store, "history.jsonl")
	if err := os.WriteFile(historyPath, []byte("{\"id\":\"kept\",\"kind\":\"sent\"}\n"), 0o600); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	_, proxy := env.startCaptureProxy(t)
	requireExit(t, env.runWithStore(t, "proxy", "stop"), 0)
	proxy.waitExit(t, 5*time.Second)

	for _, temp := range []string{rootTemp, caTemp} {
		if _, err := os.Stat(temp); !os.IsNotExist(err) {
			t.Errorf("proxy start left crash residue behind: %v", err)
		}
	}
	stderr := proxy.stderr.String()
	if !strings.Contains(stderr, "removed 1 stale temporary file(s) from "+env.store+"\n") {
		t.Errorf("proxy stderr does not report the store-root sweep:\n%s", stderr)
	}
	if !strings.Contains(stderr, "removed 1 stale temporary file(s) from "+filepath.Join(env.store, "ca")) {
		t.Errorf("proxy stderr does not report the CA-directory sweep:\n%s", stderr)
	}
	history, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if string(history) != "{\"id\":\"kept\",\"kind\":\"sent\"}\n" {
		t.Errorf("sweep changed history: %q", history)
	}
}
