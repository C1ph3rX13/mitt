package cli_test

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// validateJSONSchema checks value against the draft 2020-12 keyword subset used
// by mitt's registered schemas: $ref, type, enum, minLength, pattern, required,
// properties, additionalProperties, items, minItems, maxItems, minimum,
// maximum, allOf, and not. Annotation-only keywords ($schema, $id, title,
// description, format, contentEncoding) are ignored. It returns one message
// per violation.
func validateJSONSchema(schema any, root any, value any, path string) []string {
	node, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	if ref, ok := node["$ref"].(string); ok {
		resolved, ok := resolveLocalRef(root, ref)
		if !ok {
			return []string{path + ": unresolvable $ref " + ref}
		}
		return validateJSONSchema(resolved, root, value, path)
	}
	problems := make([]string, 0)
	report := func(format string, args ...any) {
		problems = append(problems, path+": "+fmt.Sprintf(format, args...))
	}

	if forbidden, ok := node["not"].(map[string]any); ok {
		// A subschema that validates the value forbids it. The nested check
		// is a predicate, so its messages are discarded.
		if len(validateJSONSchema(forbidden, root, value, path)) == 0 {
			report("value matches a forbidden subschema")
		}
	}
	if clauses, ok := node["allOf"].([]any); ok {
		for _, clause := range clauses {
			problems = append(problems, validateJSONSchema(clause, root, value, path)...)
		}
	}
	if rawType, ok := node["type"]; ok && !matchesJSONType(rawType, value) {
		report("type %s does not match %v", jsonValueType(value), rawType)
		return problems
	}
	if enum, ok := node["enum"].([]any); ok && len(enum) > 0 {
		found := false
		for _, candidate := range enum {
			if reflect.DeepEqual(candidate, value) {
				found = true
				break
			}
		}
		if !found {
			report("value %v is not in enum %v", value, enum)
		}
	}
	if text, ok := value.(string); ok {
		if minLength, ok := numberValue(node["minLength"]); ok && len(text) < int(minLength) {
			report("length %d is less than minLength %d", len(text), int(minLength))
		}
		if pattern, ok := node["pattern"].(string); ok {
			compiled, err := regexp.Compile(pattern)
			switch {
			case err != nil:
				report("schema pattern %q is invalid: %v", pattern, err)
			case !compiled.MatchString(text):
				report("value %q does not match pattern %q", text, pattern)
			}
		}
	}
	if object, ok := value.(map[string]any); ok {
		if required, ok := node["required"].([]any); ok {
			for _, rawName := range required {
				name, ok := rawName.(string)
				if !ok {
					continue
				}
				if _, present := object[name]; !present {
					report("missing required property %q", name)
				}
			}
		}
		properties, _ := node["properties"].(map[string]any)
		for name, subSchema := range properties {
			if child, present := object[name]; present {
				problems = append(problems, validateJSONSchema(subSchema, root, child, path+"."+name)...)
			}
		}
		if additional, declared := node["additionalProperties"]; declared {
			for name, child := range object {
				if _, known := properties[name]; known {
					continue
				}
				switch typed := additional.(type) {
				case bool:
					if !typed {
						report("unexpected property %q", name)
					}
				case map[string]any:
					problems = append(problems, validateJSONSchema(typed, root, child, path+"."+name)...)
				}
			}
		}
	}
	if array, ok := value.([]any); ok {
		if items, ok := node["items"].(map[string]any); ok {
			for index, item := range array {
				problems = append(problems, validateJSONSchema(items, root, item, fmt.Sprintf("%s[%d]", path, index))...)
			}
		}
		if minItems, ok := numberValue(node["minItems"]); ok && len(array) < int(minItems) {
			report("length %d is less than minItems %d", len(array), int(minItems))
		}
		if maxItems, ok := numberValue(node["maxItems"]); ok && len(array) > int(maxItems) {
			report("length %d is greater than maxItems %d", len(array), int(maxItems))
		}
	}
	if number, ok := numberValue(value); ok {
		if minimum, ok := numberValue(node["minimum"]); ok && number < minimum {
			report("value %v is less than minimum %v", number, minimum)
		}
		if maximum, ok := numberValue(node["maximum"]); ok && number > maximum {
			report("value %v is greater than maximum %v", number, maximum)
		}
	}
	return problems
}

func requireSchemaValid(t *testing.T, schema any, value any, label string) {
	t.Helper()
	if problems := validateJSONSchema(schema, schema, value, "$"); len(problems) > 0 {
		t.Fatalf("%s fails schema validation:\n  %s", label, strings.Join(problems, "\n  "))
	}
}

func matchesJSONType(rawType any, value any) bool {
	switch typed := rawType.(type) {
	case string:
		return matchesJSONTypeName(typed, value)
	case []any:
		for _, rawName := range typed {
			if name, ok := rawName.(string); ok && matchesJSONTypeName(name, value) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func matchesJSONTypeName(name string, value any) bool {
	switch name {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		_, ok := numberValue(value)
		return ok
	case "integer":
		number, ok := numberValue(value)
		return ok && number == math.Trunc(number)
	default:
		return true
	}
}

func jsonValueType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64, json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

// resolveLocalRef resolves the #/$defs/<name> references emitted for recursive
// schema types.
func resolveLocalRef(root any, ref string) (any, bool) {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, false
	}
	rootObject, ok := root.(map[string]any)
	if !ok {
		return nil, false
	}
	defs, ok := rootObject["$defs"].(map[string]any)
	if !ok {
		return nil, false
	}
	schema, ok := defs[strings.TrimPrefix(ref, prefix)]
	return schema, ok
}
