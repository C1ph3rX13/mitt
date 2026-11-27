//go:build linux

package truststore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func okLookPath(string) (string, error) {
	return "/usr/bin/certutil", nil
}

func TestNSSTargetInstallInitializesAndImports(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "nssdb")
	cert := newTestCertificate(t, "ca")
	importPath := ""
	runner := &funcRunner{handle: func(name string, args []string) ([]byte, error) {
		switch {
		case hasArg(args, "-N"):
			return nil, nil
		case hasArg(args, "-A"):
			importPath = argValue(args, "-i")
			data, err := os.ReadFile(importPath)
			if err != nil {
				t.Errorf("read imported PEM: %v", err)
			} else if !bytes.Equal(data, cert.pem) {
				t.Error("imported PEM differs from the CA PEM")
			}
			return nil, nil
		default:
			return nil, nil
		}
	}}
	target := nssTarget{
		dbDir:    dbDir,
		nickname: "mitt",
		runner:   runner,
		lookPath: okLookPath,
	}

	note, err := target.install(cert)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(note, "NSS trust store updated (nickname mitt)") {
		t.Errorf("note = %q, want the NSS success note", note)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls = %+v, want init and import", runner.calls)
	}
	wantInit := []string{"-N", "--empty-password", "-d", "sql:" + dbDir}
	if got := strings.Join(runner.calls[0].args, " "); got != strings.Join(wantInit, " ") {
		t.Errorf("init args = %q, want %q", got, strings.Join(wantInit, " "))
	}
	wantImportPrefix := []string{"-A", "-d", "sql:" + dbDir, "-n", "mitt", "-t", "C,,", "-i"}
	if got := strings.Join(runner.calls[1].args[:len(wantImportPrefix)], " "); got != strings.Join(wantImportPrefix, " ") {
		t.Errorf("import args = %q, want prefix %q", got, strings.Join(wantImportPrefix, " "))
	}
	if filepath.Base(importPath) == "" || !strings.HasPrefix(filepath.Base(importPath), "mitt-ca-") {
		t.Errorf("import path = %q, want a mitt-ca-* temp file", importPath)
	}
	if _, err := os.Stat(importPath); !os.IsNotExist(err) {
		t.Errorf("temp PEM was not removed: stat err = %v", err)
	}
	info, err := os.Stat(dbDir)
	if err != nil {
		t.Fatalf("stat db dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("db dir mode = %o, want 700", got)
	}
}

func TestNSSTargetInstallUsesExistingDB(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "nssdb")
	writeTestFile(t, filepath.Join(dbDir, "cert9.db"), "")
	runner := &funcRunner{}
	target := nssTarget{
		dbDir:    dbDir,
		nickname: "mitt",
		runner:   runner,
		lookPath: okLookPath,
	}

	if _, err := target.install(newTestCertificate(t, "ca")); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %+v, want only the import", runner.calls)
	}
	if hasArg(runner.calls[0].args, "-N") {
		t.Error("existing NSS database was re-initialized")
	}
	if !hasArg(runner.calls[0].args, "-A") {
		t.Errorf("first call = %+v, want an import", runner.calls[0])
	}
}

func TestNSSTargetInstallWithoutCertutil(t *testing.T) {
	runner := &funcRunner{}
	target := nssTarget{
		dbDir:    "/tmp/does-not-matter",
		nickname: "mitt",
		runner:   runner,
		lookPath: func(string) (string, error) { return "", errors.New("missing") },
	}

	note, err := target.install(newTestCertificate(t, "ca"))
	if err == nil {
		t.Fatal("install succeeded without certutil")
	}
	if !strings.Contains(note, "certutil not found") {
		t.Errorf("note = %q, want certutil not found", note)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner calls = %+v, want none", runner.calls)
	}
}

func TestNSSTargetInstallWithoutHome(t *testing.T) {
	runner := &funcRunner{}
	target := nssTarget{
		dbDir:    "",
		nickname: "mitt",
		runner:   runner,
		lookPath: okLookPath,
	}

	note, err := target.install(newTestCertificate(t, "ca"))
	if err == nil {
		t.Fatal("install succeeded without a home directory")
	}
	if !strings.Contains(note, "cannot resolve the home directory") {
		t.Errorf("note = %q, want the home directory note", note)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner calls = %+v, want none", runner.calls)
	}
}

