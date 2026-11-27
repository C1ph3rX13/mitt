package spec

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

const draft202012 = "https://json-schema.org/draft/2020-12/schema"

// SchemaOf derives the draft 2020-12 JSON Schema for the JSON encoding of T.
//
// Field names, property order, and required properties come from the json
// tags. Descriptions and validation keywords come from the desc and schema
// tags. A pointer field is rendered as its element type; add
// schema:"nullable" when the field can also emit JSON null.
//
// The derived schema describes the output DTO itself, so a field cannot drift
// from the contract without the compiler and this generator disagreeing. The
// one shape Go cannot express statically is a nil slice, map, or pointer: the
// end-to-end schema validation test catches an output that emits null where
// the declaration says otherwise.
func SchemaOf[T any](slug, title string) map[string]any {
	state := newSchemaState()
	t := reflect.TypeFor[T]()
	state.markRecursive(t)
	schema := state.typeSchema(t)
	schema["$schema"] = draft202012
	schema["$id"] = "https://mitt.dev/schema/" + slug + ".json"
	schema["title"] = title
	if len(state.defs) > 0 {
		schema["$defs"] = state.defs
	}
	return schema
}

// Describe sets a schema's top-level description and returns it.
func Describe(schema map[string]any, text string) map[string]any {
	if text != "" {
		schema["description"] = text
	}
	return schema
}

// RestrictEnum narrows a property's enum to the values one command can emit.
// A shared type carries the full vocabulary of its domain — every event kind
// the store writes — but a command that can only produce a subset must not
// publish the rest. Each value must already belong to the derived enum, so a
// restriction can shrink a vocabulary but never invent a value or disagree
// with the type it came from.
func RestrictEnum(schema map[string]any, name string, values []string) {
	value := property(schema, "RestrictEnum", name)
	full, ok := value["enum"].([]string)
	if !ok {
		panic(fmt.Sprintf("spec: RestrictEnum %q requires a property with a derived enum, got %v", name, value["enum"]))
	}
	allowed := make(map[string]bool, len(full))
	for _, candidate := range full {
		allowed[candidate] = true
	}
	for _, candidate := range values {
		if !allowed[candidate] {
			panic(fmt.Sprintf("spec: RestrictEnum %q value %q is not in the derived vocabulary %v", name, candidate, full))
		}
	}
	value["enum"] = values
}

// property returns one declared property of an object schema, panicking when it
// does not exist. Every narrowing helper goes through it, so a command that
// names a field the shared type does not have fails at declaration time.
func property(schema map[string]any, helper, name string) map[string]any {
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		panic("spec: " + helper + " on a schema that is not an object")
	}
	value, ok := properties[name].(map[string]any)
	if !ok {
		panic(fmt.Sprintf("spec: %s %q is not a declared property", helper, name))
	}
	return value
}

// RequireNonNull narrows a nullable property to its non-null type. A shared
// type says a value may be absent (a mitm_error has no status); a command that
// cannot produce that case publishes the stronger guarantee.
func RequireNonNull(schema map[string]any, name string) {
	value := property(schema, "RequireNonNull", name)
	switch types := value["type"].(type) {
	case string:
		panic(fmt.Sprintf("spec: RequireNonNull %q is already non-null (%s)", name, types))
	case []string:
		narrowed := make([]string, 0, len(types))
		for _, candidate := range types {
			if candidate != "null" {
				narrowed = append(narrowed, candidate)
			}
		}
		if len(narrowed) == 0 {
			panic(fmt.Sprintf("spec: RequireNonNull %q has no non-null type in %v", name, types))
		}
		if len(narrowed) == 1 {
			value["type"] = narrowed[0]
			return
		}
		value["type"] = narrowed
	case []any:
		narrowed := make([]any, 0, len(types))
		for _, candidate := range types {
			if candidate != "null" {
				narrowed = append(narrowed, candidate)
			}
		}
		if len(narrowed) == 0 {
			panic(fmt.Sprintf("spec: RequireNonNull %q has no non-null type in %v", name, types))
		}
		if len(narrowed) == 1 {
			value["type"] = narrowed[0]
			return
		}
		value["type"] = narrowed
	default:
		panic(fmt.Sprintf("spec: RequireNonNull %q has no type to narrow: %v", name, value["type"]))
	}
}

