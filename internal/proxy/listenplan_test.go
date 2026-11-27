//go:build linux

package proxy

import (
	"errors"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/session"
)

func TestNewListenPlanSingleAddrIsVerbatim(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "localhost", "", "not:valid:host"} {
		t.Run(addr, func(t *testing.T) {
			plan, err := NewListenPlan(ListenPlanRequest{
				Store: session.Open("/store"),
				Addr:  addr,
			})
			if err != nil {
				t.Fatalf("NewListenPlan(addr=%q): %v", addr, err)
			}
			if plan.fromRange {
				t.Error("a single --addr plan must not be marked as a range run")
			}
			endpoints := plan.Endpoints()
			if len(endpoints) != 1 {
				t.Fatalf("Endpoints() = %v, want one endpoint", endpoints)
			}
			if endpoints[0].Addr != addr {
				t.Errorf("endpoint addr = %q, want the verbatim --addr %q", endpoints[0].Addr, addr)
			}
			if got := endpoints[0].Store.Dir(); got != "/store" {
				t.Errorf("endpoint store = %q, want shared root /store", got)
			}
		})
	}
}

func TestNewListenPlanPortRange(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    []string
		wantErr bool
	}{
		{name: "single port", value: "8080-8080", want: []string{"127.0.0.1:8080"}},
		{name: "three ports", value: "8080-8082", want: []string{"127.0.0.1:8080", "127.0.0.1:8081", "127.0.0.1:8082"}},
		{name: "lowest ports", value: "1-2", want: []string{"127.0.0.1:1", "127.0.0.1:2"}},
		{name: "highest ports", value: "65534-65535", want: []string{"127.0.0.1:65534", "127.0.0.1:65535"}},
		{name: "zero", value: "0-1", wantErr: true},
		{name: "descending", value: "1-0", wantErr: true},
		{name: "start above end", value: "2-1", wantErr: true},
		{name: "too high", value: "65535-65536", wantErr: true},
		{name: "missing dash", value: "8080", wantErr: true},
		{name: "missing end", value: "8080-", wantErr: true},
		{name: "missing start", value: "-8080", wantErr: true},
		{name: "extra hyphen", value: "8080-8081-8082", wantErr: true},
		{name: "non numeric", value: "a-b", wantErr: true},
		{name: "leading plus", value: "+8080-8081", wantErr: true},
		{name: "leading space", value: " 8080-8081", wantErr: true},
		{name: "spaces around hyphen", value: "8080 - 8081", wantErr: true},
		{name: "empty", value: "", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan, err := NewListenPlan(ListenPlanRequest{
				Store:        session.Open("/store"),
				Addr:         "127.0.0.1:0",
				PortRange:    test.value,
				PortRangeSet: true,
			})
			if test.wantErr {
				if !errors.Is(err, ErrPortRange) {
					t.Fatalf("NewListenPlan(port-range=%q) err = %v, want ErrPortRange", test.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewListenPlan(port-range=%q): %v", test.value, err)
			}
			if !plan.fromRange {
				t.Error("a --port-range plan must be marked as a range run")
			}
			endpoints := plan.Endpoints()
			if len(endpoints) != len(test.want) {
				t.Fatalf("Endpoints() = %v, want %v", endpoints, test.want)
			}
			for i, endpoint := range endpoints {
				if endpoint.Addr != test.want[i] {
					t.Errorf("endpoint[%d].Addr = %q, want %q", i, endpoint.Addr, test.want[i])
				}
				if got := endpoint.Store.Dir(); got != "/store" {
					t.Errorf("endpoint[%d].Store = %q, want shared root /store", i, got)
				}
			}
		})
	}
}

