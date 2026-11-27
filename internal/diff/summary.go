package diff

// Summary is the compact response comparison attached by
// `mitt send --diff-against`. It keeps the three signals a replay needs —
// status, header changes, and body byte difference/similarity — without the
// unified diff, so a replay result stays easy to scan.
type Summary struct {
	Status  Status      `json:"status"`
	Headers Headers     `json:"headers"`
	Body    BodySummary `json:"body"`
	Changed bool        `json:"changed" desc:"True when the status, headers, or body differ."`
}

// BodySummary describes how the two response bodies differ. SizeDelta is
// size_b - size_a. Similarity is a line-based ratio in [0,1] for text bodies
// and 1 only for identical binary bodies; it is never based on bytes beyond
// the 1 MiB comparison limit.
type BodySummary struct {
	Type       string  `json:"type" schema:"enum=text|binary"`
	Same       bool    `json:"same"`
	Truncated  bool    `json:"truncated"`
	SizeA      int64   `json:"size_a" schema:"min=0" desc:"Full on-disk parent body size in bytes."`
	SizeB      int64   `json:"size_b" schema:"min=0" desc:"Full on-disk replay body size in bytes."`
	SizeDelta  int64   `json:"size_delta" desc:"size_b - size_a; negative when the replay body is smaller."`
	SHA256A    string  `json:"sha256_a" schema:"pattern=^[0-9a-f]{64}$" desc:"SHA-256 of the compared bytes; truncated to diff_body_max_bytes when truncated is true."`
	SHA256B    string  `json:"sha256_b" schema:"pattern=^[0-9a-f]{64}$" desc:"SHA-256 of the compared bytes; truncated to diff_body_max_bytes when truncated is true."`
	Similarity float64 `json:"similarity" schema:"min=0,max=1" desc:"Line-based shared-content ratio for text bodies; 1 for identical binary bodies and for two empty bodies."`
}

// Summarize compares two responses under the same body limit and body
// classification as Compare. It is the summary form used by `send
// --diff-against`; `mitt diff` keeps the detailed unified-diff form.
func Summarize(a, b Response) (*Summary, error) {
	bodies, err := readComparedBodies(a, b)
	if err != nil {
		return nil, err
	}

	summary := &Summary{
		Status:  Status{Same: a.Status == b.Status, A: a.Status, B: b.Status},
		Headers: compareHeaders(a.Headers, b.Headers),
		Body: BodySummary{
			Type:      BodyTypeBinary,
			Same:      bodies.same,
			Truncated: bodies.truncatedA || bodies.truncatedB,
			SizeA:     bodies.sizeA,
			SizeB:     bodies.sizeB,
			SizeDelta: bodies.sizeB - bodies.sizeA,
			SHA256A:   bodies.sha256A,
			SHA256B:   bodies.sha256B,
		},
	}
	switch {
	case bodies.isTextA && bodies.isTextB:
		summary.Body.Type = BodyTypeText
		summary.Body.Similarity = lineSimilarity(bodies.textA, bodies.textB)
	case bodies.same:
		summary.Body.Similarity = 1
	}
	summary.Changed = !summary.Status.Same || !summary.Headers.Same || !summary.Body.Same
	return summary, nil
}

// lineSimilarity is the shared-line ratio 2*LCS/(len(a)+len(b)) using the same
// bounded line alignment as the unified diff, so large bodies degrade to the
// same prefix/suffix-only answer instead of unbounded work.
func lineSimilarity(a, b []byte) float64 {
	linesA, linesB := splitLines(a), splitLines(b)
	if len(linesA) == 0 && len(linesB) == 0 {
		return 1
	}
	equal := 0
	for _, item := range diffLines(linesA, linesB) {
		if item.kind == editEqual {
			equal++
		}
	}
	return 2 * float64(equal) / float64(len(linesA)+len(linesB))
}
