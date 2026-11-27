package diff

import (
	"fmt"
	"strings"
)

// contextLines is the number of unchanged lines shown around each change in a
// unified diff hunk.
const contextLines = 3

// maxLCSCells bounds the dynamic-programming table used for the line diff.
// Larger inputs fall back to replacing the differing region; the cap keeps a
// pathological pair of bodies from consuming unbounded CPU and memory.
const maxLCSCells = 1 << 20

type editKind byte

const (
	editEqual  editKind = ' '
	editDelete editKind = '-'
	editInsert editKind = '+'
)

type edit struct {
	kind editKind
	line string
}

// unifiedDiff renders two text bodies as a unified diff with "--- a" and
// "+++ b" headers. Identical bodies produce an empty string.
func unifiedDiff(a, b []byte) string {
	edits := diffLines(splitLines(a), splitLines(b))
	return renderUnified(edits)
}

// splitLines splits on LF. A trailing newline does not produce an extra empty
// line, so "a\n" is one line while "a\n\n" is two.
func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines builds an edit script, trimming the common prefix and suffix before
// running LCS over the differing middle.
func diffLines(a, b []string) []edit {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}

	edits := make([]edit, 0, len(a)+len(b))
	for _, line := range a[:prefix] {
		edits = append(edits, edit{kind: editEqual, line: line})
	}
	edits = append(edits, middleEdits(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix])...)
	for _, line := range a[len(a)-suffix:] {
		edits = append(edits, edit{kind: editEqual, line: line})
	}
	return edits
}

// middleEdits diffs two line slices with no common prefix or suffix.
func middleEdits(a, b []string) []edit {
	if len(a) == 0 {
		return inserts(b)
	}
	if len(b) == 0 {
		return deletes(a)
	}
	if (len(a)+1)*(len(b)+1) > maxLCSCells {
		edits := deletes(a)
		return append(edits, inserts(b)...)
	}

	columns := len(b) + 1
	table := make([]int, (len(a)+1)*columns)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			index := i*columns + j
			switch {
			case a[i] == b[j]:
				table[index] = table[(i+1)*columns+j+1] + 1
			case table[(i+1)*columns+j] >= table[i*columns+j+1]:
				table[index] = table[(i+1)*columns+j]
			default:
				table[index] = table[i*columns+j+1]
			}
		}
	}

	edits := make([]edit, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			edits = append(edits, edit{kind: editEqual, line: a[i]})
			i++
			j++
		case table[(i+1)*columns+j] >= table[i*columns+j+1]:
			edits = append(edits, edit{kind: editDelete, line: a[i]})
			i++
		default:
			edits = append(edits, edit{kind: editInsert, line: b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		edits = append(edits, edit{kind: editDelete, line: a[i]})
	}
	for ; j < len(b); j++ {
		edits = append(edits, edit{kind: editInsert, line: b[j]})
	}
	return edits
}

func inserts(lines []string) []edit {
	edits := make([]edit, 0, len(lines))
	for _, line := range lines {
		edits = append(edits, edit{kind: editInsert, line: line})
	}
	return edits
}

func deletes(lines []string) []edit {
	edits := make([]edit, 0, len(lines))
	for _, line := range lines {
		edits = append(edits, edit{kind: editDelete, line: line})
	}
	return edits
}

// renderUnified groups changes into hunks with context and writes the
// standard unified-diff framing.
func renderUnified(edits []edit) string {
	changes := make([]int, 0)
	for index, item := range edits {
		if item.kind != editEqual {
			changes = append(changes, index)
		}
	}
	if len(changes) == 0 {
		return ""
	}

	type block struct{ start, end int }
	blocks := make([]block, 0)
	start, end := hunkRange(changes[0], len(edits))
	for _, index := range changes[1:] {
		nextStart, nextEnd := hunkRange(index, len(edits))
		if nextStart <= end {
			if nextEnd > end {
				end = nextEnd
			}
			continue
		}
		blocks = append(blocks, block{start: start, end: end})
		start, end = nextStart, nextEnd
	}
	blocks = append(blocks, block{start: start, end: end})

	oldLine := make([]int, len(edits)+1)
	newLine := make([]int, len(edits)+1)
	oldNo, newNo := 1, 1
	for index, item := range edits {
		oldLine[index] = oldNo
		newLine[index] = newNo
		switch item.kind {
		case editEqual:
			oldNo++
			newNo++
		case editDelete:
			oldNo++
		case editInsert:
			newNo++
		}
	}
	oldLine[len(edits)] = oldNo
	newLine[len(edits)] = newNo

	var buf strings.Builder
	buf.WriteString("--- a\n+++ b\n")
	for _, item := range blocks {
		oldCount, newCount := 0, 0
		for _, change := range edits[item.start:item.end] {
			if change.kind != editInsert {
				oldCount++
			}
			if change.kind != editDelete {
				newCount++
			}
		}
		fmt.Fprintf(&buf, "@@ -%s +%s @@\n",
			rangeHeader(oldLine[item.start], oldCount),
			rangeHeader(newLine[item.start], newCount))
		for _, change := range edits[item.start:item.end] {
			buf.WriteByte(byte(change.kind))
			buf.WriteString(change.line)
			buf.WriteByte('\n')
		}
	}
	return buf.String()
}

// hunkRange returns the edit indices covered by context around one change.
func hunkRange(index, length int) (int, int) {
	start := index - contextLines
	if start < 0 {
		start = 0
	}
	end := index + contextLines + 1
	if end > length {
		end = length
	}
	return start, end
}

// rangeHeader formats one side of a hunk header: GNU diff omits a count of 1
// and reports the line before the hunk when the side is empty.
func rangeHeader(start, count int) string {
	if count == 0 {
		start--
	}
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
