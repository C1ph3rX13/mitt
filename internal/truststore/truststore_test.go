//go:build linux

package truststore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

type fakeTarget struct {
	targetName  string
	installNote string
	installErr  error
	installedOK bool
	installs    int
	checks      int
}

func (f *fakeTarget) name() string { return f.targetName }

func (f *fakeTarget) install(cert certificate) (string, error) {
	f.installs++
	return f.installNote, f.installErr
}

func (f *fakeTarget) installed(cert certificate) bool {
	f.checks++
	return f.installedOK
}

func newTestCertificate(t *testing.T, commonName string) certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := parseCertificate(certPEM)
	if err != nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	return cert
}

func TestInstallReportsTargetsInOrderAndAggregatesNotes(t *testing.T) {
	system := &fakeTarget{targetName: "system"}
	nss := &fakeTarget{targetName: "nss", installNote: "nss updated"}

	result, err := install(newTestCertificate(t, "ca"), []entry{
		{target: system},
		{target: nss, optional: true},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := strings.Join(result.InstalledTo, ","); got != "system,nss" {
		t.Errorf("installed_to = %q, want system,nss", got)
	}
	if result.Note != "nss updated" {
		t.Errorf("note = %q, want nss updated", result.Note)
	}
}

func TestInstallRequiredFailureAbortsBeforeLaterTargets(t *testing.T) {
	system := &fakeTarget{targetName: "system", installErr: errors.New("boom")}
	nss := &fakeTarget{targetName: "nss"}

	result, err := install(newTestCertificate(t, "ca"), []entry{
		{target: system},
		{target: nss, optional: true},
	})

	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want boom", err)
	}
	if nss.installs != 0 {
		t.Errorf("optional target was attempted after a required failure: %d calls", nss.installs)
	}
	if len(result.InstalledTo) != 0 {
		t.Errorf("installed_to = %v, want empty", result.InstalledTo)
	}
}

func TestInstallOptionalFailureBecomesNote(t *testing.T) {
	system := &fakeTarget{targetName: "system"}
	nss := &fakeTarget{
		targetName:  "nss",
		installNote: "nss failed",
		installErr:  errors.New("exit 1"),
	}

	result, err := install(newTestCertificate(t, "ca"), []entry{
		{target: system},
		{target: nss, optional: true},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := strings.Join(result.InstalledTo, ","); got != "system" {
		t.Errorf("installed_to = %q, want system", got)
	}
	if result.Note != "nss failed" {
		t.Errorf("note = %q, want nss failed", result.Note)
	}
}

func TestInstallOptionalFailureWithoutNoteUsesError(t *testing.T) {
	system := &fakeTarget{targetName: "system"}
	nss := &fakeTarget{targetName: "nss", installErr: errors.New("nss exploded")}

	result, err := install(newTestCertificate(t, "ca"), []entry{
		{target: system},
		{target: nss, optional: true},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Note != "nss exploded" {
		t.Errorf("note = %q, want nss exploded", result.Note)
	}
	if got := strings.Join(result.InstalledTo, ","); got != "system" {
		t.Errorf("installed_to = %q, want system", got)
	}
}

func TestInstallJoinsMultipleNotes(t *testing.T) {
	system := &fakeTarget{targetName: "system"}
	nss := &fakeTarget{targetName: "nss", installNote: "nss note"}
	firefox := &fakeTarget{targetName: "firefox", installNote: "firefox note"}

	result, err := install(newTestCertificate(t, "ca"), []entry{
		{target: system},
		{target: nss, optional: true},
		{target: firefox, optional: true},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Note != "nss note\nfirefox note" {
		t.Errorf("note = %q, want nss note\\nfirefox note", result.Note)
	}
	if got := strings.Join(result.InstalledTo, ","); got != "system,nss,firefox" {
		t.Errorf("installed_to = %q, want system,nss,firefox", got)
	}
}

func TestInstalledReturnsMatchingTargetsInOrder(t *testing.T) {
	system := &fakeTarget{targetName: "system", installedOK: true}
	nss := &fakeTarget{targetName: "nss", installedOK: false}
	firefox := &fakeTarget{targetName: "firefox", installedOK: true}

	targets := installed(newTestCertificate(t, "ca"), []entry{
		{target: system},
		{target: nss, optional: true},
		{target: firefox, optional: true},
	})

	if got := strings.Join(targets, ","); got != "system,firefox" {
		t.Errorf("installed = %q, want system,firefox", got)
	}
}

func TestInstallRejectsInvalidPEM(t *testing.T) {
	if _, err := Install([]byte("not a pem")); err == nil {
		t.Fatal("Install accepted invalid PEM")
	}
	if got := Installed([]byte("not a pem")); len(got) != 0 {
		t.Errorf("Installed = %v, want empty", got)
	}
}

func TestDefaultEntriesOrderAndOptionality(t *testing.T) {
	entries := defaultEntries()
	if len(entries) != 2 {
		t.Fatalf("len(defaultEntries) = %d, want 2", len(entries))
	}
	if got := entries[0].target.name(); got != "system" {
		t.Errorf("first target = %q, want system", got)
	}
	if entries[0].optional {
		t.Error("system target must be required")
	}
	if got := entries[1].target.name(); got != "nss" {
		t.Errorf("second target = %q, want nss", got)
	}
	if !entries[1].optional {
		t.Error("nss target must be optional")
	}
}
