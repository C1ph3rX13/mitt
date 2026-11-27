package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// normalize converts arbitrary Go values into the JSON-shaped value tree used
// by the YAML and table renderers.
func normalize(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func objectRows(value any) ([]Row, error) {
	normalized, err := normalize(value)
	if err != nil {
		return nil, err
	}
	object, ok := normalized.(map[string]any)
	if !ok {
		return []Row{{Field{Name: "value", Value: normalized}}}, nil
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]Row, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, Row{
			Field{Name: "key", Value: key},
			Field{Name: "value", Value: object[key]},
		})
	}
	return rows, nil
}

func streamColumns(stream Stream) []string {
	if len(stream.Columns) > 0 {
		return stream.Columns
	}
	seen := map[string]bool{}
	var columns []string
	for _, row := range stream.Rows {
		for _, field := range row {
			if !seen[field.Name] {
				seen[field.Name] = true
				columns = append(columns, field.Name)
			}
		}
	}
	return columns
}

func displayValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(data)
	}
}

func rowValue(row Row, column string) string {
	for _, field := range row {
		if field.Name == column {
			return displayValue(field.Value)
		}
	}
	return ""
}

func writeTable(w io.Writer, stream Stream) error {
	columns := streamColumns(stream)
	if len(columns) == 0 {
		return nil
	}
	headers := make([]string, len(columns))
	widths := make([]int, len(columns))
	for i, column := range columns {
		headers[i] = strings.ToUpper(column)
		widths[i] = len(headers[i])
	}
	cells := make([][]string, len(stream.Rows))
	for r, row := range stream.Rows {
		cells[r] = make([]string, len(columns))
		for c, column := range columns {
			cells[r][c] = rowValue(row, column)
			if len(cells[r][c]) > widths[c] {
				widths[c] = len(cells[r][c])
			}
		}
	}

	writeColumns := func(values []string) {
		for i, value := range values {
			if i > 0 {
				fmt.Fprint(w, "  ")
			}
			fmt.Fprintf(w, "%-*s", widths[i], value)
		}
		fmt.Fprintln(w)
	}
	writeColumns(headers)
	separators := make([]string, len(widths))
	for i, width := range widths {
		separators[i] = strings.Repeat("-", width)
	}
	writeColumns(separators)
	for _, row := range cells {
		writeColumns(row)
	}
	return nil
}

func writeMarkdown(w io.Writer, stream Stream) error {
	columns := streamColumns(stream)
	if len(columns) == 0 {
		return nil
	}
	headers := make([]string, len(columns))
	for i, column := range columns {
		headers[i] = strings.ToUpper(column)
	}
	writeMarkdownRow(w, headers)
	separators := make([]string, len(columns))
	for i := range separators {
		separators[i] = "---"
	}
	writeMarkdownRow(w, separators)
	for _, row := range stream.Rows {
		values := make([]string, len(columns))
		for i, column := range columns {
			values[i] = rowValue(row, column)
		}
		writeMarkdownRow(w, values)
	}
	return nil
}

func writeMarkdownRow(w io.Writer, values []string) {
	for i, value := range values {
		if i == 0 {
			fmt.Fprint(w, "| ")
		} else {
			fmt.Fprint(w, " | ")
		}
		fmt.Fprint(w, escapeMarkdownCell(value))
	}
	fmt.Fprintln(w, " |")
}

func escapeMarkdownCell(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r\n", "<br>")
	value = strings.ReplaceAll(value, "\n", "<br>")
	return value
}
