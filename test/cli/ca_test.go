package cli_test

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const rootCASubject = "CN=mitt Root CA, O=mitt, C=--"

func TestCAInitGeneratesRootCA(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "ca", "init")

	requireExit(t, got, 0)
	output := decodeJSONObject(t, got.stdout)
	for _, key := range []string{"ca_pem", "ca_key_pem", "subject", "not_after"} {
		if _, ok := output[key]; !ok {
			t.Errorf("ca init output missing %q: %v", key, output)
		}
	}
	if output["subject"] != rootCASubject {
		t.Errorf("subject = %v, want %q", output["subject"], rootCASubject)
	}

	caPEM, _ := output["ca_pem"].(string)
	cert := parseCertificatePEM(t, caPEM)
	if cert.Subject.CommonName != "mitt Root CA" {
		t.Errorf("certificate CN = %q, want %q", cert.Subject.CommonName, "mitt Root CA")
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "mitt" {
		t.Errorf("certificate O = %v, want [mitt]", cert.Subject.Organization)
	}
	if len(cert.Subject.Country) != 1 || cert.Subject.Country[0] != "--" {
		t.Errorf("certificate C = %v, want [--]", cert.Subject.Country)
	}
	if !cert.IsCA {
		t.Error("certificate must have CA:TRUE")
	}
	if cert.MaxPathLen != 0 || !cert.MaxPathLenZero {
		t.Errorf("certificate pathlen = %d (zero=%v), want 0", cert.MaxPathLen, cert.MaxPathLenZero)
	}
	wantUsage := x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	if cert.KeyUsage != wantUsage {
		t.Errorf("key usage = %v, want CertSign|CRLSign", cert.KeyUsage)
	}
	publicKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key type = %T, want *ecdsa.PublicKey", cert.PublicKey)
	}
	if publicKey.Curve.Params().Name != "P-256" {
		t.Errorf("curve = %q, want P-256", publicKey.Curve.Params().Name)
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Errorf("certificate is not self-signed: %v", err)
	}

	notAfter := cert.NotAfter
	expectedNotAfter := time.Now().AddDate(10, 0, 0)
	if delta := notAfter.Sub(expectedNotAfter); delta > 48*time.Hour || delta < -48*time.Hour {
		t.Errorf("not_after = %v, want about 10 years from now (%v)", notAfter, expectedNotAfter)
	}
	reportedNotAfter, err := time.Parse(time.RFC3339, output["not_after"].(string))
	if err != nil {
		t.Fatalf("not_after is not RFC3339: %v", err)
	}
	if !reportedNotAfter.Equal(notAfter) {
		t.Errorf("not_after = %v, want certificate NotAfter %v", reportedNotAfter, notAfter)
	}

	caKeyPEM, _ := output["ca_key_pem"].(string)
	keyBlock, _ := pem.Decode([]byte(caKeyPEM))
	if keyBlock == nil {
		t.Fatalf("ca_key_pem is not PEM:\n%s", caKeyPEM)
	}
	privateKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse ca_key_pem: %v", err)
	}
	if !privateKey.PublicKey.Equal(publicKey) {
		t.Error("ca_key_pem does not match ca_pem")
	}

	caDir := filepath.Join(env.store, "ca")
	assertMode(t, caDir, 0o700)
	certPath := filepath.Join(caDir, "ca.pem")
	keyPath := filepath.Join(caDir, "ca.key.pem")
	assertMode(t, keyPath, 0o600)
	if got := readFileString(t, certPath); got != caPEM {
		t.Error("ca.pem on disk does not match ca_pem in output")
	}
	if got := readFileString(t, keyPath); got != caKeyPEM {
		t.Error("ca.key.pem on disk does not match ca_key_pem in output")
	}
}

func parseCertificatePEM(t *testing.T, data string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		t.Fatalf("not a PEM certificate:\n%s", data)
	}
	if block.Type != "CERTIFICATE" {
		t.Fatalf("PEM block type = %q, want CERTIFICATE", block.Type)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func requireContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("output missing %q:\n%s", want, got)
	}
}

func TestCAInitReusesExistingCA(t *testing.T) {
	env := newTestEnv(t)

	first := env.runWithStore(t, "ca", "init")
	requireExit(t, first, 0)
	firstOutput := decodeJSONObject(t, first.stdout)

	certPath := filepath.Join(env.store, "ca", "ca.pem")
	keyPath := filepath.Join(env.store, "ca", "ca.key.pem")
	certBefore := readFileString(t, certPath)
	keyBefore := readFileString(t, keyPath)

	second := env.runWithStore(t, "ca", "init")
	requireExit(t, second, 0)
	secondOutput := decodeJSONObject(t, second.stdout)

	for _, key := range []string{"ca_pem", "ca_key_pem", "subject", "not_after"} {
		if firstOutput[key] != secondOutput[key] {
			t.Errorf("second ca init changed %q; existing CA must be reused", key)
		}
	}
	if got := readFileString(t, certPath); got != certBefore {
		t.Error("second ca init rewrote ca.pem")
	}
	if got := readFileString(t, keyPath); got != keyBefore {
		t.Error("second ca init rewrote ca.key.pem")
	}
}