// RestrictRange adds numeric bounds to an integer or number property. The
// shared type leaves a value open so every command can report it; a command
// whose value can never fall outside a range says so.
func RestrictRange(schema map[string]any, name string, minimum, maximum int) {
	value := property(schema, "RestrictRange", name)
	kind, _ := value["type"].(string)
	if kind != "integer" && kind != "number" {
		panic(fmt.Sprintf("spec: RestrictRange %q requires a numeric property, got %v", name, value["type"]))
	}
	if minimum > maximum {
		panic(fmt.Sprintf("spec: RestrictRange %q has minimum %d above maximum %d", name, minimum, maximum))
	}
	value["minimum"] = minimum
	value["maximum"] = maximum
}

// RequireProperties marks properties as always present. A shared type marks a
// field omitempty because some kinds omit it; a command whose own outputs never
// omit it publishes the stronger requirement.
func RequireProperties(schema map[string]any, names ...string) {
	required, _ := schema["required"].([]string)
	present := make(map[string]bool, len(required))
	for _, name := range required {
		present[name] = true
	}
	for _, name := range names {
		property(schema, "RequireProperties", name)
		if present[name] {
			continue
		}
		required = append(required, name)
		present[name] = true
	}
	schema["required"] = required
}

// DescribeProperty sets the description of one property of a derived object
// schema. Callers use it when a field's documented values come from a runtime
// vocabulary instead of a struct tag, so the document cannot list values the
// code does not emit. A missing property is a programmer error.
func DescribeProperty(schema map[string]any, name, text string) {
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		panic("spec: DescribeProperty on a schema that is not an object")
	}
	property, ok := properties[name].(map[string]any)
	if !ok {
		panic(fmt.Sprintf("spec: DescribeProperty %q is not a declared property", name))
	}
	if text != "" {
		property["description"] = text
	}
}

type schemaState struct {
	defs      map[string]map[string]any
	defNames  map[string]reflect.Type
	pending   map[reflect.Type]bool
	scanning  map[reflect.Type]bool
	scanned   map[reflect.Type]bool
	recursive map[reflect.Type]bool
}

func newSchemaState() *schemaState {
	return &schemaState{
		defs:      map[string]map[string]any{},
		defNames:  map[string]reflect.Type{},
		pending:   map[reflect.Type]bool{},
		scanning:  map[reflect.Type]bool{},
		scanned:   map[reflect.Type]bool{},
		recursive: map[reflect.Type]bool{},
	}
}

// markRecursive finds the struct types that participate in a reference cycle.
// Only those types need a $defs entry; every other type stays inline so the
// published schemas keep their reviewable shape.
func (s *schemaState) markRecursive(t reflect.Type) {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		s.markRecursive(t.Elem())
	case reflect.Struct:
		if s.scanned[t] || s.recursive[t] {
			return
		}
		if s.scanning[t] {
			s.recursive[t] = true
			return
		}
		s.scanning[t] = true
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !fieldVisible(field) || jsonSkip(field) {
				continue
			}
			s.markRecursive(field.Type)
		}
		delete(s.scanning, t)
		s.scanned[t] = true
	}
}

// enumProvider is implemented by a named string type that has exactly one
// vocabulary. Deriving the enum from the type means a published schema cannot
// list a value the code does not write, and every output that mentions event
// kinds shares the one list instead of carrying its own copy.
type enumProvider interface {
	EnumValues() []string
}

func (s *schemaState) typeSchema(t reflect.Type) map[string]any {
	switch t.Kind() {
	case reflect.Pointer:
		return s.typeSchema(t.Elem())
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.String:
		schema := map[string]any{"type": "string"}
		if provider, ok := reflect.Zero(t).Interface().(enumProvider); ok {
			schema["enum"] = provider.EnumValues()
		}
		return schema
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": s.typeSchema(t.Elem())}
	case reflect.Array:
		return map[string]any{
			"type":     "array",
			"minItems": t.Len(),
			"maxItems": t.Len(),
			"items":    s.typeSchema(t.Elem()),
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			panic(fmt.Sprintf("spec: schema map %s must have string keys", t))
		}
		if isEmptyInterface(t.Elem()) {
			return map[string]any{"type": "object", "additionalProperties": true}
		}
		return map[string]any{"type": "object", "additionalProperties": s.typeSchema(t.Elem())}
	case reflect.Interface:
		return map[string]any{}
	case reflect.Struct:
		if s.recursive[t] {
			s.ensureDef(t)
			return map[string]any{"$ref": "#/$defs/" + s.defName(t)}
		}
		return s.structSchema(t)
	default:
		panic(fmt.Sprintf("spec: unsupported schema type %s", t))
	}
}

