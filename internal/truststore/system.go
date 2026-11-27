//go:build linux

package truststore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// systemTarget is the Debian/Kali anchor consumed by update-ca-certificates.
type systemTarget struct {
	anchorPath        string
	osReleasePath     string
	debianVersionPath string
	runner            commandRunner
}

func (s systemTarget) name() string { return "system" }

func (s systemTarget) install(cert certificate) (string, error) {
	if !isDebianFamily(s.osReleasePath, s.debianVersionPath) {
		return "", errors.New(
			"this system is not Debian/Kali based; use `mitt ca install --output <path>` to export the PEM",
		)
	}
	if err := atomicfile.Write(s.anchorPath, cert.pem, atomicfile.Shared); err != nil {
		return "", fmt.Errorf("write %s: %v", s.anchorPath, err)
	}
	out, err := s.runner.run(context.Background(), "update-ca-certificates")
	if err != nil {
		return "", fmt.Errorf("update-ca-certificates failed: %v%s", err, outputSuffix(out))
	}
	return "", nil
}

func (s systemTarget) installed(cert certificate) bool {
	data, err := os.ReadFile(s.anchorPath)
	if err != nil {
		return false
	}
	return sameFingerprint(data, cert.pem)
}

// isDebianFamily reports whether the running system can consume
// /usr/local/share/ca-certificates through update-ca-certificates. When
// os-release is readable its verdict is final: a Debian base that relabels
// itself as something else (for example a derivative image) must not sneak in
// through /etc/debian_version.
func isDebianFamily(osReleasePath, debianVersionPath string) bool {
	data, err := os.ReadFile(osReleasePath)
	if err == nil {
		values := parseOSRelease(string(data))
		switch values["ID"] {
		case "debian", "kali":
			return true
		}
		for _, id := range strings.Fields(values["ID_LIKE"]) {
			if id == "debian" || id == "kali" {
				return true
			}
		}
		return false
	}
	if _, err := os.Stat(debianVersionPath); err == nil {
		return true
	}
	return false
}

func parseOSRelease(data string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		values[strings.TrimSpace(key)] = value
	}
	return values
}
