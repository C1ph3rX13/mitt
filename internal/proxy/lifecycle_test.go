//go:build linux

package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/C1ph3rX13/mitt/internal/ca"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// startTestAuthority loads a throwaway CA for tests that need a run to
// publish a record. ECDSA generation keeps this cheap, and the directory is
// under t.TempDir() so nothing outlives the test.
func startTestAuthority(t *testing.T) *ca.Authority {
	t.Helper()
	authority, err := ca.Init(t.TempDir())
	if err != nil {
		t.Fatalf("init test CA: %v", err)
	}
	return authority
}

// singleEndpointPlan builds a plan whose one endpoint is bound to port 0.
func singleEndpointPlan(t *testing.T, store *session.Store) ListenPlan {
	t.Helper()
	plan, err := NewListenPlan(ListenPlanRequest{Store: store, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("new listen plan: %v", err)
	}
	return plan
}

// requirePortFree reports whether a recorded listener address can be bound
// again. The run must have given the port back, not merely unlinked a file.
func requirePortFree(t *testing.T, addr string) {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s is still bound after release: %v", addr, err)
	}
	_ = listener.Close()
}

// requireStoreUnlocked proves the data-directory lock was released: a second
// owner must be able to take it immediately.
func requireStoreUnlocked(t *testing.T, store *session.Store) {
	t.Helper()
	lock, err := store.AcquireLock()
	if err != nil {
		t.Fatalf("data directory still locked after release: %v", err)
	}
	lock.Release()
}

func requireNoProxyState(t *testing.T, store *session.Store) {
	t.Helper()
	if _, err := os.Stat(store.ProxyStatePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("proxy.json must not survive a released run, stat err = %v", err)
	}
}

// TestAcquireRunReleasesBindingsAndLockWhenAPortFails is the report's
// acceptance case for a failed start: the first endpoint was already bound
// when the second bind failed, so the module must give back that port and the
// lock while the process is still alive.
func TestAcquireRunReleasesBindingsAndLockWhenAPortFails(t *testing.T) {
	store := session.Open(t.TempDir())
	start, blocked := blockedSecondPortPair(t)
	defer blocked.Close()

	plan, err := NewListenPlan(ListenPlanRequest{
		Store:        store,
		Addr:         "127.0.0.1:0",
		PortRange:    fmt.Sprintf("%d-%d", start, start+1),
		PortRangeSet: true,
	})
	if err != nil {
		t.Fatalf("new listen plan: %v", err)
	}

	run, err := acquireRun(StartOptions{Plan: plan, Authority: startTestAuthority(t), Identity: acceptAll})
	if err == nil {
		run.release()
		t.Fatal("acquireRun must fail when the second port is busy")
	}
	var addrErr *AddrError
	if !errors.As(err, &addrErr) || !addrErr.InUse {
		t.Fatalf("err = %v, want an in-use AddrError", err)
	}
	if addrErr.Addr != fmt.Sprintf("127.0.0.1:%d", start+1) {
		t.Errorf("AddrError.Addr = %q, want the busy port", addrErr.Addr)
	}

	requirePortFree(t, fmt.Sprintf("127.0.0.1:%d", start))
	requireStoreUnlocked(t, store)
	requireNoProxyState(t, store)
}

// TestAcquireRunLeavesLiveRecordAlone pins the ownership rule: a live proxy
// that predates the directory lock must be refused without its record being
// reaped by the refused start.
func TestAcquireRunLeavesLiveRecordAlone(t *testing.T) {
	store := session.Open(t.TempDir())
	writeTestProxyState(t, store, os.Getpid())

	plan := singleEndpointPlan(t, store)
	_, err := acquireRun(StartOptions{Plan: plan, Authority: startTestAuthority(t), Identity: acceptAll})

	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("err = %v, want LockedError", err)
	}
	if _, statErr := os.Stat(store.ProxyStatePath()); statErr != nil {
		t.Errorf("a refused start must not remove the live owner's record: %v", statErr)
	}
	requireStoreUnlocked(t, store)
}

