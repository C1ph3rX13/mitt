//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/C1ph3rX13/mitt/internal/ca"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// DefaultAddr is the foreground proxy's default listen address.
const DefaultAddr = "0.0.0.0:8080"

const (
	shutdownTimeout  = 3 * time.Second
	stopWaitTimeout  = 4 * time.Second
	stopPollInterval = 25 * time.Millisecond
)

var (
	// ErrLocked reports that another proxy owns the data directory.
	ErrLocked = errors.New("another proxy is already running for this data directory")
	// ErrAddrInUse reports that the listen address is already bound.
	ErrAddrInUse = errors.New("listen address is already in use")
	// ErrInvalidAddr reports a malformed listen address.
	ErrInvalidAddr = errors.New("invalid listen address")
	// ErrInterrupted reports a clean SIGINT shutdown; the CLI maps it to 130.
	ErrInterrupted = errors.New("interrupted by SIGINT")
)

// LockedError carries the runtime state of the owner that rejected a start.
type LockedError struct {
	State RunState
}

func (e *LockedError) Error() string { return ErrLocked.Error() }
func (e *LockedError) Unwrap() error { return ErrLocked }

// AddrError carries the rejected listen address.
type AddrError struct {
	Addr  string
	Err   error
	InUse bool
}

func (e *AddrError) Error() string {
	if e.InUse {
		return fmt.Sprintf("listen address %s is already in use", e.Addr)
	}
	return fmt.Sprintf("invalid listen address %q: %v", e.Addr, e.Err)
}

func (e *AddrError) Unwrap() error {
	if e.InUse {
		return ErrAddrInUse
	}
	return ErrInvalidAddr
}

// StartOptions describes one foreground proxy run.
type StartOptions struct {
	// Plan owns the listen endpoints and the root data directory.
	Plan      ListenPlan
	Authority *ca.Authority
	Scope     *Scope
	// Identity decides whether a pid belongs to this proxy binary; the CLI
	// supplies it so stale-state recovery never trusts an unrelated process.
	Identity ProcessIdentity
	// HTTP2ALPN selects the ALPN protocols advertised during CONNECT MITM.
	// The zero value keeps the original HTTP/1.1-only behavior.
	HTTP2ALPN ALPNMode
	Events    io.Writer
	Stderr    io.Writer
}

// Start runs the capture proxy in the foreground until SIGTERM or SIGINT. One
// proxyRun owns the run: the data-directory lock, the bound listeners, the
// published proxy.json, and the servers. Start acquires it, publishes
// readiness, and serves until a signal or a listener failure; releasing that
// same run is the only teardown path, so every failure before or after
// readiness gives the ports, the lock, and the record back by the same rules.
func Start(opts StartOptions) error {
	// Register the handler before any resource is acquired. A stop can land
	// while the endpoints are being bound or at the instant proxy.json becomes
	// visible, and it must reach the run that owns the cleanup instead of the
	// default disposition, which would kill the process and leave state behind.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	run, err := acquireRun(opts)
	if err != nil {
		return err
	}
	defer run.release()

	if err := run.publish(); err != nil {
		return err
	}
	serveErr := run.serve()

	select {
	case sig := <-signals:
		if sig == syscall.SIGINT {
			return ErrInterrupted
		}
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("proxy listener failed: %w", err)
		}
		return nil
	}
}

// proxyRun is one foreground proxy run and the owner of everything it
// acquires: the data-directory lock, the bound listeners, the published
// proxy.json, and the servers accepting on those listeners.
//
// Acquisition either returns a run that owns every resource it touched, or
// gives back what it already took and returns an error. Release is idempotent
// and is the only place the teardown order lives, so a failed bind, a failed
// state publication, a signal, and a listener failure cannot each invent their
// own cleanup. Publication is a step of the run rather than a side effect of
// startup: nothing is visible through `proxy status` until every endpoint is
// bound and the data directories are ready.
type proxyRun struct {
	opts  StartOptions
	store *session.Store
	lock  *session.Lock
	bound []boundListener
	// ownsRecord records whether proxy.json in this store may be removed by
	// this run. A live proxy that predates the directory lock owns its record;
	// a run that merely observed it must never delete it on the way out.
	ownsRecord bool
	servers    []*Server
	released   bool
}

