package replay

import (
	"crypto/tls"
	"fmt"
	"net"

	utls "github.com/refraction-networking/utls"

	"github.com/C1ph3rX13/mitt/internal/output"
)

// TLSProfile selects the ClientHello fingerprint used for the outbound TLS
// handshake of a direct send. It never affects the capture proxy.
type TLSProfile string

const (
	TLSProfileStandard TLSProfile = "standard"
	TLSProfileChrome   TLSProfile = "chrome"
	TLSProfileFirefox  TLSProfile = "firefox"
)

// TLSProfiles lists the accepted --tls-profile values in stable order.
var TLSProfiles = []string{
	string(TLSProfileStandard),
	string(TLSProfileChrome),
	string(TLSProfileFirefox),
}

// ParseTLSProfile validates a --tls-profile value. An empty value selects
// standard, which is also the CLI default.
func ParseTLSProfile(value string) (TLSProfile, error) {
	profile := TLSProfile(value)
	if profile == "" {
		return TLSProfileStandard, nil
	}
	for _, allowed := range TLSProfiles {
		if string(profile) == allowed {
			return profile, nil
		}
	}
	return "", output.NewError(
		output.CodeInvalidRequest,
		fmt.Sprintf("--tls-profile %q is not supported", value),
		"use one of: standard, chrome, firefox",
	).WithDetail("value", value).WithDetail("allowed", TLSProfiles)
}

// handshakeTLS completes an outbound TLS handshake with the requested
// ClientHello fingerprint. Every profile advertises only http/1.1 so send
// always stays on HTTP/1.1.
func handshakeTLS(conn net.Conn, serverName string, profile TLSProfile) (net.Conn, error) {
	nextProtos := []string{"http/1.1"}
	switch profile {
	case TLSProfileChrome:
		return handshakeUTLS(conn, serverName, nextProtos, utls.HelloChrome_Auto)
	case TLSProfileFirefox:
		return handshakeUTLS(conn, serverName, nextProtos, utls.HelloFirefox_Auto)
	default:
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: serverName,
			NextProtos: nextProtos,
		})
		if err := tlsConn.Handshake(); err != nil {
			return nil, err
		}
		return tlsConn, nil
	}
}

// handshakeUTLS mimics a browser ClientHello from uTLS while keeping the ALPN
// offer at http/1.1 only.
func handshakeUTLS(conn net.Conn, serverName string, nextProtos []string, clientHelloID utls.ClientHelloID) (net.Conn, error) {
	uconn := utls.UClient(conn, &utls.Config{
		ServerName: serverName,
		NextProtos: nextProtos,
	}, clientHelloID)
	if err := uconn.BuildHandshakeState(); err != nil {
		return nil, err
	}
	// Browser specs hard-code an h2-first ALPN offer. send always speaks
	// HTTP/1.1, so replace the offer on the preset extension. The extension
	// object is shared with the client-hello spec, so it survives the
	// handshake's internal rebuild.
	for _, extension := range uconn.Extensions {
		if alpn, ok := extension.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = nextProtos
		}
	}
	if err := uconn.Handshake(); err != nil {
		return nil, err
	}
	return uconn, nil
}