// TestReleaseAfterPublishFailureGivesEverythingBack reproduces the report's
// second case: proxy.json reached the filesystem but its write reported an
// error (the directory sync failed). The run must remove the record it cannot
// trust, unbind every port, and release the lock — all while the process is
// still running.
func TestReleaseAfterPublishFailureGivesEverythingBack(t *testing.T) {
	store := session.Open(t.TempDir())
	plan := singleEndpointPlan(t, store)

	run, err := acquireRun(StartOptions{Plan: plan, Authority: startTestAuthority(t), Identity: acceptAll})
	if err != nil {
		t.Fatalf("acquire run: %v", err)
	}
	addr := run.bound[0].listener.Addr().String()

	realPublish := publishRunState
	t.Cleanup(func() { publishRunState = realPublish })
	publishRunState = func(store *session.Store, state *session.ProxyState) error {
		// The protocol's late failure: bytes renamed into place, directory
		// sync failed. The record is visible even though the write failed.
		if err := store.WriteProxyState(state); err != nil {
			t.Fatalf("write state: %v", err)
		}
		return errors.New("injected: sync directory failed after publish")
	}

	if err := run.publish(); err == nil {
		t.Fatal("publish must report the injected failure")
	}
	if _, statErr := os.Stat(store.ProxyStatePath()); statErr != nil {
		t.Fatalf("the injected failure must leave a visible record, stat err = %v", statErr)
	}

	run.release()

	requireNoProxyState(t, store)
	requirePortFree(t, addr)
	requireStoreUnlocked(t, store)
}

// TestPublishReportsBoundEndpoints proves readiness names what is actually
// bound: the record carries the listener addresses the kernel assigned, not
// the requested port 0.
func TestPublishReportsBoundEndpoints(t *testing.T) {
	store := session.Open(t.TempDir())
	plan := singleEndpointPlan(t, store)

	run, err := acquireRun(StartOptions{Plan: plan, Authority: startTestAuthority(t), Identity: acceptAll})
	if err != nil {
		t.Fatalf("acquire run: %v", err)
	}
	defer run.release()

	if err := run.publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	state, err := store.ReadProxyState()
	if err != nil {
		t.Fatalf("read published state: %v", err)
	}
	if len(state.Listeners) != 1 {
		t.Fatalf("listeners = %d, want 1", len(state.Listeners))
	}
	if got, want := state.Listeners[0].Addr, run.bound[0].listener.Addr().String(); got != want {
		t.Errorf("listener addr = %q, want the bound address %q", got, want)
	}
	if state.Addr != state.Listeners[0].Addr {
		t.Errorf("state.addr = %q, want the first listener %q", state.Addr, state.Listeners[0].Addr)
	}
}

// TestReleaseIsIdempotentAndSafeWithoutServers keeps the single teardown path
// honest: a second release (or one after a start that never served) must not
// panic, rebind, or resurrect the record.
func TestReleaseIsIdempotentAndSafeWithoutServers(t *testing.T) {
	store := session.Open(t.TempDir())
	plan := singleEndpointPlan(t, store)

	run, err := acquireRun(StartOptions{Plan: plan, Authority: startTestAuthority(t), Identity: acceptAll})
	if err != nil {
		t.Fatalf("acquire run: %v", err)
	}
	if err := run.publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	addr := run.bound[0].listener.Addr().String()

	run.release()
	run.release()

	requireNoProxyState(t, store)
	requirePortFree(t, addr)
	requireStoreUnlocked(t, store)
}

// TestServeReturnsWhenListenerCloses exercises the post-readiness end of the
// run: once serving stops, release is what unbinds the remaining listeners and
// clears the record. A listener closed behind Serve's back is the run's own
// shutdown (Serve reports it as a clean stop), so the test only pins that
// serving ends and teardown still gives everything back.
func TestServeReturnsWhenListenerCloses(t *testing.T) {
	store := session.Open(t.TempDir())
	plan := singleEndpointPlan(t, store)

	run, err := acquireRun(StartOptions{Plan: plan, Authority: startTestAuthority(t), Identity: acceptAll})
	if err != nil {
		t.Fatalf("acquire run: %v", err)
	}
	if err := run.publish(); err != nil {
		run.release()
		t.Fatalf("publish: %v", err)
	}
	addr := run.bound[0].listener.Addr().String()
	serveErr := run.serve()

	// A close from outside Serve is this run's own shutdown; Serve must stop
	// instead of spinning on the closed listener.
	_ = run.bound[0].listener.Close()
	select {
	case <-serveErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its listener closed")
	}

	run.release()
	requireNoProxyState(t, store)
	requirePortFree(t, addr)
	requireStoreUnlocked(t, store)
}

