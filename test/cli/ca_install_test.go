package cli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const systemCAPath = "/usr/local/share/ca-certificates/mitt.crt"

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type fileSnapshot struct {
	exists bool
	hash   string
}

func snapshotFile(t *testing.T, path string) fileSnapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fileSnapshot{}
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fileSnapshot{exists: true, hash: sha256Hex(string(data))}
}

func TestCAInstallOutputExportsOnlyPEM(t *testing.T) {
	env := newTestEnv(t)
	requireExit(t, env.runWithStore(t, "ca", "init"), 0)
	caPEM := readFileString(t, filepath.Join(env.store, "ca", "ca.pem"))

	fakeBin := filepath.Join(t.TempDir(), "fakebin")
	updateSentinel := filepath.Join(t.TempDir(), "update-ran")
	certutilSentinel := filepath.Join(t.TempDir(), "certutil-ran")
	writeExecutable(t, filepath.Join(fakeBin, "update-ca-certificates"),
		"#!/bin/sh\ntouch \"$UPDATE_SENTINEL\"\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "certutil"),
		"#!/bin/sh\ntouch \"$CERTUTIL_SENTINEL\"\nexit 0\n")

	systemBefore := snapshotFile(t, systemCAPath)
	outputPath := filepath.Join(env.home, "exports", "nested", "mitt.pem")
	got := env.runWithEnv(t, []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"UPDATE_SENTINEL=" + updateSentinel,
		"CERTUTIL_SENTINEL=" + certutilSentinel,
	}, "--store", env.store, "ca", "install", "--output", outputPath)

	requireExit(t, got, 0)
	output := decodeJSONObject(t, got.stdout)
	targets, ok := output["installed_to"].([]any)
	if !ok || len(targets) != 0 {
		t.Errorf("installed_to = %#v, want []", output["installed_to"])
	}
	if output["output_path"] != outputPath {
		t.Errorf("output_path = %v, want %v", output["output_path"], outputPath)
	}
	if output["sha256"] != sha256Hex(caPEM) {
		t.Errorf("sha256 = %v, want %v", output["sha256"], sha256Hex(caPEM))
	}
	if _, ok := output["note"].(string); !ok {
		t.Errorf("note = %#v, want a string", output["note"])
	}

	if got := readFileString(t, outputPath); got != caPEM {
		t.Error("exported PEM differs from ca.pem")
	}
	assertMode(t, outputPath, 0o644)
	if _, err := os.Stat(updateSentinel); !os.IsNotExist(err) {
		t.Errorf("--output must not run update-ca-certificates, sentinel err = %v", err)
	}
	if _, err := os.Stat(certutilSentinel); !os.IsNotExist(err) {
		t.Errorf("--output must not run certutil, sentinel err = %v", err)
	}
	if after := snapshotFile(t, systemCAPath); after != systemBefore {
		t.Errorf("--output changed the system trust path: before %+v, after %+v", systemBefore, after)
	}
}

func TestCAInstallOutputFailureIsConfigError(t *testing.T) {
	env := newTestEnv(t)
	requireExit(t, env.runWithStore(t, "ca", "init"), 0)

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("file\n"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	got := env.runWithStore(t, "ca", "install", "--output", filepath.Join(blocker, "mitt.pem"))

	requireExit(t, got, 78)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "CERT_INSTALL_FAILED" {
		t.Errorf("error.code = %v, want CERT_INSTALL_FAILED", errObj["code"])
	}
	hint, _ := errObj["hint"].(string)
	requireContains(t, hint, "--output")
	details, _ := errObj["details"].(map[string]any)
	if details["ca_path"] != filepath.Join(env.store, "ca") {
		t.Errorf("details.ca_path = %v, want %v", details["ca_path"], filepath.Join(env.store, "ca"))
	}
	if details["output_path"] == "" {
		t.Errorf("details.output_path must identify the failed export path: %v", details)
	}
}

func TestCAInstallWithoutCAIsConfigError(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "ca", "install", "--output", filepath.Join(env.home, "mitt.pem"))

	requireExit(t, got, 78)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "CA_MISSING" {
		t.Errorf("error.code = %v, want CA_MISSING", errObj["code"])
	}
	hint, _ := errObj["hint"].(string)
	requireContains(t, hint, "mitt ca init")
	if _, err := os.Stat(filepath.Join(env.home, "mitt.pem")); !os.IsNotExist(err) {
		t.Errorf("install without a CA must not write --output, stat err = %v", err)
	}
}