func TestNSSTargetInstallReportsInitFailure(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "nssdb")
	runner := &funcRunner{handle: func(name string, args []string) ([]byte, error) {
		return []byte("init exploded"), errors.New("exit status 1")
	}}
	target := nssTarget{
		dbDir:    dbDir,
		nickname: "mitt",
		runner:   runner,
		lookPath: okLookPath,
	}

	note, err := target.install(newTestCertificate(t, "ca"))
	if err == nil {
		t.Fatal("install succeeded after a failed init")
	}
	if !strings.Contains(note, "initialize "+dbDir) || !strings.Contains(note, "init exploded") {
		t.Errorf("note = %q, want initialize context and command output", note)
	}
	if len(runner.calls) != 1 || !hasArg(runner.calls[0].args, "-N") {
		t.Errorf("runner calls = %+v, want only the init", runner.calls)
	}
}

func TestNSSTargetInstallReportsImportFailure(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "nssdb")
	runner := &funcRunner{handle: func(name string, args []string) ([]byte, error) {
		if hasArg(args, "-A") {
			return []byte("import exploded"), errors.New("exit status 1")
		}
		return nil, nil
	}}
	target := nssTarget{
		dbDir:    dbDir,
		nickname: "mitt",
		runner:   runner,
		lookPath: okLookPath,
	}

	note, err := target.install(newTestCertificate(t, "ca"))
	if err == nil {
		t.Fatal("install succeeded after a failed import")
	}
	if !strings.Contains(note, "import into "+dbDir) || !strings.Contains(note, "import exploded") {
		t.Errorf("note = %q, want import context and command output", note)
	}
	if len(runner.calls) != 2 {
		t.Errorf("runner calls = %+v, want init and import", runner.calls)
	}
}

func TestNSSTargetInstalledComparesCertificate(t *testing.T) {
	cert := newTestCertificate(t, "ca")
	other := newTestCertificate(t, "other")

	runner := &funcRunner{handle: func(name string, args []string) ([]byte, error) {
		return cert.pem, nil
	}}
	target := nssTarget{
		dbDir:    "/tmp/nssdb",
		nickname: "mitt",
		runner:   runner,
		lookPath: okLookPath,
	}

	if !target.installed(cert) {
		t.Error("matching certificate reported as not installed")
	}
	want := []string{"-L", "-d", "sql:/tmp/nssdb", "-n", "mitt", "-a"}
	if got := strings.Join(runner.calls[0].args, " "); got != strings.Join(want, " ") {
		t.Errorf("query args = %q, want %q", got, strings.Join(want, " "))
	}

	runner = &funcRunner{handle: func(name string, args []string) ([]byte, error) {
		return other.pem, nil
	}}
	target.runner = runner
	if target.installed(cert) {
		t.Error("different certificate reported as installed")
	}
}

func TestNSSTargetInstalledTreatsFailuresAsNotInstalled(t *testing.T) {
	cert := newTestCertificate(t, "ca")

	tests := []struct {
		name     string
		dbDir    string
		lookPath func(string) (string, error)
		output   []byte
		err      error
	}{
		{
			name:     "no certutil",
			dbDir:    "/tmp/nssdb",
			lookPath: func(string) (string, error) { return "", errors.New("missing") },
		},
		{
			name:     "no db dir",
			dbDir:    "",
			lookPath: okLookPath,
		},
		{
			name:     "query failure",
			dbDir:    "/tmp/nssdb",
			lookPath: okLookPath,
			err:      errors.New("exit status 1"),
		},
		{
			name:     "unparseable output",
			dbDir:    "/tmp/nssdb",
			lookPath: okLookPath,
			output:   []byte("not a certificate"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &funcRunner{handle: func(name string, args []string) ([]byte, error) {
				return tc.output, tc.err
			}}
			target := nssTarget{
				dbDir:    tc.dbDir,
				nickname: "mitt",
				runner:   runner,
				lookPath: tc.lookPath,
			}
			if target.installed(cert) {
				t.Error("installed = true, want false")
			}
		})
	}
}
