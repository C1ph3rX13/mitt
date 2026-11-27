package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// The two ways reading <store>/proxy.json can fail. Whether the record
// describes a live proxy is proxy.RunState's judgment; the store only reads
// and decodes bytes.
var (
	ErrProxyStateMissing = errors.New("proxy state file does not exist")
	ErrProxyStateInvalid = errors.New("proxy state file is invalid")
)

// ProxyListener is one bound capture listener. Store is the data directory
// that receives this listener's events.
type ProxyListener struct {
	Addr  string `json:"addr"`
	Store string `json:"store"`
}

// ProxyState is the runtime state persisted in <store>/proxy.json.
type ProxyState struct {
	PID                 int             `json:"pid"`
	Addr                string          `json:"addr"`
	StartedAt           string          `json:"started_at"`
	CAFingerprintSHA256 string          `json:"ca_fingerprint_sha256"`
	Listeners           []ProxyListener `json:"listeners"`
}

// ReadProxyState reads and decodes <store>/proxy.json. It reports only the two
// ways the file itself can fail: missing and unparseable. A record that decodes
// but names a dead or unrelated process is still a successful read.
func (s *Store) ReadProxyState() (*ProxyState, error) {
	data, err := os.ReadFile(s.ProxyStatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrProxyStateMissing
	}
	if err != nil {
		return nil, fmt.Errorf("read proxy state: %w", err)
	}

	var state ProxyState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProxyStateInvalid, err)
	}
	if state.PID <= 0 {
		return nil, ErrProxyStateInvalid
	}
	if state.Listeners == nil {
		state.Listeners = []ProxyListener{}
	}
	return &state, nil
}

// WriteProxyState atomically creates or replaces <store>/proxy.json.
func (s *Store) WriteProxyState(state *ProxyState) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	if state.Listeners == nil {
		state.Listeners = []ProxyListener{}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode proxy state: %w", err)
	}
	data = append(data, '\n')
	if err := atomicfile.Write(s.ProxyStatePath(), data, atomicfile.Private); err != nil {
		return fmt.Errorf("write proxy state: %w", err)
	}
	return nil
}

// RemoveProxyState deletes <store>/proxy.json. A missing file is not an error.
func (s *Store) RemoveProxyState() error {
	err := os.Remove(s.ProxyStatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