func TestCAStatusReportsExistingCA(t *testing.T) {
	env := newTestEnv(t)
	requireExit(t, env.runWithStore(t, "ca", "init"), 0)

	got := env.runWithStore(t, "ca", "status")

	requireExit(t, got, 0)
	status := decodeJSONObject(t, got.stdout)
	if status["ca_exists"] != true {
		t.Errorf("ca_exists = %v, want true", status["ca_exists"])
	}
	if status["ca_path"] != filepath.Join(env.store, "ca") {
		t.Errorf("ca_path = %v, want %v", status["ca_path"], filepath.Join(env.store, "ca"))
	}
	if status["installed"] != false {
		t.Errorf("installed = %v, want false", status["installed"])
	}
	targets, ok := status["install_targets"].([]any)
	if !ok || len(targets) != 0 {
		t.Errorf("install_targets = %#v, want []", status["install_targets"])
	}
}

func TestCAStatusMissingCAIsConfigError(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "ca", "status")

	requireExit(t, got, 78)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "CA_MISSING" {
		t.Errorf("error.code = %v, want CA_MISSING", errObj["code"])
	}
	hint, _ := errObj["hint"].(string)
	requireContains(t, hint, "mitt ca init")
	requireContains(t, hint, "--dir")
	requireContains(t, hint, "back up")

	details, _ := errObj["details"].(map[string]any)
	if details["ca_path"] != filepath.Join(env.store, "ca") {
		t.Errorf("details.ca_path = %v, want %v", details["ca_path"], filepath.Join(env.store, "ca"))
	}
	if details["reason"] != "missing" {
		t.Errorf("details.reason = %v, want missing", details["reason"])
	}
	if _, err := os.Stat(filepath.Join(env.store, "ca")); !os.IsNotExist(err) {
		t.Errorf("status must not create the CA directory, stat err = %v", err)
	}
	if _, err := os.Stat(env.store); !os.IsNotExist(err) {
		t.Errorf("status must not create the store, stat err = %v", err)
	}
}

