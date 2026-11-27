//go:build linux

package proxy

import "testing"

func TestScopeAllowsAndDeniesHosts(t *testing.T) {
	tests := []struct {
		name  string
		allow string
		deny  string
		host  string
		want  bool
	}{
		{name: "no scope allows everything", host: "anything.example", want: true},
		{name: "allow exact match", allow: "example.com", host: "example.com", want: true},
		{name: "allow comma list and whitespace", allow: " first.example , *.Example.com ", host: "API.example.com:443", want: true},
		{name: "allow exact does not match a subdomain", allow: "example.com", host: "api.example.com", want: false},
		{name: "allow is case insensitive and strips the port", allow: "Example.COM", host: "EXAMPLE.com:8443", want: true},
		{name: "allow miss denies", allow: "example.com", host: "other.example", want: false},
		{name: "deny exact match", deny: "blocked.example", host: "blocked.example", want: false},
		{name: "deny comma list", deny: "first.example, blocked.example", host: "blocked.example", want: false},
		{name: "deny only blocks its own host", deny: "blocked.example", host: "example.com", want: true},
		{name: "deny wins over allow", allow: "example.com", deny: "example.com", host: "example.com", want: false},
		{name: "allow wildcard matches subdomain", allow: "*.example.com", host: "api.example.com", want: true},
		{name: "allow wildcard matches nested subdomain", allow: "*.example.com", host: "a.b.example.com", want: true},
		{name: "allow wildcard does not match apex", allow: "*.example.com", host: "example.com", want: false},
		{name: "allow wildcard does not match a lookalike suffix", allow: "*.example.com", host: "badexample.com", want: false},
		{name: "allow wildcard does not match a longer suffix", allow: "*.example.com", host: "example.com.evil", want: false},
		{name: "deny wildcard matches subdomain", deny: "*.evil.example", host: "api.evil.example", want: false},
		{name: "deny wildcard does not match apex", deny: "*.evil.example", host: "evil.example", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := NewScope(tt.allow, tt.deny)
			if got := scope.Check(tt.host).Allowed; got != tt.want {
				t.Fatalf("Check(%q) with allow=%q deny=%q = %v, want %v", tt.host, tt.allow, tt.deny, got, tt.want)
			}
		})
	}
}

func TestScopeReportsWhyItDenied(t *testing.T) {
	scope := NewScope("*.example.com", "blocked.example.com")
	decision := scope.Check("blocked.example.com")
	if decision.Allowed {
		t.Fatal("deny match was allowed")
	}
	if decision.Matched != "deny" || decision.Pattern != "blocked.example.com" {
		t.Fatalf("deny decision = %+v, want matched=deny pattern=blocked.example.com", decision)
	}

	decision = scope.Check("other.example")
	if decision.Allowed {
		t.Fatal("allow miss was allowed")
	}
	if decision.Matched != "allow" {
		t.Fatalf("allow miss decision = %+v, want matched=allow", decision)
	}
}