func TestNewListenPlanAddrForms(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		want    []string
		wantErr bool
	}{
		{name: "host and port", addr: "127.0.0.1:8080", want: []string{"127.0.0.1:9000", "127.0.0.1:9001"}},
		{name: "wildcard", addr: "0.0.0.0:0", want: []string{"0.0.0.0:9000", "0.0.0.0:9001"}},
		{name: "empty host", addr: ":8080", want: []string{":9000", ":9001"}},
		{name: "empty addr", addr: "", want: []string{":9000", ":9001"}},
		{name: "host only", addr: "127.0.0.1", want: []string{"127.0.0.1:9000", "127.0.0.1:9001"}},
		{name: "hostname only", addr: "localhost", want: []string{"localhost:9000", "localhost:9001"}},
		{name: "ipv6", addr: "[::1]:8080", want: []string{"[::1]:9000", "[::1]:9001"}},
		{name: "ipv6 host only", addr: "::1", want: []string{"[::1]:9000", "[::1]:9001"}},
		{name: "invalid port", addr: "127.0.0.1:notaport", wantErr: true},
		{name: "invalid host", addr: "foo:bar:baz", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan, err := NewListenPlan(ListenPlanRequest{
				Store:        session.Open("/store"),
				Addr:         test.addr,
				PortRange:    "9000-9001",
				PortRangeSet: true,
			})
			if test.wantErr {
				if !errors.Is(err, ErrInvalidAddr) {
					t.Fatalf("NewListenPlan(addr=%q) err = %v, want ErrInvalidAddr", test.addr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewListenPlan(addr=%q): %v", test.addr, err)
			}
			endpoints := plan.Endpoints()
			if len(endpoints) != len(test.want) {
				t.Fatalf("Endpoints() = %v, want %v", endpoints, test.want)
			}
			for i, endpoint := range endpoints {
				if endpoint.Addr != test.want[i] {
					t.Errorf("endpoint[%d].Addr = %q, want %q", i, endpoint.Addr, test.want[i])
				}
			}
		})
	}
}

func TestNewListenPlanSessionPerPort(t *testing.T) {
	plan, err := NewListenPlan(ListenPlanRequest{
		Store:          session.Open("/store"),
		Addr:           "127.0.0.1:0",
		PortRange:      "9000-9001",
		PortRangeSet:   true,
		SessionPerPort: true,
	})
	if err != nil {
		t.Fatalf("NewListenPlan: %v", err)
	}
	wantDirs := []string{"/store/ports/9000", "/store/ports/9001"}
	endpoints := plan.Endpoints()
	if len(endpoints) != len(wantDirs) {
		t.Fatalf("Endpoints() = %v, want %d endpoints", endpoints, len(wantDirs))
	}
	for i, endpoint := range endpoints {
		if got := endpoint.Store.Dir(); got != wantDirs[i] {
			t.Errorf("endpoint[%d].Store = %q, want %q", i, got, wantDirs[i])
		}
	}
}

func TestNewListenPlanRejectsSessionPerPortWithoutRange(t *testing.T) {
	_, err := NewListenPlan(ListenPlanRequest{
		Store:          session.Open("/store"),
		Addr:           "127.0.0.1:0",
		SessionPerPort: true,
	})
	if !errors.Is(err, ErrSessionPerPort) {
		t.Fatalf("err = %v, want ErrSessionPerPort", err)
	}
}

func TestNewListenPlanRejectsExplicitEmptyRange(t *testing.T) {
	_, err := NewListenPlan(ListenPlanRequest{
		Store:        session.Open("/store"),
		Addr:         "127.0.0.1:0",
		PortRange:    "",
		PortRangeSet: true,
	})
	if !errors.Is(err, ErrPortRange) {
		t.Fatalf("err = %v, want ErrPortRange", err)
	}
}

func TestNewListenPlanRejectsMissingStore(t *testing.T) {
	_, err := NewListenPlan(ListenPlanRequest{Addr: "127.0.0.1:0"})
	if err == nil {
		t.Fatal("NewListenPlan without a store must fail")
	}
}

func TestListenPlanEndpointsAreACopy(t *testing.T) {
	plan, err := NewListenPlan(ListenPlanRequest{
		Store:        session.Open("/store"),
		Addr:         "127.0.0.1:0",
		PortRange:    "9000-9001",
		PortRangeSet: true,
	})
	if err != nil {
		t.Fatalf("NewListenPlan: %v", err)
	}
	endpoints := plan.Endpoints()
	endpoints[0].Addr = "mutated"
	if got := plan.Endpoints()[0].Addr; got == "mutated" {
		t.Error("Endpoints() exposed the plan's internal slice")
	}
}
