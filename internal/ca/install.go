//go:build linux

package ca

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
	"github.com/C1ph3rX13/mitt/internal/truststore"
)

// ErrInstall reports a failed trust-store installation. It maps to
// CERT_INSTALL_FAILED (78) at the command layer.
var ErrInstall = errors.New("CA installation failed")

// InstallResult is the trust-store installation outcome.
type InstallResult struct {
	InstalledTo []string
	OutputPath  string
	SHA256      string
	Note        string
}

// Fingerprint is the lowercase hex SHA-256 of the CA certificate PEM. It is
// the value reported by `ca install`; `ca status` compares the same digest for
// the system target.
func Fingerprint(certPEM []byte) string {
	sum := sha256.Sum256(certPEM)
	return hex.EncodeToString(sum[:])
}

// Install writes the CA into the environment trust stores. With outputPath set
// it only exports the PEM and touches nothing else. System installation is
// Debian/Kali only and fails as ErrInstall when the OS family cannot accept it
// or update-ca-certificates fails. NSS installation is best-effort and never
// fails the command; its outcome is reported through Note.
func Install(authority *Authority, outputPath string) (InstallResult, error) {
	result := InstallResult{
		InstalledTo: []string{},
		OutputPath:  outputPath,
		SHA256:      Fingerprint(authority.CertPEM),
	}

	if outputPath != "" {
		if err := atomicfile.Write(outputPath, authority.CertPEM, atomicfile.Shared); err != nil {
			return result, fmt.Errorf("%w: export CA PEM to %s: %v", ErrInstall, outputPath, err)
		}
		return result, nil
	}

	stored, err := truststore.Install(authority.CertPEM)
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrInstall, err)
	}
	result.InstalledTo = stored.InstalledTo
	result.Note = stored.Note
	return result, nil
}

// InstalledTargets reports the trust stores that currently contain this CA.
// The result uses the stable system/nss strings in a fixed order.
func InstalledTargets(authority *Authority) []string {
	return truststore.Installed(authority.CertPEM)
}