func (s *schemaState) ensureDef(t reflect.Type) {
	name := s.defName(t)
	if _, done := s.defs[name]; done {
		return
	}
	if s.pending[t] {
		return
	}
	s.pending[t] = true
	s.defs[name] = s.structSchema(t)
	delete(s.pending, t)
}

func (s *schemaState) defName(t reflect.Type) string {
	name := t.Name()
	if name == "" {
		panic("spec: a recursive schema type must be named")
	}
	if existing, ok := s.defNames[name]; ok && existing != t {
		panic(fmt.Sprintf("spec: two schema types are named %s", name))
	}
	s.defNames[name] = t
	return name
}

func (s *schemaState) structSchema(t reflect.Type) map[string]any {
	properties := map[string]any{}
	required := []string{}
	groups := map[string][]string{}
	groupOrder := []string{}
	clauses := []any{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !fieldVisible(field) || jsonSkip(field) {
			continue
		}
		name, optional := jsonField(field)
		if field.Anonymous && name == "" && deref(field.Type).Kind() == reflect.Struct {
			embedded := s.structSchema(deref(field.Type))
			embeddedProperties, ok := embedded["properties"].(map[string]any)
			if !ok {
				panic(fmt.Sprintf("spec: embedded %s is not an object schema", field.Type))
			}
			for key, value := range embeddedProperties {
				properties[key] = value
			}
			if names, ok := embedded["required"].([]string); ok {
				required = append(required, names...)
			}
			if embeddedClauses, ok := embedded["allOf"].([]any); ok {
				clauses = append(clauses, embeddedClauses...)
			}
			continue
		}
		if name == "" {
			name = field.Name
		}
		property := s.typeSchema(deref(field.Type))
		group := applyFieldMeta(property, field)
		properties[name] = property
		if group != "" {
			if _, seen := groups[group]; !seen {
				groupOrder = append(groupOrder, group)
			}
			groups[group] = append(groups[group], name)
		}
		if !optional {
			required = append(required, name)
		}
	}
	clauses = append(clauses, atMostOneClauses(groups, groupOrder, properties)...)
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	if len(clauses) > 0 {
		schema["allOf"] = clauses
	}
	return schema
}

// atMostOneClauses renders each at_most_one field group as the pairwise
// negation of "both present with a non-null value". The count is the same
// rule the domain enforces at runtime; publishing it as allOf/not keeps the
// schema inside the keyword set every draft 2020-12 validator implements.
func atMostOneClauses(groups map[string][]string, order []string, properties map[string]any) []any {
	clauses := []any{}
	for _, group := range order {
		names := groups[group]
		if len(names) < 2 {
			panic(fmt.Sprintf("spec: at_most_one group %q needs at least two members", group))
		}
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				clauses = append(clauses, map[string]any{
					"not": map[string]any{
						"required": []string{names[i], names[j]},
						"properties": map[string]any{
							names[i]: map[string]any{"type": nonNullTypeOf(memberSchema(properties, group, names[i]))},
							names[j]: map[string]any{"type": nonNullTypeOf(memberSchema(properties, group, names[j]))},
						},
					},
				})
			}
		}
	}
	return clauses
}

// memberSchema returns the declared property schema of one group member. A
// group naming a field the object does not have is a programmer error, not a
// constraint that silently never fires.
func memberSchema(properties map[string]any, group, name string) map[string]any {
	property, ok := properties[name].(map[string]any)
	if !ok {
		panic(fmt.Sprintf("spec: at_most_one group %q names undeclared property %q", group, name))
	}
	return property
}

