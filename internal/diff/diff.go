// Package diff compares the responses of two history entries. It owns the
// status, header, and body sections of `mitt diff` and does not depend on the
// CLI or on how entries were captured.
package diff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/session"
)

// BodyLimitBytes is the maximum number of response body bytes compared for one
// side. Larger bodies are truncated before comparison so a single huge payload
// cannot flood the Agent's output. `mitt spec` registers this limit.
const BodyLimitBytes = 1 << 20

// Body classifications reported in the body section.
const (
	BodyTypeText   = "text"
	BodyTypeBinary = "binary"
)

// Response is one side of the comparison. It is the entry package's response
// projection rather than a second shape with the same fields: `diff` compares
// responses but must not know where an entry stores its body.
type Response = entry.Response

// Entry identifies one compared response. The kind is the session vocabulary,
// so `diff` cannot publish an enum the store does not write; both sides have a
// response by construction (HasResponse gates Compare).
type Entry struct {
	ID     string       `json:"id"`
	Kind   session.Kind `json:"kind"`
	Status int          `json:"status" schema:"min=100,max=999"`
	URL    string       `json:"url"`
}

// Status is the status-code section.
type Status struct {
	Same bool `json:"same"`
	A    int  `json:"a" schema:"min=100,max=999" desc:"First compared response; the parent entry for send --diff-against."`
	B    int  `json:"b" schema:"min=100,max=999" desc:"Second compared response; the new replay for send --diff-against."`
}

// HeaderChange is one response header whose values differ between the sides.
type HeaderChange struct {
	Name string   `json:"name"`
	A    []string `json:"a"`
	B    []string `json:"b"`
}

// Headers is the header section. Names are matched case-insensitively; added
// and removed pairs keep the stored order and duplicates.
type Headers struct {
	Same    bool           `json:"same"`
	Added   [][2]string    `json:"added" desc:"Header pairs present only in b, in stored order with duplicates."`
	Removed [][2]string    `json:"removed" desc:"Header pairs present only in a, in stored order with duplicates."`
	Changed []HeaderChange `json:"changed" desc:"Headers present on both sides whose value sequence differs. Names are matched case-insensitively."`
}

// Body is the body section. Text bodies carry a unified diff; binary bodies
// carry only equality, hashes, and sizes. Hashes cover the compared bytes,
// which are the full body unless Truncated is true; Same only asserts equality
// of those compared bytes plus the full sizes, never of unread tails.
type Body struct {
	Type        string `json:"type" schema:"enum=text|binary"`
	Same        bool   `json:"same" desc:"Equality of the compared bytes and full sizes; content beyond diff_body_max_bytes is never compared when truncated is true."`
	Truncated   bool   `json:"truncated"`
	SizeA       int64  `json:"size_a" schema:"min=0"`
	SizeB       int64  `json:"size_b" schema:"min=0"`
	SHA256A     string `json:"sha256_a" schema:"pattern=^[0-9a-f]{64}$" desc:"SHA-256 of the compared bytes; truncated to diff_body_max_bytes when truncated is true."`
	SHA256B     string `json:"sha256_b" schema:"pattern=^[0-9a-f]{64}$" desc:"SHA-256 of the compared bytes; truncated to diff_body_max_bytes when truncated is true."`
	UnifiedDiff string `json:"unified_diff,omitempty" desc:"Unified diff for text bodies; omitted for binary bodies and when the bodies are equal."`
}

// Result is the complete `mitt diff` success output.
type Result struct {
	A       Entry   `json:"a"`
	B       Entry   `json:"b"`
	Status  Status  `json:"status"`
	Headers Headers `json:"headers"`
	Body    Body    `json:"body"`
}

// comparedBodies holds the bounded body reads and text/binary classification
// shared by the detailed diff and the compact summary.
type comparedBodies struct {
	textA, textB           []byte
	sizeA, sizeB           int64
	truncatedA, truncatedB bool
	isTextA, isTextB       bool
	same                   bool
	sha256A, sha256B       string
}

func readComparedBodies(a, b Response) (*comparedBodies, error) {
	bodyA, sizeA, truncatedA, err := readBody(a)
	if err != nil {
		return nil, err
	}
	bodyB, sizeB, truncatedB, err := readBody(b)
	if err != nil {
		return nil, err
	}
	textA, isTextA := textContent(bodyA, truncatedA)
	textB, isTextB := textContent(bodyB, truncatedB)

	return &comparedBodies{
		textA: textA, textB: textB,
		sizeA: sizeA, sizeB: sizeB,
		truncatedA: truncatedA, truncatedB: truncatedB,
		isTextA: isTextA, isTextB: isTextB,
		same:    sizeA == sizeB && bytes.Equal(bodyA, bodyB),
		sha256A: sha256Hex(bodyA),
		sha256B: sha256Hex(bodyB),
	}, nil
}

