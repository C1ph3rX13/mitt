package replay

import "github.com/C1ph3rX13/mitt/internal/entry"

// PreviewLimitBytes is the fixed number of rendered request bytes included in
// a --dry-run preview. `mitt spec` registers this limit.
const PreviewLimitBytes = 8 << 10

// DryRunResult is the rendered but unsent form of one send request.
type DryRunResult struct {
	Document          *entry.Document
	Preview           []byte
	RequestTotalBytes int
	Truncated         bool
}

// DryRun resolves the base and patch into the exact request that would be
// sent. It never dials the target and never writes history or body files. The
// document and the preview come from one rendering, so what an Agent previews
// as a document is what it previews as bytes.
//
// DryRun runs the same preconditions Send does — the same TLS profile check and
// the same base/patch resolution — so a preview success cannot hide a send
// failure of those kinds.
func DryRun(request SendRequest) (*DryRunResult, error) {
	resolved, err := resolve(request)
	if err != nil {
		return nil, err
	}
	prepared := resolved.prepared
	preview := prepared.raw
	truncated := len(preview) > PreviewLimitBytes
	if truncated {
		preview = preview[:PreviewLimitBytes]
	}
	return &DryRunResult{
		Document:          prepared.document,
		Preview:           preview,
		RequestTotalBytes: len(prepared.raw),
		Truncated:         truncated,
	}, nil
}