// nonNullTypeOf reduces a property's declared type to the types a non-null
// value can have, so a pairwise clause can say "present with a value" without
// repeating the property's other keywords.
func nonNullTypeOf(value map[string]any) any {
	switch types := value["type"].(type) {
	case string:
		return types
	case []string:
		return narrowedType(types, value["type"])
	case []any:
		narrowed := make([]any, 0, len(types))
		for _, candidate := range types {
			if candidate != "null" {
				narrowed = append(narrowed, candidate)
			}
		}
		if len(narrowed) == 0 {
			panic(fmt.Sprintf("spec: at_most_one member has no non-null type: %v", types))
		}
		if len(narrowed) == 1 {
			return narrowed[0]
		}
		return narrowed
	default:
		panic(fmt.Sprintf("spec: at_most_one member has no type to compare: %v", value["type"]))
	}
}

func narrowedType(types []string, raw any) any {
	narrowed := make([]string, 0, len(types))
	for _, candidate := range types {
		if candidate != "null" {
			narrowed = append(narrowed, candidate)
		}
	}
	if len(narrowed) == 0 {
		panic(fmt.Sprintf("spec: at_most_one member has no non-null type: %v", raw))
	}
	if len(narrowed) == 1 {
		return narrowed[0]
	}
	return narrowed
}

// applyFieldMeta applies one field's schema tags and returns the at_most_one
// group it belongs to, if any. The group is a constraint on the object, not on
// the field, so structSchema collects it and emits the clause once per pair.
func applyFieldMeta(schema map[string]any, field reflect.StructField) (atMostOne string) {
	if description := field.Tag.Get("desc"); description != "" {
		schema["description"] = description
	}
	rules := field.Tag.Get("schema")
	if rules == "" {
		return ""
	}
	for _, rule := range strings.Split(rules, ",") {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if rule == "open" {
			schema["additionalProperties"] = true
			continue
		}
		if rule == "nullable" {
			schema["type"] = nullableType(schema["type"])
			continue
		}
		key, value, ok := strings.Cut(rule, "=")
		if !ok {
			panic(fmt.Sprintf("spec: schema rule %q must be key=value", rule))
		}
		switch key {
		case "enum":
			schema["enum"] = enumValues(value)
		case "item_enum":
			items, ok := schema["items"].(map[string]any)
			if !ok {
				panic("spec: item_enum requires an array field")
			}
			items["enum"] = enumValues(value)
		case "format":
			schema["format"] = value
		case "pattern":
			schema["pattern"] = value
		case "encoding":
			schema["contentEncoding"] = value
		case "min":
			schema["minimum"] = schemaNumber(value)
		case "max":
			schema["maximum"] = schemaNumber(value)
		case "minlen":
			schema["minLength"] = schemaNumber(value)
		case "value_min":
			additional, ok := schema["additionalProperties"].(map[string]any)
			if !ok {
				panic("spec: value_min requires a map field")
			}
			additional["minimum"] = schemaNumber(value)
		case "at_most_one":
			if value == "" {
				panic(fmt.Sprintf("spec: at_most_one on %s needs a group name", field.Name))
			}
			atMostOne = value
		default:
			panic(fmt.Sprintf("spec: unknown schema rule %q", key))
		}
	}
	return atMostOne
}

func enumValues(raw string) []string {
	values := strings.Split(raw, "|")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	return values
}

func schemaNumber(raw string) any {
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return value
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		panic(fmt.Sprintf("spec: schema number %q: %v", raw, err))
	}
	return value
}

func nullableType(raw any) any {
	switch typed := raw.(type) {
	case string:
		return []string{typed, "null"}
	case []string:
		for _, name := range typed {
			if name == "null" {
				return typed
			}
		}
		return append(typed, "null")
	default:
		panic(fmt.Sprintf("spec: nullable requires a type, got %v", raw))
	}
}

func jsonField(field reflect.StructField) (string, bool) {
	name, rest, _ := strings.Cut(field.Tag.Get("json"), ",")
	optional := strings.Contains(","+rest+",", ",omitempty,")
	return name, optional
}

func jsonSkip(field reflect.StructField) bool {
	return field.Tag.Get("json") == "-"
}

// fieldVisible mirrors encoding/json: unexported fields are skipped, except
// that an anonymous unexported struct still promotes its exported fields.
func fieldVisible(field reflect.StructField) bool {
	if field.PkgPath == "" {
		return true
	}
	return field.Anonymous && deref(field.Type).Kind() == reflect.Struct
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func isEmptyInterface(t reflect.Type) bool {
	return t.Kind() == reflect.Interface && t.NumMethod() == 0
}