func TestCACorruptOrIncompleteIsConfigError(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, caDir string)
	}{
		{
			name: "corrupt files",
			setup: func(t *testing.T, caDir string) {
				writeFile(t, filepath.Join(caDir, "ca.pem"), "not a certificate\n")
				writeFile(t, filepath.Join(caDir, "ca.key.pem"), "not a key\n")
			},
		},
		{
			name: "missing private key",
			setup: func(t *testing.T, caDir string) {
				env := newTestEnv(t)
				requireExit(t, env.runWithStore(t, "ca", "init"), 0)
				cert, err := os.ReadFile(filepath.Join(env.store, "ca", "ca.pem"))
				if err != nil {
					t.Fatalf("read generated CA: %v", err)
				}
				writeFile(t, filepath.Join(caDir, "ca.pem"), string(cert))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			caDir := filepath.Join(env.store, "ca")
			tc.setup(t, caDir)
			certBefore := readFileString(t, filepath.Join(caDir, "ca.pem"))

			for _, args := range [][]string{{"ca", "status"}, {"ca", "init"}} {
				got := env.runWithStore(t, args...)
				requireExit(t, got, 78)
				errObj := errorEnvelope(t, got)
				if errObj["code"] != "CA_MISSING" {
					t.Errorf("%v error.code = %v, want CA_MISSING", args, errObj["code"])
				}
				hint, _ := errObj["hint"].(string)
				requireContains(t, hint, "back up")
				details, _ := errObj["details"].(map[string]any)
				if details["reason"] != "invalid" {
					t.Errorf("%v details.reason = %v, want invalid", args, details["reason"])
				}
			}

			if got := readFileString(t, filepath.Join(caDir, "ca.pem")); got != certBefore {
				t.Error("ca init must not overwrite a damaged CA")
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create parent of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCAUsesStoreCAByDefault(t *testing.T) {
	env := newTestEnv(t)

	got := env.run(t, "ca", "init")
	requireExit(t, got, 0)

	defaultCA := filepath.Join(env.home, ".mitt", "ca")
	if _, err := os.Stat(filepath.Join(defaultCA, "ca.pem")); err != nil {
		t.Fatalf("ca init must create ~/.mitt/ca/ca.pem: %v", err)
	}

	status := env.run(t, "ca", "status")
	requireExit(t, status, 0)
	output := decodeJSONObject(t, status.stdout)
	if output["ca_path"] != defaultCA {
		t.Errorf("ca_path = %v, want %v", output["ca_path"], defaultCA)
	}
	if _, err := os.Stat(env.store); !os.IsNotExist(err) {
		t.Errorf("default CA must not touch the override store, stat err = %v", err)
	}
}

func TestCADirFlagOverridesStore(t *testing.T) {
	env := newTestEnv(t)
	customDir := filepath.Join(env.home, "custom-ca")

	got := env.runWithStore(t, "ca", "init", "--dir", customDir)
	requireExit(t, got, 0)
	if _, err := os.Stat(filepath.Join(customDir, "ca.pem")); err != nil {
		t.Fatalf("--dir must place ca.pem in %s: %v", customDir, err)
	}
	if _, err := os.Stat(filepath.Join(env.store, "ca")); !os.IsNotExist(err) {
		t.Errorf("--dir must not write <store>/ca, stat err = %v", err)
	}

	status := env.runWithStore(t, "ca", "status", "--dir", customDir)
	requireExit(t, status, 0)
	output := decodeJSONObject(t, status.stdout)
	if output["ca_path"] != customDir {
		t.Errorf("ca_path = %v, want %v", output["ca_path"], customDir)
	}
}

func TestStoreOverrideNeverReadsRealHomeCA(t *testing.T) {
	env := newTestEnv(t)
	homeCA := filepath.Join(env.home, ".mitt", "ca")
	writeFile(t, filepath.Join(homeCA, "ca.pem"), "damaged home CA\n")
	writeFile(t, filepath.Join(homeCA, "ca.key.pem"), "damaged home key\n")

	got := env.runWithStore(t, "ca", "init")
	requireExit(t, got, 0)
	output := decodeJSONObject(t, got.stdout)
	parseCertificatePEM(t, output["ca_pem"].(string))
	if _, err := os.Stat(filepath.Join(env.store, "ca", "ca.pem")); err != nil {
		t.Fatalf("store override must create <store>/ca/ca.pem: %v", err)
	}
	if got := readFileString(t, filepath.Join(homeCA, "ca.pem")); got != "damaged home CA\n" {
		t.Error("store override must not modify or replace the real ~/.mitt/ca")
	}

	status := env.runWithStore(t, "ca", "status")
	requireExit(t, status, 0)

	homeStatus := env.run(t, "ca", "status")
	requireExit(t, homeStatus, 78)
}

func TestSpecRegistersCACommands(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	for _, path := range []string{"mitt ca", "mitt ca init", "mitt ca status"} {
		if _, ok := doc.Command.find(path); !ok {
			t.Errorf("spec command tree missing %q", path)
		}
	}

	initCmd, ok := doc.Command.find("mitt ca init")
	if !ok {
		t.Fatal("spec missing mitt ca init")
	}
	statusCmd, ok := doc.Command.find("mitt ca status")
	if !ok {
		t.Fatal("spec missing mitt ca status")
	}

	for _, command := range []specCommand{initCmd, statusCmd} {
		dirFlag, ok := flagByName(command.Flags, "dir")
		if !ok {
			t.Errorf("%s flags missing --dir", command.Path)
			continue
		}
		if dirFlag.Type != "string" {
			t.Errorf("%s --dir type = %q, want string", command.Path, dirFlag.Type)
		}
		if !containsString(command.ErrorCodes, "CA_MISSING") {
			t.Errorf("%s error_codes = %v, want CA_MISSING", command.Path, command.ErrorCodes)
		}
		if !containsString(command.ErrorCodes, "INTERNAL_ERROR") {
			t.Errorf("%s error_codes = %v, want INTERNAL_ERROR", command.Path, command.ErrorCodes)
		}
	}

	requireSchemaFields(t, initCmd.OutputSchema, "ca_pem", "ca_key_pem", "subject", "not_after")
	requireSchemaFields(t, statusCmd.OutputSchema, "ca_exists", "ca_path", "installed", "install_targets")
	if got := initCmd.OutputSchema["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("ca init output schema $schema = %v", got)
	}
	if got := statusCmd.OutputSchema["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("ca status output schema $schema = %v", got)
	}
}

func requireSchemaFields(t *testing.T, schema map[string]any, fields ...string) {
	t.Helper()
	if schema == nil {
		t.Fatal("output schema is missing")
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("output schema required is %T, want array", schema["required"])
	}
	present := map[string]bool{}
	for _, field := range required {
		name, _ := field.(string)
		present[name] = true
	}
	for _, field := range fields {
		if !present[field] {
			t.Errorf("output schema required missing %q: %v", field, required)
		}
	}
}