// blockedSecondPortPair reserves a busy port and returns a free predecessor so
// a two-port range fails on its second bind. Every attempt is a direct
// net.Listen call, so the loop cannot hang.
func blockedSecondPortPair(t *testing.T) (int, net.Listener) {
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

// TestStartHandlesStopDuringPublish is the deterministic form of "a stop that
// lands the instant readiness becomes visible still cleans up": SIGTERM is
// delivered from inside the state write, after proxy.json is already in place
// but before Start reaches its select. Nothing is registered later than that
// instant, so the handler start installed before acquiring anything must catch
// it; otherwise the process dies by default disposition with a record and a
// bound port behind it.
func TestStartHandlesStopDuringPublish(t *testing.T) {
	store := session.Open(t.TempDir())
	addr := reserveFreeAddr(t)

	plan, err := NewListenPlan(ListenPlanRequest{Store: store, Addr: addr})
	if err != nil {
		t.Fatalf("new listen plan: %v", err)
	}

	realPublish := publishRunState
	t.Cleanup(func() { publishRunState = realPublish })
	publishRunState = func(store *session.Store, state *session.ProxyState) error {
		if err := realPublish(store, state); err != nil {
			return err
		}
		// proxy.json is visible now. Send the stop before Start can select.
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("signal self: %v", err)
		}
		return nil
	}

	err = Start(StartOptions{
		Plan:      plan,
		Authority: startTestAuthority(t),
		Identity:  acceptAll,
	})
	if err != nil {
		t.Fatalf("Start after SIGTERM = %v, want a clean stop", err)
	}

	requireNoProxyState(t, store)
	requirePortFree(t, addr)
	requireStoreUnlocked(t, store)
}

// TestReleaseIsBoundedWhenUpstreamNeverAnswers keeps a hung exchange from
// holding the whole teardown open. A request whose upstream accepts the
// connection and then never answers leaves the server with an active
// exchange; release may wait for it, but only up to the shutdown deadline —
// after that the forced close ends it. Without that bound one unresponsive
// upstream would wedge `mitt proxy stop` (and this test).
func TestReleaseIsBoundedWhenUpstreamNeverAnswers(t *testing.T) {
	store := session.Open(t.TempDir())
	plan := singleEndpointPlan(t, store)

	// The upstream accepts and never writes: the exchange stays in the middle
	// of reading the response head.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer upstream.Close()
	upstreamAccepted := make(chan struct{})
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		close(upstreamAccepted)
		// Hold the connection open, swallowing whatever the proxy sends, until
		// the test cleanup closes the listener and the read fails.
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
	}()

	run, err := acquireRun(StartOptions{Plan: plan, Authority: startTestAuthority(t), Identity: acceptAll})
	if err != nil {
		t.Fatalf("acquire run: %v", err)
	}
	if err := run.publish(); err != nil {
		run.release()
		t.Fatalf("publish: %v", err)
	}
	proxyAddr := run.bound[0].listener.Addr().String()
	run.serve()

	client, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		run.release()
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	target := upstream.Addr().String()
	request := "GET http://" + target + "/ HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
	if _, err := client.Write([]byte(request)); err != nil {
		run.release()
		t.Fatalf("write request: %v", err)
	}
	// The upstream accepting the request is what proves the client connection
	// is now an in-flight exchange rather than an idle keep-alive socket.
	select {
	case <-upstreamAccepted:
	case <-time.After(5 * time.Second):
		run.release()
		t.Fatal("proxy did not forward the request upstream within 5s")
	}

	released := make(chan struct{})
	go func() {
		run.release()
		close(released)
	}()

	// The budget is the shutdown deadline plus room for the forced close, not
	// an unbounded wait on the upstream.
	budget := shutdownTimeout + 5*time.Second
	select {
	case <-released:
	case <-time.After(budget):
		t.Fatalf("release did not return within %s while an upstream held an exchange", budget)
	}

	requireNoProxyState(t, store)
	requirePortFree(t, proxyAddr)
	requireStoreUnlocked(t, store)
}

// reserveFreeAddr finds a bindable 127.0.0.1 address so a test can name the
// port before the run binds it. Like the CLI's freePortRange, it probes by
// binding and closing; the loop is bounded.
func reserveFreeAddr(t *testing.T) string {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve address: %v", err)
		}
		addr := listener.Addr().String()
		if err := listener.Close(); err != nil {
			continue
		}
		return addr
	}
	t.Fatal("could not reserve a free address")
	return ""
}
