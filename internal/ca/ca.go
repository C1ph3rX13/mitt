//go:build linux

// Package ca generates, loads and inspects the mitt root CA.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// Subject is the canonical root CA subject required by the command contract.
const Subject = "CN=mitt Root CA, O=mitt, C=--"

const (
	certFileName  = "ca.pem"
	keyFileName   = "ca.key.pem"
	validForYears = 10
)

var (
	// ErrNotFound reports that neither CA file exists.
	ErrNotFound = errors.New("CA does not exist")
	// ErrInvalid reports that CA files exist but are incomplete or malformed.
	ErrInvalid = errors.New("CA is invalid")
)

// Authority is a loaded root CA together with its PEM encoding and private
// key. The key signs ephemeral leaf certificates for CONNECT MITM.
type Authority struct {
	Cert    *x509.Certificate
	CertPEM []byte
	KeyPEM  []byte
	key     *ecdsa.PrivateKey
}

// Init loads an existing valid CA from dir or generates a new one when the
// directory holds no CA at all. Partially written or corrupt CAs are refused
// instead of being overwritten, so a damaged trust chain is never replaced
// silently.
func Init(dir string) (*Authority, error) {
	authority, err := Load(dir)
	if err == nil {
		return authority, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return generate(dir)
}

// Load reads and validates the CA in dir.
func Load(dir string) (*Authority, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, certFileName))
	if errors.Is(err, fs.ErrNotExist) {
		certPEM = nil
	} else if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrInvalid, certFileName, err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, keyFileName))
	if errors.Is(err, fs.ErrNotExist) {
		keyPEM = nil
	} else if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrInvalid, keyFileName, err)
	}

	switch {
	case certPEM == nil && keyPEM == nil:
		return nil, ErrNotFound
	case certPEM == nil:
		return nil, fmt.Errorf("%w: %s is missing", ErrInvalid, certFileName)
	case keyPEM == nil:
		return nil, fmt.Errorf("%w: %s is missing", ErrInvalid, keyFileName)
	}

	cert, key, err := parse(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if err := validate(cert, key, time.Now()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return &Authority{Cert: cert, CertPEM: certPEM, KeyPEM: keyPEM, key: key}, nil
}

func generate(dir string) (*Authority, error) {
	if err := atomicfile.EnsureDir(dir, atomicfile.Private.DirMode); err != nil {
		return nil, fmt.Errorf("create CA directory: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, fmt.Errorf("generate CA serial number: %w", err)
	}

	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "mitt Root CA",
			Organization: []string{"mitt"},
			Country:      []string{"--"},
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(validForYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode CA private key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Write the key first: without it the certificate is unusable, and a
	// half-written pair is reported as invalid rather than mistaken for a CA.
	keyPath := filepath.Join(dir, keyFileName)
	if err := atomicfile.Write(keyPath, keyPEM, atomicfile.Private); err != nil {
		return nil, fmt.Errorf("write %s: %w", keyFileName, err)
	}
	if err := atomicfile.Write(filepath.Join(dir, certFileName), certPEM, atomicfile.Private); err != nil {
		// Leave no key-only CA behind so a transient failure can be retried.
		_ = os.Remove(keyPath)
		return nil, fmt.Errorf("write %s: %w", certFileName, err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse generated certificate: %w", err)
	}
	return &Authority{Cert: cert, CertPEM: certPEM, KeyPEM: keyPEM, key: key}, nil
}

func parse(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("%w: %s does not contain a CERTIFICATE block", ErrInvalid, certFileName)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: parse %s: %v", ErrInvalid, certFileName, err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("%w: %s does not contain a PEM block", ErrInvalid, keyFileName)
	}
	var key *ecdsa.PrivateKey
	switch keyBlock.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err == nil {
			var ok bool
			key, ok = parsed.(*ecdsa.PrivateKey)
			if !ok {
				err = errors.New("not an ECDSA private key")
			}
		}
	default:
		err = fmt.Errorf("unsupported PEM block %q", keyBlock.Type)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: parse %s: %v", ErrInvalid, keyFileName, err)
	}
	return cert, key, nil
}

func validate(cert *x509.Certificate, key *ecdsa.PrivateKey, now time.Time) error {
	if cert.Subject.CommonName != "mitt Root CA" {
		return fmt.Errorf("certificate CN is %q, want %q", cert.Subject.CommonName, "mitt Root CA")
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "mitt" {
		return fmt.Errorf("certificate O is %v, want [mitt]", cert.Subject.Organization)
	}
	if len(cert.Subject.Country) != 1 || cert.Subject.Country[0] != "--" {
		return fmt.Errorf("certificate C is %v, want [--]", cert.Subject.Country)
	}
	if !cert.BasicConstraintsValid || !cert.IsCA {
		return errors.New("certificate is not a CA")
	}
	if cert.MaxPathLen != 0 || !cert.MaxPathLenZero {
		return fmt.Errorf("certificate path length is %d, want 0", cert.MaxPathLen)
	}
	if cert.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		return fmt.Errorf("certificate key usage is %v, want CertSign|CRLSign", cert.KeyUsage)
	}
	publicKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return errors.New("certificate public key is not ECDSA P-256")
	}
	if !publicKey.Equal(&key.PublicKey) {
		return errors.New("certificate does not match the private key")
	}
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("certificate is not valid before %s", cert.NotBefore.Format(time.RFC3339))
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("certificate expired at %s", cert.NotAfter.Format(time.RFC3339))
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return fmt.Errorf("certificate is not self-signed: %v", err)
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, err
	}
	if serial.Sign() == 0 {
		return big.NewInt(1), nil
	}
	return serial, nil
}
