package session

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteProxyStateIsAtomicAndPrivate(t *testing.T) {
	store := newRecorderStore(t)
	state := &ProxyState{
		PID:       4242,
		Addr:      "127.0.0.1:8080",
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Listeners: []ProxyListener{{Addr: "127.0.0.1:8080", Store: store.Dir()}},
	}

	if err := store.WriteProxyState(state); err != nil {
		t.Fatalf("WriteProxyState: %v", err)
	}

	info, err := os.Stat(store.ProxyStatePath())
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("state mode = %o, want 600", got)
	}
	dirInfo, err := os.Stat(store.Dir())
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("store mode = %o, want 700", got)
	}

	// The state read back is the state written, and no protocol temporary
	// survived the replace.
	read, err := store.ReadProxyState()
	if err != nil {
		t.Fatalf("ReadProxyState: %v", err)
	}
	if read.PID != state.PID || read.Addr != state.Addr {
		t.Errorf("read state = %+v, want %+v", read, state)
	}
	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "proxy.json" {
			t.Errorf("WriteProxyState left %q behind", entry.Name())
		}
	}
}

func TestWriteProxyStateReplacesAnExistingRecord(t *testing.T) {
	store := newRecorderStore(t)
	first := &ProxyState{PID: 1, Addr: "127.0.0.1:1"}
	second := &ProxyState{PID: 2, Addr: "127.0.0.1:2"}

	if err := store.WriteProxyState(first); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := store.WriteProxyState(second); err != nil {
		t.Fatalf("second write: %v", err)
	}

	data, err := os.ReadFile(store.ProxyStatePath())
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var decoded ProxyState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if decoded.PID != second.PID {
		t.Errorf("pid = %d, want %d (the write was not a replace)", decoded.PID, second.PID)
	}
}

func TestRemoveProxyStateToleratesAMissingFile(t *testing.T) {
	store := newRecorderStore(t)
	if err := store.RemoveProxyState(); err != nil {
		t.Fatalf("RemoveProxyState on an empty store: %v", err)
	}
	if err := store.WriteProxyState(&ProxyState{PID: 7}); err != nil {
		t.Fatalf("WriteProxyState: %v", err)
	}
	if err := store.RemoveProxyState(); err != nil {
		t.Fatalf("RemoveProxyState: %v", err)
	}
	if _, err := os.Stat(store.ProxyStatePath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state file survived removal (stat err = %v)", err)
	}
}

// TestHistoryFileIsPrivateAndDurable pins the append contract at the store
// seam: history.jsonl is 0600 even when an earlier writer left it wider, and a
// committed line is readable immediately after Commit.
func TestHistoryFileIsPrivateAndDurable(t *testing.T) {
	store := newRecorderStore(t)
	if err := store.EnsureLayout(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := os.WriteFile(store.HistoryPath(), []byte{}, 0o644); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if err := os.Chmod(store.HistoryPath(), 0o644); err != nil {
		t.Fatalf("chmod history: %v", err)
	}

	rec := openExchange(t, store, ExchangeSpec{Kind: KindSent})
	defer rec.Abort()
	if err := commitCompleted(rec); err != nil {
		t.Fatalf("commit: %v", err)
	}

	info, err := os.Stat(store.HistoryPath())
	if err != nil {
		t.Fatalf("stat history: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("history mode = %o, want corrected 600", got)
	}
	lines := historyObjects(t, store)
	if len(lines) != 1 || lines[0]["id"] != rec.Event().ID {
		t.Errorf("history = %v, want one line for %s", lines, rec.Event().ID)
	}
}

// TestCommitLeavesNoProtocolTemps pins that a completed record publishes every
// staged file and leaves the store clean for the next sweep.
func TestCommitLeavesNoProtocolTemps(t *testing.T) {
	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	request, _ := rec.Request()
	_, _ = request.Write([]byte("req"))
	response, _ := rec.Response()
	_, _ = response.Write([]byte("resp"))
	if err := commitCompleted(rec); err != nil {
		t.Fatalf("commit: %v", err)
	}

	for _, dir := range []string{store.Dir(), store.BodiesDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) == ".tmp" {
				t.Errorf("commit left %s in %s", entry.Name(), dir)
			}
		}
	}
}
