//go:build linux

// Package proxy owns the foreground capture proxy. It accepts HTTP/1.1
// connections directly (rather than through net/http.Server) so the original
// request line and header order survive forwarding and persistence.
package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/C1ph3rX13/mitt/internal/ca"
	"github.com/C1ph3rX13/mitt/internal/session"
)

const (
	dialTimeout = 10 * time.Second
)

// Capture receives proxied exchanges and persists them as history events.
type Capture struct {
	store     *session.Store
	leaves    *ca.LeafIssuer
	scope     *Scope
	http2ALPN ALPNMode
	stderr    io.Writer
	events    *eventEmitter
}

// NewCapture builds the capture pipeline for one store. events may be nil,
// which keeps the default silent proxy behavior. The authority signs the leaf
// certificates used for CONNECT MITM. scope may be nil, which allows all
// targets. http2ALPN selects the TLS ALPN advertisement; an empty value keeps
// the HTTP/1.1-only default.
func NewCapture(store *session.Store, authority *ca.Authority, scope *Scope, http2ALPN ALPNMode, stderr io.Writer, events io.Writer) *Capture {
	return newCapture(store, authority, scope, http2ALPN, stderr, newEventEmitter(events))
}

// newCapture lets a multi-listener run share one emitter so concurrent --json
// events stay on whole NDJSON lines.
func newCapture(store *session.Store, authority *ca.Authority, scope *Scope, http2ALPN ALPNMode, stderr io.Writer, emitter *eventEmitter) *Capture {
	if http2ALPN == "" {
		http2ALPN = ALPNModeH1
	}
	return &Capture{
		store:     store,
		leaves:    ca.NewLeafIssuer(authority),
		scope:     scope,
		http2ALPN: http2ALPN,
		stderr:    stderr,
		events:    emitter,
	}
}

// Server accepts connections on one bound listener until Shutdown or Close.
type Server struct {
	listener net.Listener
	capture  *Capture

	mu           sync.Mutex
	conns        map[net.Conn]*connState
	shuttingDown bool
	wg           sync.WaitGroup
}

type connState struct {
	idle bool
}

// NewServer wraps an already-bound listener. Binding happens before the server
// is constructed so startup can report ADDR_IN_USE before writing proxy.json.
func NewServer(listener net.Listener, capture *Capture) *Server {
	return &Server{
		listener: listener,
		capture:  capture,
		conns:    map[net.Conn]*connState{},
	}
}

// Serve blocks until the listener is closed or fails.
func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			shuttingDown := s.shuttingDown
			s.mu.Unlock()
			if shuttingDown || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		s.mu.Lock()
		if s.shuttingDown {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.conns[conn] = &connState{}
		s.wg.Add(1)
		s.mu.Unlock()

		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// Shutdown stops accepting connections, closes idle keep-alive connections,
// and waits up to ctx for in-flight exchanges to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	idle := make([]net.Conn, 0, len(s.conns))
	for conn, state := range s.conns {
		if state.idle {
			idle = append(idle, conn)
		}
	}
	s.mu.Unlock()

	_ = s.listener.Close()
	for _, conn := range idle {
		_ = conn.Close()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.closeConns()
		return ctx.Err()
	}
}

// Close forcibly closes listeners and active connections.
func (s *Server) Close() error {
	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()
	err := s.listener.Close()
	s.closeConns()
	return err
}

func (s *Server) closeConns() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	s.capture.serveConnection(conn, func() bool { return s.markIdle(conn) }, func() bool { return s.markActive(conn) })
}

// markIdle flags a connection as waiting for its next request. It returns
// false when shutdown has already begun, in which case the connection must be
// closed instead of blocking in another read.
func (s *Server) markIdle(conn net.Conn) bool {
	s.mu.Lock()
	shuttingDown := s.shuttingDown
	if state := s.conns[conn]; state != nil && !shuttingDown {
		state.idle = true
	}
	s.mu.Unlock()
	return !shuttingDown
}

func (s *Server) markActive(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown {
		return false
	}
	if state := s.conns[conn]; state != nil {
		state.idle = false
	}
	return true
}