func TestCAStatusDetectsNSSViaCertutil(t *testing.T) {
	env := newTestEnv(t)
	requireExit(t, env.runWithStore(t, "ca", "init"), 0)

	fakeBin := filepath.Join(t.TempDir(), "fakebin")
	logPath := filepath.Join(t.TempDir(), "certutil.log")
	writeExecutable(t, filepath.Join(fakeBin, "certutil"), `#!/bin/sh
printf '%s\n' "$*" >> "$CERTUTIL_LOG"
if [ "${CERTUTIL_EXIT:-0}" != "0" ]; then
  exit "$CERTUTIL_EXIT"
fi
case " $* " in
  *" -a "*) cat "$CERTUTIL_PEM" ;;
esac
exit 0
`)

	extra := []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"CERTUTIL_LOG=" + logPath,
		"CERTUTIL_EXIT=0",
		"CERTUTIL_PEM=" + filepath.Join(env.store, "ca", "ca.pem"),
	}
	got := env.runWithEnv(t, extra,
		"--store", env.store, "ca", "status")
	requireExit(t, got, 0)
	status := decodeJSONObject(t, got.stdout)
	if status["installed"] != true {
		t.Errorf("installed = %v, want true", status["installed"])
	}
	assertTargets(t, status["install_targets"], []string{"nss"})

	log := readFileString(t, logPath)
	wantQuery := fmt.Sprintf("-L -d sql:%s -n mitt -a", filepath.Join(env.home, ".pki", "nssdb"))
	requireContains(t, log, wantQuery)

	otherStore := filepath.Join(t.TempDir(), "other-store")
	requireExit(t, env.runWithEnv(t, nil, "--store", otherStore, "ca", "init"), 0)
	mismatch := []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"CERTUTIL_LOG=" + logPath,
		"CERTUTIL_EXIT=0",
		"CERTUTIL_PEM=" + filepath.Join(otherStore, "ca", "ca.pem"),
	}
	got = env.runWithEnv(t, mismatch,
		"--store", env.store, "ca", "status")
	requireExit(t, got, 0)
	status = decodeJSONObject(t, got.stdout)
	if status["installed"] != false {
		t.Errorf("installed = %v, want false when the NSS nickname holds a different CA", status["installed"])
	}
	assertTargets(t, status["install_targets"], []string{})

	extra = []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"CERTUTIL_LOG=" + logPath,
		"CERTUTIL_EXIT=1",
	}
	got = env.runWithEnv(t, extra,
		"--store", env.store, "ca", "status")
	requireExit(t, got, 0)
	status = decodeJSONObject(t, got.stdout)
	if status["installed"] != false {
		t.Errorf("installed = %v, want false when certutil cannot find the nickname", status["installed"])
	}
	assertTargets(t, status["install_targets"], []string{})
}

func assertTargets(t *testing.T, value any, want []string) {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("install_targets = %#v, want []string", value)
	}
	got := make([]string, 0, len(raw))
	for _, item := range raw {
		name, _ := item.(string)
		got = append(got, name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("install_targets = %v, want %v", got, want)
	}
}

func TestSpecRegistersCAInstall(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	installCmd, ok := doc.Command.find("mitt ca install")
	if !ok {
		t.Fatal("spec missing mitt ca install")
	}
	outputFlag, ok := flagByName(installCmd.Flags, "output")
	if !ok {
		t.Fatal("ca install flags missing --output")
	}
	if outputFlag.Type != "string" {
		t.Errorf("--output type = %q, want string", outputFlag.Type)
	}
	for _, code := range []string{"CA_MISSING", "CERT_INSTALL_FAILED", "INTERNAL_ERROR"} {
		if !containsString(installCmd.ErrorCodes, code) {
			t.Errorf("ca install error_codes = %v, want %s", installCmd.ErrorCodes, code)
		}
	}
	requireSchemaFields(t, installCmd.OutputSchema, "installed_to", "output_path", "sha256", "note")
	if got := installCmd.OutputSchema["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("ca install output schema $schema = %v", got)
	}
}
