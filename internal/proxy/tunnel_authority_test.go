//go:build linux

package proxy

import "testing"

// newTestUpstreamFor builds the tunnel-authority parts of a mitmUpstream
// without dialing anything. Every field the identity rules read is set from
// the CONNECT authority, so the tunnel can be asked about requests directly.
func newTestUpstreamFor(authority string) *mitmUpstream {
	host, port, err := connectAuthority(authority)
	if err != nil {
		panic(err)
	}
	return &mitmUpstream{host: host, address: host + ":" + port}
}

func TestNewOriginUsesTheTunnelAuthority(t *testing.T) {
	upstream := newTestUpstreamFor("example.test:8443")
	origin, err := upstream.newOrigin("/a?q=1")
	if err != nil {
		t.Fatalf("newOrigin: %v", err)
	}
	if got := origin.URL(); got != "https://example.test:8443/a?q=1" {
		t.Errorf("URL() = %q, want the tunnel origin plus the target", got)
	}
	if got := origin.WireTarget(); got != "/a?q=1" {
		t.Errorf("WireTarget() = %q", got)
	}
}

func TestNewOriginKeepsAsteriskFormOffThePath(t *testing.T) {
	upstream := newTestUpstreamFor("example.test:443")
	origin, err := upstream.newOrigin("*")
	if err != nil {
		t.Fatalf("newOrigin: %v", err)
	}
	if got := origin.URL(); got != "https://example.test:443" {
		t.Errorf("URL() = %q, want the bare tunnel origin", got)
	}
	if got := origin.WireTarget(); got != "*" {
		t.Errorf("WireTarget() = %q, want *", got)
	}
}

func TestNewOriginRejectsCrossHostTargets(t *testing.T) {
	upstream := newTestUpstreamFor("example.test:443")
	if _, err := upstream.newOrigin("http://other.test/"); err == nil {
		t.Fatal("newOrigin accepted an absolute-form target inside a tunnel")
	}
}

func TestAuthorityMismatchComparesHostsNotPorts(t *testing.T) {
	// The tunnel was opened for example.test on the default port. Host values
	// are compared by hostname, so an omitted port is not a mismatch, but a
	// different host is.
	upstream := newTestUpstreamFor("example.test")
	cases := []struct {
		name      string
		authority string
		want      bool
	}{
		{name: "absent authority", authority: "", want: false},
		{name: "same host", authority: "example.test", want: false},
		{name: "same host with default port", authority: "example.test:443", want: false},
		{name: "same host different casing", authority: "EXAMPLE.test:443", want: false},
		{name: "trailing root dot", authority: "example.test.", want: false},
		{name: "other host", authority: "other.test:443", want: true},
		{name: "other host without port", authority: "other.test", want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := upstream.authorityMismatch(testCase.authority); got != testCase.want {
				t.Errorf("authorityMismatch(%q) = %v, want %v", testCase.authority, got, testCase.want)
			}
		})
	}
}
