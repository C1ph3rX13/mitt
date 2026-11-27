package spec_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/diff"
	"github.com/C1ph3rX13/mitt/internal/entry"
	"github.com/C1ph3rX13/mitt/internal/session"
	"github.com/C1ph3rX13/mitt/internal/skillinstall"
	"github.com/C1ph3rX13/mitt/internal/spec"
)

type sampleEmbedded struct {
	Embedded string `json:"embedded"`
}

type sampleStruct struct {
	ID       string         `json:"id"`
	Optional string         `json:"optional,omitempty"`
	Count    int            `json:"count" schema:"min=0,max=10"`
	Kind     string         `json:"kind" schema:"enum=a|b"`
	Tags     []string       `json:"tags" schema:"item_enum=x|y"`
	Fixed    [2]string      `json:"fixed"`
	Meta     map[string]any `json:"meta,omitempty"`
	Status   *int           `json:"status" schema:"nullable"`
	Note     string         `json:"note" desc:"a note"`
	Secret   string         `json:"-"`
	sampleEmbedded
}

type recursiveNode struct {
	Name     string          `json:"name"`
	Children []recursiveNode `json:"children"`
}

func TestSchemaOfDerivesStructShape(t *testing.T) {
	got := spec.SchemaOf[sampleStruct]("sample", "sample")
	want := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  "https://mitt.dev/schema/sample.json",
		"title":                "sample",
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"id", "count", "kind", "tags", "fixed", "status", "note", "embedded",
		},
		"properties": map[string]any{
			"id":       map[string]any{"type": "string"},
			"optional": map[string]any{"type": "string"},
			"count":    map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
			"kind":     map[string]any{"type": "string", "enum": []string{"a", "b"}},
			"tags": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string", "enum": []string{"x", "y"}},
			},
			"fixed": map[string]any{
				"type":     "array",
				"minItems": 2,
				"maxItems": 2,
				"items":    map[string]any{"type": "string"},
			},
			"meta":   map[string]any{"type": "object", "additionalProperties": true},
			"status": map[string]any{"type": []string{"integer", "null"}},
			"note":   map[string]any{"type": "string", "description": "a note"},
			"embedded": map[string]any{
				"type": "string",
			},
		},
	}
	assertJSONEqual(t, got, want)
}

func TestSchemaOfUsesDefsOnlyForRecursiveTypes(t *testing.T) {
	got := spec.SchemaOf[recursiveNode]("node", "node")
	want := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://mitt.dev/schema/node.json",
		"title":   "node",
		"$ref":    "#/$defs/recursiveNode",
		"$defs": map[string]any{
			"recursiveNode": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"name", "children"},
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
					"children": map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/$defs/recursiveNode"},
					},
				},
			},
		},
	}
	assertJSONEqual(t, got, want)
}

func TestSpecEnumsStayInSyncWithDomainConstants(t *testing.T) {
	assertEnum(t, spec.SchemaOf[session.Event]("event", "event"), "kind", session.Kinds)
	assertEnum(t, spec.SchemaOf[entry.ListRow]("list-row", "list row"), "kind", session.Kinds)
	assertEnum(t, spec.SchemaOf[entry.View]("view", "view"), "kind", session.Kinds)
	assertEnum(t, spec.SchemaOf[diff.Entry]("diff-entry", "diff entry"), "kind", session.Kinds)
	assertEnum(t, spec.SchemaOf[diff.Body]("body", "body"), "type", []string{diff.BodyTypeText, diff.BodyTypeBinary})
	assertEnum(t, spec.SchemaOf[skillinstall.Result]("result", "result"), "action", []string{
		string(skillinstall.Installed),
		string(skillinstall.Updated),
		string(skillinstall.Unchanged),
	})
}

