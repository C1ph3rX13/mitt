package output

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// StreamOf renders records as an ordered stream. Column order follows the JSON
// field order of T, so the same struct that declares the output schema also
// fixes the rendered column order and the two cannot drift.
func StreamOf[T any](records []T) (Stream, error) {
	columns := make([]string, 0, 8)
	seen := map[string]bool{}
	rows := make([]Row, 0, len(records))
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			return Stream{}, fmt.Errorf("encode stream record: %w", err)
		}
		row, err := decodeOrderedRow(data)
		if err != nil {
			return Stream{}, err
		}
		for _, field := range row {
			if !seen[field.Name] {
				seen[field.Name] = true
				columns = append(columns, field.Name)
			}
		}
		rows = append(rows, row)
	}
	return Stream{Columns: columns, Rows: rows}, nil
}

// decodeOrderedRow decodes one JSON object into an ordered Row. encoding/json
// discards object order when decoding into a map, so the record is read token
// by token instead.
func decodeOrderedRow(data []byte) (Row, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("decode stream record: %w", err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("decode stream record: expected a JSON object, got %v", token)
	}
	var row Row
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("decode stream record key: %w", err)
		}
		name, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("decode stream record key: %v is not a string", token)
		}
		var value any
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode stream record field %q: %w", name, err)
		}
		row = append(row, Field{Name: name, Value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("decode stream record: %w", err)
	}
	return row, nil
}
