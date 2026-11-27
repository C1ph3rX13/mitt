//go:build linux

package proxy

import (
	"errors"
	"fmt"

	"github.com/C1ph3rX13/mitt/internal/session"
)

var (
	// ErrPortRange reports a malformed --port-range value.
	ErrPortRange = errors.New("invalid port range")
	// ErrSessionPerPort reports --session-per-port without --port-range.
	ErrSessionPerPort = errors.New("--session-per-port requires --port-range")
)

// ListenEndpoint is one listener a plan binds. Store is never nil; it is the
// data directory that receives this listener's events.
type ListenEndpoint struct {
	Addr  string
	Store *session.Store
}

// ListenPlanRequest is the CLI-level listen configuration. PortRangeSet
// distinguishes an absent --port-range from an explicitly empty one, which is
// invalid rather than "not set".
type ListenPlanRequest struct {
	Store          *session.Store
	Addr           string
	PortRange      string
	PortRangeSet   bool
	SessionPerPort bool
}

// ListenPlan is an immutable resolution of the listen flags: the root data
// directory plus one endpoint per listening socket. Build it with
// NewListenPlan; Start accepts nothing else.
type ListenPlan struct {
	root      *session.Store
	endpoints []ListenEndpoint
	// fromRange records that the plan came from --port-range. A range run maps
	// every bind failure to ADDR_IN_USE; a single --addr run keeps the finer
	// invalid-address classification.
	fromRange bool
}

// NewListenPlan resolves (addr, port-range, session-per-port) into listen
// endpoints. Without --port-range the address is taken verbatim as one
// endpoint; with it, the host comes from addr and each port becomes an
// endpoint. --session-per-port gives each endpoint <store>/ports/<port>.
func NewListenPlan(req ListenPlanRequest) (ListenPlan, error) {
	if req.Store == nil {
		return ListenPlan{}, errors.New("listen plan: store is required")
	}
	if req.SessionPerPort && !req.PortRangeSet {
		return ListenPlan{}, ErrSessionPerPort
	}
	if !req.PortRangeSet {
		return ListenPlan{
			root:      req.Store,
			endpoints: []ListenEndpoint{{Addr: req.Addr, Store: req.Store}},
		}, nil
	}

	ports, err := parsePortRange(req.PortRange)
	if err != nil {
		return ListenPlan{}, err
	}
	addrs, err := listenerAddresses(req.Addr, ports)
	if err != nil {
		return ListenPlan{}, fmt.Errorf("%w %q: %v", ErrInvalidAddr, req.Addr, err)
	}

	endpoints := make([]ListenEndpoint, 0, len(addrs))
	for i, addr := range addrs {
		store := req.Store
		if req.SessionPerPort {
			store = req.Store.PortStore(ports[i])
		}
		endpoints = append(endpoints, ListenEndpoint{Addr: addr, Store: store})
	}
	return ListenPlan{root: req.Store, endpoints: endpoints, fromRange: true}, nil
}

// RootStore is the data directory that owns proxy.json and the run lock.
func (p ListenPlan) RootStore() *session.Store {
	return p.root
}

// Endpoints returns a copy of the plan's listen endpoints.
func (p ListenPlan) Endpoints() []ListenEndpoint {
	return append([]ListenEndpoint(nil), p.endpoints...)
}