// Compare loads both response bodies through the truncation limit and builds
// the three-section result.
func Compare(a, b Response) (*Result, error) {
	bodies, err := readComparedBodies(a, b)
	if err != nil {
		return nil, err
	}

	result := &Result{
		A: Entry{ID: a.ID, Kind: a.Kind, Status: a.Status, URL: a.URL},
		B: Entry{ID: b.ID, Kind: b.Kind, Status: b.Status, URL: b.URL},
		Status: Status{
			Same: a.Status == b.Status,
			A:    a.Status,
			B:    b.Status,
		},
		Headers: compareHeaders(a.Headers, b.Headers),
		Body: Body{
			Type:      BodyTypeBinary,
			Same:      bodies.same,
			Truncated: bodies.truncatedA || bodies.truncatedB,
			SizeA:     bodies.sizeA,
			SizeB:     bodies.sizeB,
			SHA256A:   bodies.sha256A,
			SHA256B:   bodies.sha256B,
		},
	}
	if bodies.isTextA && bodies.isTextB {
		result.Body.Type = BodyTypeText
		result.Body.UnifiedDiff = unifiedDiff(bodies.textA, bodies.textB)
	}
	return result, nil
}

// readBody reads at most BodyLimitBytes of one stored response body. The
// record's own reference decides what a missing file means: a body the entry
// says is empty reads as empty, and a body it claims bytes for must be present
// at that size. A comparison therefore never turns lost bytes into "same".
func readBody(response Response) ([]byte, int64, bool, error) {
	data, size, truncated, err := response.Body.Bytes(BodyLimitBytes)
	if err != nil {
		return nil, 0, false, err
	}
	return data, size, truncated, nil
}

// textContent reports whether data is a text body: valid UTF-8 with no NUL
// byte. When a truncated read splits the final rune, the partial rune is
// dropped before the validity check so truncation never turns text into binary.
func textContent(data []byte, truncated bool) ([]byte, bool) {
	if bytes.IndexByte(data, 0) >= 0 {
		return data, false
	}
	if utf8.Valid(data) {
		return data, true
	}
	if truncated {
		if trimmed := trimIncompleteRune(data); utf8.Valid(trimmed) {
			return trimmed, true
		}
	}
	return data, false
}

// trimIncompleteRune removes a trailing partial UTF-8 encoding, if any.
func trimIncompleteRune(data []byte) []byte {
	for offset := 1; offset <= utf8.UTFMax && offset <= len(data); offset++ {
		index := len(data) - offset
		if !utf8.RuneStart(data[index]) {
			continue
		}
		if utf8.FullRune(data[index:]) {
			return data
		}
		return data[:index]
	}
	return data
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type headerGroup struct {
	name   string
	values []string
}

// compareHeaders builds a case-insensitive, order-preserving header diff.
func compareHeaders(a, b [][2]string) Headers {
	groupsA, orderA := groupHeaders(a)
	groupsB, orderB := groupHeaders(b)

	result := Headers{
		Added:   [][2]string{},
		Removed: [][2]string{},
		Changed: []HeaderChange{},
	}
	for _, key := range orderA {
		groupA := groupsA[key]
		groupB, present := groupsB[key]
		if !present {
			for _, value := range groupA.values {
				result.Removed = append(result.Removed, [2]string{groupA.name, value})
			}
			continue
		}
		if !equalStrings(groupA.values, groupB.values) {
			result.Changed = append(result.Changed, HeaderChange{
				Name: groupA.name,
				A:    groupA.values,
				B:    groupB.values,
			})
		}
	}
	for _, key := range orderB {
		if _, present := groupsA[key]; present {
			continue
		}
		for _, value := range groupsB[key].values {
			result.Added = append(result.Added, [2]string{groupsB[key].name, value})
		}
	}
	result.Same = len(result.Added) == 0 && len(result.Removed) == 0 && len(result.Changed) == 0
	return result
}

func groupHeaders(headers [][2]string) (map[string]*headerGroup, []string) {
	groups := make(map[string]*headerGroup, len(headers))
	order := make([]string, 0, len(headers))
	for _, pair := range headers {
		key := strings.ToLower(pair[0])
		group, present := groups[key]
		if !present {
			group = &headerGroup{name: pair[0]}
			groups[key] = group
			order = append(order, key)
		}
		group.values = append(group.values, pair[1])
	}
	return groups, order
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
