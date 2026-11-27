package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// stagedRequest writes bytes into the exchange's request body stage and
// returns the body with its temporary path so a crash test can age it. The
// sweep sees only the staged file, never the kind that produced it.
func stagedRequest(t *testing.T, exchange *Exchange) (*Body, string) {
	t.Helper()
	body, err := exchange.Request()
	if err != nil {
		t.Fatalf("open request body: %v", err)
	}
	temp := body.stage.TempPath()
	if _, err := body.Write([]byte("staged bytes")); err != nil {
		t.Fatalf("write request body: %v", err)
	}
	return body, temp
}

func agePath(t *testing.T, path string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
}

// TestSweepRemovesCrashLeftBodyTemps seeds residue through the body writer
// itself: a stage that is abandoned instead of published is exactly what a
// crash between CreateTemp and rename leaves behind.
func TestSweepRemovesCrashLeftBodyTemps(t *testing.T) {
	store := newRecorderStore(t)
	rec, err := store.BeginExchange(ExchangeSpec{Kind: KindSent, Started: time.Now()})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	body, temp := stagedRequest(t, rec)
	body.stage.Abandon()
	t.Cleanup(rec.Abort)
	agePath(t, temp, 2*atomicfile.StaleTempMaxAge)

	report, err := store.sweepStaleTemps(atomicfile.StaleTempMaxAge)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := report.removedByDir[store.BodiesDir()]; got != 1 {
		t.Errorf("removed from bodies = %d, want 1 (%+v)", got, report)
	}
	if _, err := os.Stat(temp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale body temp survived the sweep: %v", err)
	}

	again, err := store.sweepStaleTemps(atomicfile.StaleTempMaxAge)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if !again.empty() {
		t.Errorf("second sweep = %+v, want empty", again)
	}
}

// TestSweepCoversStoreRootAndCADirectory pins the protocol's reach: every
// directory a writer stages into is swept, not only bodies.
func TestSweepCoversStoreRootAndCADirectory(t *testing.T) {
	store := newRecorderStore(t)
	if err := store.EnsureLayout(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := atomicfile.EnsureDir(store.CADir(), atomicfile.Private.DirMode); err != nil {
		t.Fatalf("create CA dir: %v", err)
	}
	seeds := []string{
		filepath.Join(store.Dir(), "proxy.json"),
		filepath.Join(store.BodiesDir(), "entry-req.bin"),
		filepath.Join(store.CADir(), "ca.key.pem"),
	}
	temps := make([]string, 0, len(seeds))
	for _, finalPath := range seeds {
		stage, err := atomicfile.NewStage(finalPath, atomicfile.Private)
		if err != nil {
			t.Fatalf("stage %s: %v", finalPath, err)
		}
		if _, err := stage.Write([]byte("residue")); err != nil {
			t.Fatalf("write: %v", err)
		}
		stage.Abandon()
		agePath(t, stage.TempPath(), 2*atomicfile.StaleTempMaxAge)
		temps = append(temps, stage.TempPath())
	}

	report, err := store.sweepStaleTemps(atomicfile.StaleTempMaxAge)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(report.inUse) != 0 || len(report.failures) != 0 {
		t.Errorf("sweep = %+v, want no in-use entries and no failures", report)
	}
	total := 0
	for _, count := range report.removedByDir {
		total += count
	}
	if total != len(temps) {
		t.Errorf("removed = %d, want %d (%+v)", total, len(temps), report)
	}
	for _, temp := range temps {
		if _, err := os.Stat(temp); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived the sweep: %v", temp, err)
		}
	}
}

func TestSweepKeepsBodyTempHeldByLiveWriter(t *testing.T) {
	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindCaptured, ClientAddr: "127.0.0.1:1"})
	defer rec.Abort()
	_, temp := stagedRequest(t, rec)

	// A writer that has been busy for longer than the grace period is still
	// protected by its lock; maxAge zero proves the lock is what matters.
	agePath(t, temp, 2*atomicfile.StaleTempMaxAge)
	report, err := store.sweepStaleTemps(0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(report.inUse) != 1 || report.inUse[0] != filepath.Base(temp) {
		t.Errorf("in-use = %v, want [%s]", report.inUse, filepath.Base(temp))
	}
	if len(report.removedByDir) != 0 {
		t.Errorf("removed = %v, want none", report.removedByDir)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Fatalf("sweep removed a body with a live writer: %v", err)
	}

	if err := commitCompleted(rec); err != nil {
		t.Fatalf("commit after sweep: %v", err)
	}
	if got := readBodyFile(t, store.BodyPath(rec.Event().ID, BodyRequest)); got != "staged bytes" {
		t.Errorf("request body = %q, want staged bytes", got)
	}
}

