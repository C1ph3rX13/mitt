//go:build linux

package truststore

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// nssTarget is one NSS certificate database: the Chromium database by default,
// or a Firefox profile database when wired to one.
type nssTarget struct {
	dbDir    string
	nickname string
	runner   commandRunner
	lookPath func(string) (string, error)
}

func (n nssTarget) name() string { return "nss" }

func (n nssTarget) install(cert certificate) (string, error) {
	certutil, err := n.lookPath("certutil")
	if err != nil {
		return failed("certutil not found; NSS trust store not updated")
	}
	if n.dbDir == "" {
		return failed("cannot resolve the home directory; NSS trust store not updated")
	}
	if err := os.MkdirAll(n.dbDir, 0o700); err != nil {
		return failed(fmt.Sprintf("NSS trust store not updated: %v", err))
	}
	if _, err := os.Stat(filepath.Join(n.dbDir, "cert9.db")); errors.Is(err, os.ErrNotExist) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := n.runner.run(ctx, certutil, "-N", "--empty-password", "-d", "sql:"+n.dbDir)
		cancel()
		if err != nil {
			return failed(fmt.Sprintf("NSS trust store not updated: initialize %s: %v%s", n.dbDir, err, outputSuffix(out)))
		}
	} else if err != nil {
		return failed(fmt.Sprintf("NSS trust store not updated: inspect %s: %v", n.dbDir, err))
	}

	certFile, cleanup, err := tempPEM(cert.pem)
	if err != nil {
		return failed(fmt.Sprintf("NSS trust store not updated: %v", err))
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, err := n.runner.run(
		ctx, certutil,
		"-A", "-d", "sql:"+n.dbDir, "-n", n.nickname, "-t", "C,,", "-i", certFile,
	)
	cancel()
	if err != nil {
		return failed(fmt.Sprintf("NSS trust store not updated: import into %s: %v%s", n.dbDir, err, outputSuffix(out)))
	}
	return fmt.Sprintf("NSS trust store updated (nickname %s)", n.nickname), nil
}

// installed checks the certificate behind the fixed nickname, not only the
// nickname itself. A different CA installed under the same nickname must not
// make the current CA look installed.
func (n nssTarget) installed(cert certificate) bool {
	certutil, err := n.lookPath("certutil")
	if err != nil {
		return false
	}
	if n.dbDir == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := n.runner.run(ctx, certutil, "-L", "-d", "sql:"+n.dbDir, "-n", n.nickname, "-a")
	if err != nil {
		return false
	}
	block, _ := pem.Decode(out)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	got, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	return bytes.Equal(got.Raw, cert.der)
}

func failed(note string) (string, error) {
	return note, errors.New(note)
}

func tempPEM(certPEM []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "mitt-ca-*.pem")
	if err != nil {
		return "", func() {}, err
	}
	name := file.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, err
	}
	if _, err := file.Write(certPEM); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return name, cleanup, nil
}