// TestKindEnumComesFromTheTypeNotFromATag pins the mechanism behind the single
// kind vocabulary: no output struct carries a hand-written enum tag for kind,
// so the schema and the store cannot disagree about the values.
func TestKindEnumComesFromTheTypeNotFromATag(t *testing.T) {
	schema := spec.SchemaOf[entry.View]("view", "view")
	properties, _ := schema["properties"].(map[string]any)
	kind, _ := properties["kind"].(map[string]any)
	if kind["type"] != "string" {
		t.Errorf("kind type = %v, want string", kind["type"])
	}
	assertEnum(t, schema, "kind", session.Kinds)
}

// TestRestrictEnumNarrowsOnlyWithinTheDerivedVocabulary pins both halves of the
// narrowing rule: a command publishes only the kinds it can emit, and a value
// the type does not declare is a programmer error rather than a schema listing
// a value the code never writes.
func TestRestrictEnumNarrowsOnlyWithinTheDerivedVocabulary(t *testing.T) {
	schema := spec.SchemaOf[entry.View]("view", "view")
	spec.RestrictEnum(schema, "kind", []string{string(session.KindSent), string(session.KindReplayed)})
	assertEnum(t, schema, "kind", []string{string(session.KindSent), string(session.KindReplayed)})

	t.Run("unknown value", func(t *testing.T) {
		fresh := spec.SchemaOf[entry.View]("view", "view")
		defer func() {
			if recover() == nil {
				t.Fatal("RestrictEnum accepted a value outside the derived vocabulary")
			}
		}()
		spec.RestrictEnum(fresh, "kind", []string{"sent", "exploded"})
	})
}

// TestRestrictEnumRequiresADerivedEnum refuses to narrow a property that has no
// vocabulary of its own: narrowing a free-form string would publish an enum the
// type never claimed.
func TestRestrictEnumRequiresADerivedEnum(t *testing.T) {
	schema := spec.SchemaOf[entry.View]("view", "view")
	defer func() {
		if recover() == nil {
			t.Fatal("RestrictEnum on a plain string property did not panic")
		}
	}()
	spec.RestrictEnum(schema, "url", []string{"http://example.test/"})
}

// TestNarrowingHelpersProduceTheStrongerContract pins the three ways a command
// tightens the shared view: a nullable field it always sets, a numeric range its
// values always fall in, and fields it never omits.
func TestNarrowingHelpersProduceTheStrongerContract(t *testing.T) {
	schema := spec.SchemaOf[entry.View]("view", "view")
	spec.RequireNonNull(schema, "status")
	spec.RestrictRange(schema, "status", 100, 999)
	spec.RequireProperties(schema, "request_line", "response_line")

	properties, _ := schema["properties"].(map[string]any)
	status, _ := properties["status"].(map[string]any)
	if status["type"] != "integer" {
		t.Errorf("status type = %v, want integer", status["type"])
	}
	if status["minimum"] != 100 || status["maximum"] != 999 {
		t.Errorf("status bounds = %v..%v, want 100..999", status["minimum"], status["maximum"])
	}
	required, _ := schema["required"].([]string)
	for _, name := range []string{"request_line", "response_line"} {
		if !containsString(required, name) {
			t.Errorf("required = %v, want %s", required, name)
		}
	}
}

func TestNarrowingHelpersRejectWrongShapes(t *testing.T) {
	cases := map[string]func(map[string]any){
		"RequireNonNull on a plain string": func(schema map[string]any) {
			spec.RequireNonNull(schema, "url")
		},
		"RequireNonNull on a missing property": func(schema map[string]any) {
			spec.RequireNonNull(schema, "absent")
		},
		"RestrictRange on a string": func(schema map[string]any) {
			spec.RestrictRange(schema, "url", 1, 2)
		},
		"RestrictRange with inverted bounds": func(schema map[string]any) {
			spec.RestrictRange(schema, "req_body_size", 10, 1)
		},
		"RequireProperties on a missing property": func(schema map[string]any) {
			spec.RequireProperties(schema, "absent")
		},
	}
	for name, narrow := range cases {
		t.Run(name, func(t *testing.T) {
			schema := spec.SchemaOf[entry.View]("view", "view")
			defer func() {
				if recover() == nil {
					t.Fatalf("%s did not panic", name)
				}
			}()
			narrow(schema)
		})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRequestDocumentBodiesAreMutuallyOptional(t *testing.T) {
	schema := spec.RequestDocumentSchema()
	properties := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]string)
	for _, name := range []string{"body", "body_base64", "body_file"} {
		for _, requiredName := range required {
			if requiredName == name {
				t.Errorf("%s must not be required", name)
			}
		}
		if _, ok := properties[name]; !ok {
			t.Errorf("%s must be declared", name)
		}
	}
}

