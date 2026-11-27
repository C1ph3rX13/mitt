package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

func writeYAML(w io.Writer, value any) error {
	normalized, err := normalize(value)
	if err != nil {
		return err
	}
	emitter := &yamlEmitter{w: w}
	return emitter.node(normalized, 0)
}

type yamlEmitter struct {
	w io.Writer
}

func (e *yamlEmitter) node(value any, indent int) error {
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			return e.line(indent, "{}")
		}
		return e.mapping(v, indent)
	case []any:
		if len(v) == 0 {
			return e.line(indent, "[]")
		}
		return e.sequence(v, indent)
	default:
		return e.line(indent, scalarYAML(v))
	}
}

func (e *yamlEmitter) mapping(value map[string]any, indent int) error {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		item := value[key]
		prefix := strings.Repeat(" ", indent) + yamlKey(key) + ":"
		if isNonEmptyComposite(item) {
			if _, err := fmt.Fprintln(e.w, prefix); err != nil {
				return err
			}
			if err := e.node(item, indent+2); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintln(e.w, prefix+" "+compactYAML(item)); err != nil {
			return err
		}
	}
	return nil
}

func (e *yamlEmitter) sequence(value []any, indent int) error {
	for _, item := range value {
		if mapping, ok := item.(map[string]any); ok && len(mapping) > 0 {
			if err := e.sequenceMapping(mapping, indent); err != nil {
				return err
			}
			continue
		}
		if isNonEmptyComposite(item) {
			if _, err := fmt.Fprintln(e.w, strings.Repeat(" ", indent)+"-"); err != nil {
				return err
			}
			if err := e.node(item, indent+2); err != nil {
				return err
			}
			continue
		}
		if err := e.line(indent, "- "+compactYAML(item)); err != nil {
			return err
		}
	}
	return nil
}

func (e *yamlEmitter) sequenceMapping(value map[string]any, indent int) error {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		prefix := strings.Repeat(" ", indent+2)
		if i == 0 {
			prefix = strings.Repeat(" ", indent) + "- "
		}
		item := value[key]
		head := prefix + yamlKey(key) + ":"
		if isNonEmptyComposite(item) {
			if _, err := fmt.Fprintln(e.w, head); err != nil {
				return err
			}
			if err := e.node(item, indent+4); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintln(e.w, head+" "+compactYAML(item)); err != nil {
			return err
		}
	}
	return nil
}

func (e *yamlEmitter) line(indent int, text string) error {
	_, err := fmt.Fprintln(e.w, strings.Repeat(" ", indent)+text)
	return err
}

func isNonEmptyComposite(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	default:
		return false
	}
}

func compactYAML(value any) string {
	switch v := value.(type) {
	case map[string]any:
		return "{}"
	case []any:
		return "[]"
	default:
		return scalarYAML(v)
	}
}

func scalarYAML(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

func yamlKey(key string) string {
	if isSafeYAMLKey(key) {
		return key
	}
	return strconv.Quote(key)
}

func isSafeYAMLKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return false
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}
