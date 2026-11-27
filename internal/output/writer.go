package output

import (
	"encoding/json"
	"io"
)

// Writer renders command results using the selected format.
type Writer struct {
	out    io.Writer
	format Format
}

// NewWriter builds a writer for one command invocation.
func NewWriter(out io.Writer, format Format) *Writer {
	return &Writer{out: out, format: format}
}

// Object renders a single result object. JSON and YAML stay structural; table
// and markdown formats render the object as key/value rows.
func (w *Writer) Object(value any) error {
	switch w.format {
	case FormatJSON:
		return writeJSON(w.out, value)
	case FormatYAML:
		return writeYAML(w.out, value)
	case FormatTable, FormatMarkdown:
		rows, err := objectRows(value)
		if err != nil {
			return err
		}
		return w.Stream(Stream{
			Columns: []string{"key", "value"},
			Rows:    rows,
		})
	default:
		return writeJSON(w.out, value)
	}
}

// Stream renders an ordered sequence of records. JSON is NDJSON, YAML is a
// list, and table/markdown render one row per record.
func (w *Writer) Stream(stream Stream) error {
	switch w.format {
	case FormatJSON:
		return writeNDJSON(w.out, stream.Rows)
	case FormatYAML:
		return writeYAML(w.out, streamRowsAsMaps(stream.Rows))
	case FormatTable:
		return writeTable(w.out, stream)
	case FormatMarkdown:
		return writeMarkdown(w.out, stream)
	default:
		return writeNDJSON(w.out, stream.Rows)
	}
}

// Field is one named value in a stream record.
type Field struct {
	Name  string
	Value any
}

// Row is an ordered stream record.
type Row []Field

// Stream is an ordered result set with explicit column order.
type Stream struct {
	Columns []string
	Rows    []Row
}

func (r Row) asMap() map[string]any {
	m := make(map[string]any, len(r))
	for _, field := range r {
		m[field.Name] = field.Value
	}
	return m
}

func streamRowsAsMaps(rows []Row) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.asMap())
	}
	return out
}

// MarshalJSON preserves the declared field order in NDJSON output.
func (r Row) MarshalJSON() ([]byte, error) {
	buf := make([]byte, 0, 64)
	buf = append(buf, '{')
	for i, field := range r {
		if i > 0 {
			buf = append(buf, ',')
		}
		name, err := json.Marshal(field.Name)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(field.Value)
		if err != nil {
			return nil, err
		}
		buf = append(buf, name...)
		buf = append(buf, ':')
		buf = append(buf, value...)
	}
	buf = append(buf, '}')
	return buf, nil
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}

func writeNDJSON(w io.Writer, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}