// TestRequestDocumentPublishesTheBodyRuleItEnforces pins the schema half of
// the request-document deepening: the one-body rule is not just prose in the
// description, it is an executable allOf of pairwise "not both" clauses, and
// its members are exactly the document's body vocabulary. A field added to
// entry.BodyFields without joining the schema group (or the reverse) fails
// here.
func TestRequestDocumentPublishesTheBodyRuleItEnforces(t *testing.T) {
	schema := spec.RequestDocumentSchema()
	clauses, ok := schema["allOf"].([]any)
	if !ok || len(clauses) == 0 {
		t.Fatalf("request document schema publishes no executable body rule: %v", schema["allOf"])
	}

	members := map[string]bool{}
	for _, raw := range clauses {
		clause, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("allOf entry = %T, want an object", raw)
		}
		forbidden, ok := clause["not"].(map[string]any)
		if !ok {
			t.Fatalf("allOf entry has no not clause: %v", clause)
		}
		required, ok := forbidden["required"].([]string)
		if !ok || len(required) != 2 {
			t.Fatalf("not clause = %v, want a pair of required fields", forbidden)
		}
		for _, name := range required {
			members[name] = true
		}
	}

	want := entry.BodyFieldNames()
	if len(members) != len(want) {
		t.Fatalf("schema body rule covers %v, want exactly %v", members, want)
	}
	for _, name := range want {
		if !members[name] {
			t.Errorf("schema body rule does not cover %q; members = %v", name, members)
		}
	}
}

// TestAtMostOneRequiresTwoDistinctMembers refuses tag combinations that would
// publish a rule with nothing to constrain: one member (never violated) or an
// empty group name (not a group).
func TestAtMostOneRequiresTwoDistinctMembers(t *testing.T) {
	type oneMember struct {
		Body string `json:"body" schema:"at_most_one=body"`
	}
	type unnamed struct {
		Body string `json:"body" schema:"at_most_one="`
		File string `json:"file" schema:"at_most_one="`
	}
	cases := map[string]func(){
		"one member":  func() { spec.SchemaOf[oneMember]("one", "one") },
		"empty group": func() { spec.SchemaOf[unnamed]("unnamed", "unnamed") },
	}
	for name, derive := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s did not panic", name)
				}
			}()
			derive()
		})
	}
}

func assertEnum(t *testing.T, schema map[string]any, field string, want []string) {
	t.Helper()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties: %v", schema)
	}
	property, ok := properties[field].(map[string]any)
	if !ok {
		t.Fatalf("schema has no %s property", field)
	}
	raw, ok := property["enum"].([]string)
	if !ok {
		t.Fatalf("%s enum = %v, want %v", field, property["enum"], want)
	}
	if !reflect.DeepEqual(raw, want) {
		t.Errorf("%s enum = %v, want %v", field, raw, want)
	}
}

func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	normalizedGot := roundTripJSON(t, got)
	normalizedWant := roundTripJSON(t, want)
	if !reflect.DeepEqual(normalizedGot, normalizedWant) {
		gotJSON, _ := json.MarshalIndent(normalizedGot, "", "  ")
		t.Fatalf("schema mismatch\n got: %s\nwant: %v", gotJSON, normalizedWant)
	}
}

func roundTripJSON(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}