// publishRunState is the run's single write of proxy.json. It is a variable so
// a test can reproduce the persistence protocol's late failure — the record
// renamed into place, the parent-directory sync failed — and prove that
// release removes a record it can no longer trust.
var publishRunState = func(store *session.Store, state *session.ProxyState) error {
	return store.WriteProxyState(state)
}

// acquireRun takes the data-directory lock, reconciles the runtime record,
// binds every endpoint, and prepares the data directories. On any failure it
// releases everything taken so far: the caller sees either a ready-to-publish
// run or a store with no lock, no bound port, and no state left behind.
func acquireRun(opts StartOptions) (*proxyRun, error) {
	plan := opts.Plan
	if plan.root == nil || len(plan.endpoints) == 0 {
		return nil, errors.New("proxy: Start requires a plan built by NewListenPlan")
	}
	if opts.Authority == nil {
		return nil, errors.New("proxy: Start requires a CA authority")
	}
	store := plan.root
	lock, err := store.AcquireLock()
	if err != nil {
		if errors.Is(err, session.ErrLocked) {
			return nil, &LockedError{State: lockedRunState(store)}
		}
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	run := &proxyRun{opts: opts, store: store, lock: lock}
	fail := func(err error) (*proxyRun, error) {
		run.release()
		return nil, err
	}

	state, err := readRunState(store, opts.Identity)
	if err != nil {
		return fail(fmt.Errorf("read proxy state: %w", err))
	}
	if state.Running() {
		// A live proxy predating the directory lock still owns the store.
		return fail(&LockedError{State: state})
	}
	// From here on the store is this run's: it holds the lock, and the only
	// record that could exist is a stale one it is about to replace, so release
	// may delete whatever proxy.json it finds.
	run.ownsRecord = true
	if state.Stale() {
		warnStale(opts.Stderr, store.ProxyStatePath(), state)
		if err := store.RemoveProxyState(); err != nil {
			return fail(fmt.Errorf("remove stale proxy.json: %w", err))
		}
	}

	for _, target := range plan.Endpoints() {
		listener, err := net.Listen(listenNetwork(target.Addr), target.Addr)
		if err != nil {
			if plan.fromRange {
				// A --port-range run treats any failed bind as ADDR_IN_USE so
				// the caller can retry the whole range after clearing the port.
				return fail(&AddrError{Addr: target.Addr, Err: err, InUse: true})
			}
			return fail(listenError(target.Addr, err))
		}
		run.bound = append(run.bound, boundListener{listener: listener, store: target.Store})
	}
	if err := run.prepareStores(opts.Stderr); err != nil {
		return fail(err)
	}
	return run, nil
}

// prepareStores sweeps every data directory this run owns, root first: with
// --session-per-port the root store still receives `mitt send` records and the
// port stores receive captures, so all of them can hold crash residue.
// Sweeping a shared store once is enough, and live writers elsewhere (a
// concurrent `send`, another proxy) hold the per-file writer lock.
func (r *proxyRun) prepareStores(stderr io.Writer) error {
	swept := map[string]bool{r.store.Dir(): true}
	r.store.SweepStaleTemps(stderr)
	for _, listener := range r.bound {
		if err := listener.store.EnsureLayout(); err != nil {
			return fmt.Errorf("prepare data directory %s: %w", listener.store.Dir(), err)
		}
		if swept[listener.store.Dir()] {
			continue
		}
		swept[listener.store.Dir()] = true
		listener.store.SweepStaleTemps(stderr)
	}
	return nil
}

// publish makes the run observable: proxy.json names the bound listeners, the
// process, and the CA. It is the last step before serving, so a record never
// describes a proxy that failed to bind a port or prepare a store. A write
// failure — including one reported after the record landed — is returned to
// Start, whose deferred release removes the record and gives the ports and the
// lock back.
func (r *proxyRun) publish() error {
	listeners := make([]session.ProxyListener, 0, len(r.bound))
	for _, listener := range r.bound {
		listeners = append(listeners, session.ProxyListener{
			Addr:  listener.listener.Addr().String(),
			Store: listener.store.Dir(),
		})
	}
	state := &session.ProxyState{
		PID:                 os.Getpid(),
		Addr:                listeners[0].Addr,
		StartedAt:           time.Now().UTC().Format(time.RFC3339Nano),
		CAFingerprintSHA256: ca.Fingerprint(r.opts.Authority.CertPEM),
		Listeners:           listeners,
	}
	if err := publishRunState(r.store, state); err != nil {
		return fmt.Errorf("write proxy.json: %w", err)
	}
	return nil
}

// serve builds one server per bound listener, shares the event emitter so
// concurrent NDJSON events stay on whole lines, and starts accepting. The
// returned channel receives from every server, so whichever listener fails
// first ends the select in Start; release performs the actual shutdown.
func (r *proxyRun) serve() <-chan error {
	emitter := newEventEmitter(r.opts.Events)
	servers := make([]*Server, 0, len(r.bound))
	for _, listener := range r.bound {
		capture := newCapture(listener.store, r.opts.Authority, r.opts.Scope, r.opts.HTTP2ALPN, r.opts.Stderr, emitter)
		servers = append(servers, NewServer(listener.listener, capture))
	}
	r.servers = servers
	serveErr := make(chan error, len(servers))
	for _, server := range servers {
		go func(server *Server) { serveErr <- server.Serve() }(server)
	}
	return serveErr
}

// release gives back every resource the run acquired, in one order, and is
// safe to call more than once:
//
//  1. stop accepting and drain in-flight exchanges, so no record can be
//     appended after the run's own record disappears;
//  2. close a listener that a failed or skipped shutdown left bound;
//  3. remove proxy.json — the run holds the directory lock, and any record in
//     the store is its own or a stale one it already replaced;
//  4. release the data-directory lock, which is what lets a restart proceed.
func (r *proxyRun) release() {
	if r == nil || r.released {
		return
	}
	r.released = true
	shutdownAll(r.servers)
	for _, listener := range r.bound {
		_ = listener.listener.Close()
	}
	if r.ownsRecord {
		_ = r.store.RemoveProxyState()
	}
	r.lock.Release()
}

type boundListener struct {
	listener net.Listener
	store    *session.Store
}

func shutdownAll(servers []*Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Add(1)
		go func(server *Server) {
			defer wg.Done()
			if err := server.Shutdown(ctx); err != nil {
				_ = server.Close()
			}
		}(server)
	}
	wg.Wait()
}

func listenError(addr string, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return &AddrError{Addr: addr, Err: err, InUse: true}
	}
	var addrErr *net.AddrError
	if errors.As(err, &addrErr) {
		return &AddrError{Addr: addr, Err: addrErr}
	}
	return fmt.Errorf("listen on %s: %w", addr, err)
}

// listenNetwork keeps IPv4 wildcard binds on tcp4. With the generic "tcp"
// network, Go normalizes 0.0.0.0 to the IPv6 dual-stack wildcard [::], which
// would make proxy.json report an address the caller never asked for.
func listenNetwork(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "tcp"
	}
	ip := net.ParseIP(host)
	switch {
	case host == "":
		return "tcp4"
	case ip != nil && ip.To4() != nil:
		return "tcp4"
	case ip != nil:
		return "tcp6"
	case strings.Contains(host, ":"):
		// IPv6 with a zone, e.g. [fe80::1%lo]:8080.
		return "tcp6"
	default:
		return "tcp"
	}
}
