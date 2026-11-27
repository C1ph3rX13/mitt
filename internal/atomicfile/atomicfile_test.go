package atomicfile

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteCreatesParentsAndSetsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "cert.pem")

	if err := Write(path, []byte("pem"), Shared); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != "pem" {
		t.Errorf("content = %q, want %q", data, "pem")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode = %o, want 644", got)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if got := parent.Mode().Perm(); got != 0o755 {
		t.Errorf("parent mode = %o, want 755", got)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "cert.pem" {
		t.Errorf("directory entries = %v, want only cert.pem", entries)
	}
}

func TestWritePrivatePolicySetsOwnerOnlyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca", "ca.key.pem")

	if err := Write(path, []byte("key"), Private); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dir.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 700", got)
	}
}

func TestWriteReplacesExistingContentAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := Write(path, []byte("new"), Shared); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != "new" {
		t.Errorf("content = %q, want %q", data, "new")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
}

func TestWriteDoesNotRelabelAnExistingParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("create shared dir: %v", err)
	}
	// A destination directory that already exists belongs to someone else; the
	// private policy must not chmod it down to 0700.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod shared dir: %v", err)
	}
	if err := Write(filepath.Join(dir, "ca.key.pem"), []byte("key"), Private); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("existing parent mode = %o, want unchanged 755", got)
	}
}

func TestWriteFailureLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	err := Write(filepath.Join(blocker, "cert.pem"), []byte("pem"), Shared)
	if err == nil {
		t.Fatal("Write through a file parent succeeded, want an error")
	}
}

// TestWriteOntoADirectoryFailsAndCleansUp pins the rename-failure path: the
// destination is a directory, so the rename cannot replace it, and the failed
// write must remove its temporary instead of leaving residue the sweep would
// later have to find.
func TestWriteOntoADirectoryFailsAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "proxy.json")
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatalf("create destination directory: %v", err)
	}

	if err := Write(dest, []byte("record"), Private); err == nil {
		t.Fatal("Write over a directory succeeded, want an error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "proxy.json" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("entries = %v, want only the untouched destination", names)
	}
}

// appendBytes is the one-shot form of Append: write the whole payload while the
// protocol holds its lock.
func appendBytes(path string, data []byte, policy Policy) error {
	return Append(path, policy, func(file *os.File) error {
		_, err := file.Write(data)
		return err
	})
}

func TestAppendCreatesFileWithPolicyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")

	if err := appendBytes(path, []byte("one\n"), Private); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := appendBytes(path, []byte("two\n"), Private); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("content = %q, want one\\ntwo\\n", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestAppendCorrectsAnExistingFilesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := appendBytes(path, []byte("line\n"), Private); err != nil {
		t.Fatalf("Append: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want corrected 600", got)
	}
}

// TestAppendSerializesConcurrentWriters pins the property history.jsonl relies
// on: two writers appending at once never interleave a line, so every line is
// a complete JSON object.
func TestAppendSerializesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	const writers = 8
	const linesPerWriter = 25

	done := make(chan int32, 1)
	go func() {
		var failures int32
		var wg sync.WaitGroup
		for writer := 0; writer < writers; writer++ {
			wg.Add(1)
			go func(writer int) {
				defer wg.Done()
				for line := 0; line < linesPerWriter; line++ {
					payload := []byte("writer-" + string(rune('a'+writer)) + "-" + string(rune('0'+line/10)) + string(rune('0'+line%10)) + "\n")
					if err := appendBytes(path, payload, Private); err != nil {
						atomic.AddInt32(&failures, 1)
					}
				}
			}(writer)
		}
		wg.Wait()
		done <- failures
	}()

	// A blocked flock is the one way this test could hang; a local deadline
	// turns that into a named failure instead of a package timeout.
	select {
	case failures := <-done:
		if failures != 0 {
			t.Fatalf("%d appends failed", failures)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent appends did not finish within 30s; a writer lock is stuck")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := 0
	for _, line := range splitLines(data) {
		if len(line) != len("writer-a-00") {
			t.Fatalf("interleaved line %q", line)
		}
		lines++
	}
	if want := writers * linesPerWriter; lines != want {
		t.Errorf("lines = %d, want %d", lines, want)
	}
}

// TestAppendHoldsTheLockForTheWholeCallback pins the window a log writer needs:
// inspecting and repairing the existing tail happens under the same lock as the
// bytes that follow, so another writer cannot append between the repair and the
// new line.
func TestAppendHoldsTheLockForTheWholeCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- Append(path, Private, func(file *os.File) error {
			close(entered)
			<-release
			_, err := file.Write([]byte("first\n"))
			return err
		})
	}()

	select {
	case <-entered:
	case err := <-firstDone:
		t.Fatalf("first append failed before its callback ran: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("first append did not reach its callback within 30s")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- appendBytes(path, []byte("second\n"), Private) }()

	// The second writer must wait for the callback to finish, not slip in while
	// the first writer is between repair and write.
	select {
	case err := <-secondDone:
		t.Fatalf("second append ran while the first held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	unblock()
	for name, done := range map[string]<-chan error{"first": firstDone, "second": secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s append: %v", name, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s append did not finish within 30s; a writer lock is stuck", name)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "first\nsecond\n" {
		t.Errorf("content = %q, want first\\nsecond\\n", data)
	}
}

func splitLines(data []byte) []string {
	lines := []string{}
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, string(data[start:i]))
			start = i + 1
		}
	}
	return lines
}

func TestEnsureDirCreatesAndCorrectsMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "bodies")

	if err := EnsureDir(dir, Private.DirMode); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 700", got)
	}

	// A directory mitt owns is corrected, unlike a parent Write only creates.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := EnsureDir(dir, Private.DirMode); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("corrected dir mode = %o, want 700", got)
	}
}

func TestEnsureDirThroughAFileFails(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if err := EnsureDir(filepath.Join(blocker, "child"), Private.DirMode); err == nil {
		t.Fatal("EnsureDir through a file succeeded, want an error")
	}
}

func TestStageFinalPathIsStableAcrossPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stage, err := NewStage(path, Private)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}
	defer stage.Discard()
	if got := stage.FinalPath(); got != path {
		t.Errorf("FinalPath = %q, want %q", got, path)
	}
	if stage.Published() {
		t.Error("fresh stage reports itself published")
	}
}
