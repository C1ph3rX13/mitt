package replay_test

import (
	"os"
	"strings"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/replay"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// TestInheritedStoredBodyKeepsTheRecordsClaim pins the replay side of the
// stored-body seam. A base entry's body is read under the record's own size, so
// an entry whose body file no longer matches its record cannot be replayed as a
// shorter (or empty) request. A patch that replaces the body names new bytes,
// so it is unaffected by the damaged record it was based on.
func TestInheritedStoredBodyKeepsTheRecordsClaim(t *testing.T) {
	store, id := newEntryStore(t)
	bodyPath := store.BodyPath(id, session.BodyRequest)
	if err := os.Truncate(bodyPath, 2); err != nil {
		t.Fatalf("truncate stored body: %v", err)
	}

	base := replay.EntryBase{ID: id}

	// No patch: the inherited body is the record's body.
	_, err := replay.DryRun(replay.SendRequest{Store: store, Base: base})
	assertCode(t, err, output.CodeInternalError)

	// An empty patch keeps the base document's body_file, so it keeps the
	// record's claim too: the replay cannot quietly send two bytes.
	_, err = replay.DryRun(replay.SendRequest{Store: store, Base: base, Patch: replay.InlinePatch{Text: `{}`}})
	assertCode(t, err, output.CodeInternalError)

	// Naming new body bytes replaces the reference; the damaged file is not
	// consulted, and the replay is accepted.
	result, err := replay.DryRun(replay.SendRequest{
		Store: store,
		Base:  base,
		Patch: replay.InlinePatch{Text: `{"body_file":null,"body":"fresh"}`},
	})
	if err != nil {
		t.Fatalf("dry run with a replacement body: %v", err)
	}
	if !strings.HasSuffix(string(result.Preview), "\r\n\r\nfresh") {
		t.Errorf("preview does not carry the replacement body:\n%q", result.Preview)
	}
}

// TestPatchNamedBodyFileStillDiagnosesAMissingFile pins the other half of the
// rule: a body_file the caller names is the caller's path, so an unreadable one
// is an INVALID_PATCH rather than store damage.
func TestPatchNamedBodyFileStillDiagnosesAMissingFile(t *testing.T) {
	store, id := newEntryStore(t)
	absent := t.TempDir() + "/absent.bin"

	_, err := replay.DryRun(replay.SendRequest{
		Store: store,
		Base:  replay.EntryBase{ID: id},
		Patch: replay.InlinePatch{Text: `{"body_file":null,"body":"x","body_file":"` + absent + `"}`},
	})
	assertCode(t, err, output.CodeInvalidPatch)
}
