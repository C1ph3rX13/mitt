//go:build linux

package truststore

import (
	"context"
	"os/exec"
	"strings"
)

// commandRunner executes an external trust-store command. It returns the
// combined stdout and stderr so adapter notes can include the tail of a failed
// command.
type commandRunner interface {
	run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// certificate is the parsed form shared by every adapter: pem is the exact
// bytes written to a store, der is the identity compared against a store's
// copy.
type certificate struct {
	pem []byte
	der []byte
}

// target is one client trust store that can install and report the CA.
type target interface {
	name() string
	install(cert certificate) (note string, err error)
	installed(cert certificate) bool
}

// entry marks whether a target is required. Required targets abort the
// install; optional targets only contribute a note.
type entry struct {
	target   target
	optional bool
}

func outputSuffix(out []byte) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return ""
	}
	const limit = 800
	if len(text) > limit {
		text = "…" + text[len(text)-limit:]
	}
	return ": " + text
}
