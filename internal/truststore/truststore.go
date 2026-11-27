//go:build linux

// Package truststore installs the mitt root CA into the client trust stores
// that apply to the current machine and reports which ones hold it.
package truststore

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	defaultSystemCertPath    = "/usr/local/share/ca-certificates/mitt.crt"
	defaultNSSNickname       = "mitt"
	defaultOSReleasePath     = "/etc/os-release"
	defaultDebianVersionPath = "/etc/debian_version"
)

// Result is the outcome of an install.
type Result struct {
	InstalledTo []string
	Note        string
}

// Install writes certPEM into the applicable client trust stores in stable
// order. The system store is required: its failure aborts the install and is
// returned. NSS is best-effort: its failure is reported in Note and does not
// fail the call. InstalledTo is never nil.
func Install(certPEM []byte) (Result, error) {
	cert, err := parseCertificate(certPEM)
	if err != nil {
		return Result{}, err
	}
	return install(cert, defaultEntries())
}

// Installed reports the client trust stores that currently hold certPEM, in
// stable order. Detection failures count as not installed.
func Installed(certPEM []byte) []string {
	cert, err := parseCertificate(certPEM)
	if err != nil {
		return []string{}
	}
	return installed(cert, defaultEntries())
}

func install(cert certificate, entries []entry) (Result, error) {
	result := Result{InstalledTo: []string{}}
	notes := []string{}
	for _, e := range entries {
		note, err := e.target.install(cert)
		if err != nil {
			if !e.optional {
				return Result{}, err
			}
			if note == "" {
				note = err.Error()
			}
		} else {
			result.InstalledTo = append(result.InstalledTo, e.target.name())
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	result.Note = strings.Join(notes, "\n")
	return result, nil
}

func installed(cert certificate, entries []entry) []string {
	names := []string{}
	for _, e := range entries {
		if e.target.installed(cert) {
			names = append(names, e.target.name())
		}
	}
	return names
}

func parseCertificate(certPEM []byte) (certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return certificate{}, errors.New("certificate PEM does not contain a CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return certificate{}, fmt.Errorf("parse certificate: %w", err)
	}
	return certificate{pem: certPEM, der: cert.Raw}, nil
}

// defaultEntries is the production composition. Order is part of the command
// contract: system is reported before nss.
func defaultEntries() []entry {
	home, _ := os.UserHomeDir()
	dbDir := ""
	if home != "" {
		dbDir = filepath.Join(home, ".pki", "nssdb")
	}
	return []entry{
		{
			target: systemTarget{
				anchorPath:        defaultSystemCertPath,
				osReleasePath:     defaultOSReleasePath,
				debianVersionPath: defaultDebianVersionPath,
				runner:            execRunner{},
			},
		},
		{
			target: nssTarget{
				dbDir:    dbDir,
				nickname: defaultNSSNickname,
				runner:   execRunner{},
				lookPath: exec.LookPath,
			},
			optional: true,
		},
	}
}

func sameFingerprint(a, b []byte) bool {
	return sha256.Sum256(a) == sha256.Sum256(b)
}
