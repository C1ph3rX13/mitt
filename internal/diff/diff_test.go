package diff_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/diff"
	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// storedBody writes a body file of size bytes and returns the reference a
// history record would carry for it. Files are sparse: the comparison reads
// only up to diff.BodyLimitBytes, so the fixture must not cost the limit either.
func storedBody(t *testing.T, dir, name string, size int64) entry.StoredBody {
	t.Helper()
	path := filepath.Join(dir, name+".bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := file.Truncate(size); err != nil {
		file.Close()
		t.Fatalf("size %s: %v", name, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return entry.StoredBody{
		EntryID:   name,
		Direction: "response",
		Path:      path,
		Size:      size,
		SizeKnown: true,
	}
}

func response(body entry.StoredBody) entry.Response {
	return entry.Response{
		ID:      body.EntryID,
		Kind:    session.KindCaptured,
		URL:     "http://example.test/",
		Status:  200,
		Body:    body,
		Headers: [][2]string{{"Content-Length", "0"}},
	}
}

// TestCompareBoundsEachSide pins the resource bound of a comparison: a body
// larger than diff_body_max_bytes is read only up to the limit, and the result
// says so instead of loading the whole file. The fixture is sparse, so the test
// costs the limit rather than the file size.
func TestCompareBoundsEachSide(t *testing.T) {
	dir := t.TempDir()
	bigA := storedBody(t, dir, "big-a", int64(4*diff.BodyLimitBytes))
	bigB := storedBody(t, dir, "big-b", int64(4*diff.BodyLimitBytes))

	result, err := diff.Compare(response(bigA), response(bigB))
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !result.Body.Truncated {
		t.Error("body.truncated = false, want true for bodies over the limit")
	}
	// Reported sizes are the records' sizes, not the compared prefixes.
	if result.Body.SizeA != int64(4*diff.BodyLimitBytes) || result.Body.SizeB != int64(4*diff.BodyLimitBytes) {
		t.Errorf("size_a/size_b = %d/%d, want the full record sizes", result.Body.SizeA, result.Body.SizeB)
	}
	if result.Body.Type != diff.BodyTypeBinary {
		t.Errorf("body.type = %q, want binary for a zero-filled body", result.Body.Type)
	}
}

// TestCompareDoesNotCountMissingBytesAsEqual is the acceptance case for stored
// bodies: two records that claim the same non-zero size, whose files are both
// gone, must not compare as identical. Missing bytes are unknown, not empty.
func TestCompareDoesNotCountMissingBytesAsEqual(t *testing.T) {
	dir := t.TempDir()
	missingA := storedBody(t, dir, "missing-a", 7)
	missingB := storedBody(t, dir, "missing-b", 7)
	for _, body := range []entry.StoredBody{missingA, missingB} {
		if err := os.Remove(body.Path); err != nil {
			t.Fatalf("remove %s: %v", body.Path, err)
		}
	}

	if _, err := diff.Compare(response(missingA), response(missingB)); err == nil {
		t.Fatal("comparison of two vanished bodies succeeded; missing bytes were counted as equal")
	}

	// The diagnostic identifies the side that is damaged.
	var missing *entry.MissingBodyError
	_, err := diff.Summarize(response(missingA), response(missingB))
	if err == nil {
		t.Fatal("summary of two vanished bodies succeeded")
	}
	if !errors.As(err, &missing) {
		t.Fatalf("error = %v, want *entry.MissingBodyError", err)
	}
	if missing.EntryID != missingA.EntryID || missing.Size != 7 {
		t.Errorf("diagnostic = %+v, want entry %s and 7 bytes", missing, missingA.EntryID)
	}
}

// TestCompareTreatsAGenuineEmptyBodyAsEmpty pins the legitimate zero case: a
// record that claims no bytes compares as an empty body even when no file was
// ever published for it.
func TestCompareTreatsAGenuineEmptyBodyAsEmpty(t *testing.T) {
	dir := t.TempDir()
	emptyA := entry.StoredBody{
		EntryID: "empty-a", Direction: "response",
		Path: filepath.Join(dir, "empty-a.bin"), Size: 0, SizeKnown: true,
	}
	emptyB := storedBody(t, dir, "empty-b", 0)

	result, err := diff.Compare(response(emptyA), response(emptyB))
	if err != nil {
		t.Fatalf("compare empty bodies: %v", err)
	}
	if !result.Body.Same {
		t.Error("two empty bodies compare unequal")
	}
	if result.Body.SizeA != 0 || result.Body.SizeB != 0 || result.Body.Truncated {
		t.Errorf("body = %+v, want two untruncated zero sizes", result.Body)
	}
}
