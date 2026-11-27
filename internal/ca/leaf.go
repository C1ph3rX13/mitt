//go:build linux

package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// leafValidity is how long an on-the-fly leaf certificate stays valid.
const leafValidity = 365 * 24 * time.Hour

// LeafIssuer signs and caches per-host server certificates with the root CA.
// The cache lives for the lifetime of the proxy process, so one host always
// receives the same certificate across CONNECT tunnels.
type LeafIssuer struct {
	authority *Authority

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// NewLeafIssuer builds an issuer for the loaded root CA.
func NewLeafIssuer(authority *Authority) *LeafIssuer {
	return &LeafIssuer{
		authority: authority,
		cache:     map[string]*tls.Certificate{},
	}
}

// Certificate returns the cached or freshly issued leaf for host. DNS names
// get a DNS SAN; IP literals get an IPAddress SAN. No wildcard SANs are ever
// synthesised: foo.example.com and example.com are separate cache entries.
func (l *LeafIssuer) Certificate(host string) (*tls.Certificate, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, fmt.Errorf("cannot issue a leaf certificate for an empty host")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if certificate, ok := l.cache[host]; ok {
		return certificate, nil
	}

	certificate, err := l.issue(host)
	if err != nil {
		return nil, err
	}
	l.cache[host] = certificate
	return certificate, nil
}

func (l *LeafIssuer) issue(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key for %s: %w", host, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, fmt.Errorf("generate leaf serial for %s: %w", host, err)
	}

	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host, Organization: []string{"mitt"}},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, l.authority.Cert, &key.PublicKey, l.authority.key)
	if err != nil {
		return nil, fmt.Errorf("sign leaf certificate for %s: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse leaf certificate for %s: %w", host, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// normalizeHost lowercases DNS names and removes a trailing root dot. IP
// literals are returned in canonical form.
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}
