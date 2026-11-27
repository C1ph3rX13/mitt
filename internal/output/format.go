package output

import (
	"fmt"
	"strings"
)

// Format selects how command data is rendered on stdout.
type Format string

const (
	FormatJSON     Format = "json"
	FormatTable    Format = "table"
	FormatYAML     Format = "yaml"
	FormatMarkdown Format = "md"
)

// FormatNames is the flag-level enum for -f/--format.
var FormatNames = []string{
	string(FormatJSON),
	string(FormatTable),
	string(FormatYAML),
	string(FormatMarkdown),
}

// ParseFormat validates a -f/--format value.
func ParseFormat(value string) (Format, error) {
	for _, name := range FormatNames {
		if value == name {
			return Format(name), nil
		}
	}
	return "", fmt.Errorf("must be one of %s", strings.Join(FormatNames, ", "))
}

// Structural reports whether the format renders the Go value tree itself
// (JSON and YAML) rather than projecting it into rows (table and md). Output
// shapes that only exist as rows are selected for the non-structural formats.
func (f Format) Structural() bool {
	return f == FormatJSON || f == FormatYAML
}
