//go:build linux

package proxy

import (
	"fmt"
	"strings"
)

// ALPNMode selects the application protocols the CONNECT MITM advertises
// during the client TLS handshake. The default keeps the original behavior:
// only HTTP/1.1 is offered, so h2-capable targets fall back to HTTP/1.1.
type ALPNMode string

const (
	// ALPNModeH1 advertises only HTTP/1.1 (the stable default).
	ALPNModeH1 ALPNMode = "h1"
	// ALPNModeH2 advertises only h2.
	ALPNModeH2 ALPNMode = "h2"
	// ALPNModeH2H1 advertises h2 first and HTTP/1.1 as the fallback.
	ALPNModeH2H1 ALPNMode = "h2,h1"
)

// ALPNModes lists the accepted --http2-alpn values in stable order.
var ALPNModes = []string{
	string(ALPNModeH1),
	string(ALPNModeH2),
	string(ALPNModeH2H1),
}

// ParseALPNMode validates a --http2-alpn value. An empty value selects h1,
// which is also the CLI default.
func ParseALPNMode(value string) (ALPNMode, error) {
	mode := ALPNMode(strings.TrimSpace(value))
	if mode == "" {
		return ALPNModeH1, nil
	}
	for _, allowed := range ALPNModes {
		if string(mode) == allowed {
			return mode, nil
		}
	}
	return "", fmt.Errorf("--http2-alpn %q is not supported", value)
}

// AllowsH2 reports whether the mode advertises h2 to clients.
func (m ALPNMode) AllowsH2() bool {
	return m == ALPNModeH2 || m == ALPNModeH2H1
}

// ClientProtocols returns the client-facing ALPN advertisement for the mode.
func (m ALPNMode) ClientProtocols() []string {
	switch m {
	case ALPNModeH2:
		return []string{"h2"}
	case ALPNModeH2H1:
		return []string{"h2", "http/1.1"}
	default:
		return []string{"http/1.1"}
	}
}

// UpstreamProtocols returns the upstream ALPN advertisement. Whenever h2 is
// enabled the proxy offers both protocols so an upstream that still speaks
// HTTP/1.1 can be bridged instead of failing the tunnel.
func (m ALPNMode) UpstreamProtocols() []string {
	if m.AllowsH2() {
		return []string{"h2", "http/1.1"}
	}
	return []string{"http/1.1"}
}
