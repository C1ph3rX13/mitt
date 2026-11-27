package httpwire

import (
	"net/url"
	"testing"
)

func TestNewOriginValidatesAndRendersBothForms(t *testing.T) {
	origin, err := NewOrigin("HTTPS", "example.test:8443", "/a/b?q=1")
	if err != nil {
		t.Fatalf("NewOrigin: %v", err)
	}
	if got := origin.URL(); got != "https://example.test:8443/a/b?q=1" {
		t.Errorf("URL() = %q", got)
	}
	if got := origin.WireTarget(); got != "/a/b?q=1" {
		t.Errorf("WireTarget() = %q", got)
	}

	asterisk, err := NewOrigin("https", "example.test", "*")
	if err != nil {
		t.Fatalf("NewOrigin(asterisk): %v", err)
	}
	if got := asterisk.URL(); got != "https://example.test" {
		t.Errorf("asterisk URL() = %q, want the bare origin", got)
	}
	if got := asterisk.WireTarget(); got != "*" {
		t.Errorf("asterisk WireTarget() = %q", got)
	}

	empty, err := NewOrigin("http", "example.test", "")
	if err != nil {
		t.Fatalf("NewOrigin(empty target): %v", err)
	}
	if got := empty.URL(); got != "http://example.test" {
		t.Errorf("empty-target URL() = %q, want no trailing slash", got)
	}
	if got := empty.WireTarget(); got != "/" {
		t.Errorf("empty-target WireTarget() = %q, want /", got)
	}
}

func TestNewOriginRejectsUnusableInput(t *testing.T) {
	cases := []struct {
		name      string
		scheme    string
		authority string
		target    string
	}{
		{name: "ftp scheme", scheme: "ftp", authority: "example.test", target: "/"},
		{name: "empty authority", scheme: "https", authority: "", target: "/"},
		{name: "authority with a path", scheme: "https", authority: "example.test/a", target: "/"},
		{name: "absolute-form target", scheme: "https", authority: "example.test", target: "http://other.test/"},
		{name: "target with CR", scheme: "https", authority: "example.test", target: "/a\r\nX: y"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewOrigin(testCase.scheme, testCase.authority, testCase.target); err == nil {
				t.Fatal("NewOrigin accepted an unusable origin")
			}
		})
	}
}

func TestOriginOfURLKeepsTheURLShape(t *testing.T) {
	cases := []struct {
		rawURL     string
		wantURL    string
		wantTarget string
	}{
		{rawURL: "http://example.test/a/b?q=1", wantURL: "http://example.test/a/b?q=1", wantTarget: "/a/b?q=1"},
		{rawURL: "https://example.test", wantURL: "https://example.test", wantTarget: ""},
		{rawURL: "https://example.test/", wantURL: "https://example.test/", wantTarget: "/"},
		{rawURL: "http://example.test/a%2Fb?x=%20", wantURL: "http://example.test/a%2Fb?x=%20", wantTarget: "/a%2Fb?x=%20"},
		{rawURL: "http://example.test/path?", wantURL: "http://example.test/path?", wantTarget: "/path?"},
		{rawURL: "http://example.test?q=1", wantURL: "http://example.test?q=1", wantTarget: "?q=1"},
		{rawURL: "http://user:pass@example.test/a", wantURL: "http://user:pass@example.test/a", wantTarget: "/a"},
	}
	for _, testCase := range cases {
		t.Run(testCase.rawURL, func(t *testing.T) {
			parsed, err := parseURL(testCase.rawURL)
			if err != nil {
				t.Fatalf("parse %q: %v", testCase.rawURL, err)
			}
			origin := OriginOfURL(parsed)
			if got := origin.URL(); got != testCase.wantURL {
				t.Errorf("URL() = %q, want %q", got, testCase.wantURL)
			}
			if got := origin.Target; got != testCase.wantTarget {
				t.Errorf("Target = %q, want %q", got, testCase.wantTarget)
			}
		})
	}
}

func TestVersionOfMapsLineTokensToHistoryTokens(t *testing.T) {
	cases := []struct {
		line      string
		wantLine  string
		wantToken string
	}{
		{line: "HTTP/1.1", wantLine: "HTTP/1.1", wantToken: "http/1.1"},
		{line: "HTTP/1.0", wantLine: "HTTP/1.0", wantToken: "http/1.0"},
		{line: "HTTP/2.0", wantLine: "HTTP/2.0", wantToken: "h2"},
		{line: "HTTP/3", wantLine: "HTTP/3", wantToken: "http/3"},
	}
	for _, testCase := range cases {
		t.Run(testCase.line, func(t *testing.T) {
			version := VersionOf(testCase.line)
			if version.Line != testCase.wantLine || version.Token != testCase.wantToken {
				t.Errorf("VersionOf(%q) = %+v, want {%q %q}", testCase.line, version, testCase.wantLine, testCase.wantToken)
			}
		})
	}
}

func TestOriginWireTargetAddsTheSlashThatOnlyExistsOnTheWire(t *testing.T) {
	cases := []struct {
		target string
		want   string
	}{
		{target: "", want: "/"},
		{target: "/", want: "/"},
		{target: "/a", want: "/a"},
		{target: "?q=1", want: "/?q=1"},
		{target: "*", want: "*"},
	}
	for _, testCase := range cases {
		t.Run(testCase.target, func(t *testing.T) {
			origin := Origin{Scheme: "http", Authority: "example.test", Target: testCase.target}
			if got := origin.WireTarget(); got != testCase.want {
				t.Errorf("WireTarget() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func parseURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}