// TestSweepKeepsSealedBodyUntilPublished pins the seal→publish window: seal
// syncs and marks the body closed for writing but must keep the writer lock,
// or a sweep could delete a body between its sync and its rename.
func TestSweepKeepsSealedBodyUntilPublished(t *testing.T) {
	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindSent})
	defer rec.Abort()
	body, temp := stagedRequest(t, rec)
	if err := body.seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := body.Write([]byte("x")); err == nil {
		t.Error("write after seal succeeded, want an error")
	}
	if err := body.seal(); err == nil {
		t.Error("second seal succeeded, want an error")
	}

	agePath(t, temp, 2*atomicfile.StaleTempMaxAge)
	report, err := store.sweepStaleTemps(0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(report.removedByDir) != 0 {
		t.Errorf("sweep removed a sealed body before publication: %v", report.removedByDir)
	}
	if len(report.inUse) != 1 {
		t.Errorf("in-use = %v, want the sealed temp file", report.inUse)
	}

	// Commit seals again, which the body rejects, so finish the record by hand:
	// the point of this test is the seal→publish window, not the commit range.
	if err := body.publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := readBodyFile(t, store.BodyPath(rec.Event().ID, BodyRequest)); got != "staged bytes" {
		t.Errorf("request body = %q, want staged bytes", got)
	}
}

func TestSweepKeepsFreshBodyTemps(t *testing.T) {
	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindSent})
	defer rec.Abort()
	body, temp := stagedRequest(t, rec)
	body.stage.Abandon()

	report, err := store.sweepStaleTemps(atomicfile.StaleTempMaxAge)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !report.empty() {
		t.Errorf("sweep = %+v, want empty", report)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Fatalf("sweep removed a fresh temp file: %v", err)
	}
}

func TestSweepLeavesPublishedBodiesAndHistoryAlone(t *testing.T) {
	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindSent})
	request, _ := rec.Request()
	_, _ = request.Write([]byte("published request"))
	response, _ := rec.Response()
	_, _ = response.Write([]byte("published response"))
	if err := commitCompleted(rec); err != nil {
		t.Fatalf("commit: %v", err)
	}
	id := rec.Event().ID

	crashRec, err := store.BeginExchange(ExchangeSpec{Kind: KindSent, Started: time.Now()})
	if err != nil {
		t.Fatalf("begin crash record: %v", err)
	}
	defer crashRec.Abort()
	body, stale := stagedRequest(t, crashRec)
	body.stage.Abandon()
	agePath(t, stale, 2*atomicfile.StaleTempMaxAge)

	report, err := store.sweepStaleTemps(atomicfile.StaleTempMaxAge)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := report.removedByDir[store.BodiesDir()]; got != 1 {
		t.Errorf("removed from bodies = %d, want 1", got)
	}

	if got := readBodyFile(t, store.BodyPath(id, BodyRequest)); got != "published request" {
		t.Errorf("request body = %q, want published request", got)
	}
	if got := readBodyFile(t, store.BodyPath(id, BodyResponse)); got != "published response" {
		t.Errorf("response body = %q, want published response", got)
	}
	if lines := historyObjects(t, store); len(lines) != 1 || lines[0]["id"] != id {
		t.Errorf("history = %v, want the single committed line %s", lines, id)
	}
}

func TestSweepIgnoresNonPatternNames(t *testing.T) {
	store := newRecorderStore(t)
	if err := store.EnsureLayout(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	old := time.Now().Add(-2 * atomicfile.StaleTempMaxAge)
	kept := []string{
		filepath.Join(store.BodiesDir(), "entry-req.bin"),
		filepath.Join(store.BodiesDir(), "notes.tmp"),
		filepath.Join(store.BodiesDir(), ".body-123.tmp"),
		filepath.Join(store.Dir(), ".proxy-123.json"),
	}
	for _, path := range kept {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("age %s: %v", path, err)
		}
	}

	report, err := store.sweepStaleTemps(atomicfile.StaleTempMaxAge)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !report.empty() {
		t.Errorf("sweep = %+v, want empty", report)
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("sweep touched %s: %v", path, err)
		}
	}
}

func TestSweepWithoutLayoutIsNotAnError(t *testing.T) {
	store := newRecorderStore(t)
	report, err := store.sweepStaleTemps(atomicfile.StaleTempMaxAge)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !report.empty() {
		t.Errorf("sweep = %+v, want empty", report)
	}
}

// TestSweepReportsResidueThroughTheStoreAPI pins the operator-visible shape:
// the sweep is silent until it removes something, then names the directory.
func TestSweepReportsResidueThroughTheStoreAPI(t *testing.T) {
	store := newRecorderStore(t)
	rec := openExchange(t, store, ExchangeSpec{Kind: KindSent})
	defer rec.Abort()
	body, temp := stagedRequest(t, rec)
	body.stage.Abandon()
	agePath(t, temp, 2*atomicfile.StaleTempMaxAge)

	var stderr strings.Builder
	store.SweepStaleTemps(&stderr)
	if got := stderr.String(); !strings.Contains(got, "removed 1 stale temporary file(s) from "+store.BodiesDir()) {
		t.Errorf("sweep report = %q, want it to name the bodies directory", got)
	}

	stderr.Reset()
	store.SweepStaleTemps(&stderr)
	if stderr.Len() != 0 {
		t.Errorf("clean sweep wrote %q, want silence", stderr.String())
	}
}

// TestSweepToleratesANilWriter pins that calling the sweep from a command with
// no diagnostic sink cannot panic.
func TestSweepToleratesANilWriter(t *testing.T) {
	store := newRecorderStore(t)
	store.SweepStaleTemps(nil)
}
