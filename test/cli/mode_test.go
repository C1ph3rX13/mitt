package cli_test

import "testing"

// TestOutputModesAreDeclaredAndSelectable is the contract-level check for the
// single output-mode owner: every mode a command can emit is published, and the
// invocation that selects it produces the shape that mode's schema describes.
func TestOutputModesAreDeclaredAndSelectable(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	cases := []struct {
		path   string
		mode   string
		flag   string
		stream bool
	}{
		{path: "mitt show", mode: "detail", flag: "--detail"},
		{path: "mitt show", mode: "raw", flag: "--raw"},
		{path: "mitt proxy start", mode: "events", flag: "--events", stream: true},
		{path: "mitt send", mode: "dry-run", flag: "--dry-run"},
		{path: "mitt send", mode: "schema", flag: "--schema"},
		{path: "mitt spec", mode: "summary", stream: true},
	}
	for _, tc := range cases {
		t.Run(tc.path+"/"+tc.mode, func(t *testing.T) {
			command, ok := doc.Command.find(tc.path)
			if !ok {
				t.Fatalf("spec missing %q", tc.path)
			}
			output, ok := command.OutputSchemas[tc.mode]
			if !ok {
				t.Fatalf("%s publishes no %q mode; got %v", tc.path, tc.mode, command.OutputSchemas)
			}
			if output.Flag != tc.flag {
				t.Errorf("%s %s selector = %q, want %q", tc.path, tc.mode, output.Flag, tc.flag)
			}
			if output.Stream != tc.stream {
				t.Errorf("%s %s stream = %t, want %t", tc.path, tc.mode, output.Stream, tc.stream)
			}
			if output.Opaque {
				if output.Description == "" {
					t.Errorf("%s %s opaque mode needs a description", tc.path, tc.mode)
				}
				return
			}
			if output.Schema == nil {
				t.Errorf("%s %s mode publishes no schema", tc.path, tc.mode)
			}
		})
	}
}

// TestSemanticModesAreRejectedByWrongFormat proves the mode resolution rejects
// combinations it cannot render instead of silently ignoring --format.
func TestSemanticModesAreRejectedByWrongFormat(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)

	t.Run("live event stream needs JSON", func(t *testing.T) {
		got := env.runWithStore(t, "proxy", "start", "--events", "-f", "table", "--addr", "127.0.0.1:0")
		requireExit(t, got, 2)
		errObj := errorEnvelope(t, got)
		if errObj["code"] != "INVALID_REQUEST" {
			t.Errorf("error.code = %v, want INVALID_REQUEST", errObj["code"])
		}
		details, _ := errObj["details"].(map[string]any)
		if details["mode"] != "events" {
			t.Errorf("details.mode = %v, want events", details["mode"])
		}
	})

	t.Run("raw bytes need a format-free invocation", func(t *testing.T) {
		got := env.runWithStore(t, "show", "entry", "--raw", "-f", "table")
		requireExit(t, got, 2)
		errObj := errorEnvelope(t, got)
		details, _ := errObj["details"].(map[string]any)
		if details["mode"] != "raw" {
			t.Errorf("details.mode = %v, want raw", details["mode"])
		}
	})
}

// TestDetailModeShapeIsNotTheSummary pins the split between the two show
// shapes: the default summary never carries detail-only keys, and --detail
// always carries all three.
func TestDetailModeShapeIsNotTheSummary(t *testing.T) {
	env := newTestEnv(t)
	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	rawRequest := "POST http://{{target}}/shape HTTP/1.1\r\nHost: {{target}}\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi"

	id, _, _ := captureEntry(t, env, targetResponse, rawRequest)

	summary := decodeJSONObject(t, env.runWithStore(t, "show", id).stdout)
	detail := decodeJSONObject(t, env.runWithStore(t, "show", id, "--detail").stdout)

	detailKeys := []string{"request_body_path", "response_body_path", "request_document"}
	for _, key := range detailKeys {
		if _, ok := summary[key]; ok {
			t.Errorf("default summary carries detail key %q", key)
		}
		if _, ok := detail[key]; !ok {
			t.Errorf("--detail is missing %q: %v", key, detail)
		}
	}
	if summary["id"] != detail["id"] || summary["method"] != detail["method"] {
		t.Errorf("the two shapes disagree on shared fields: %v vs %v", summary, detail)
	}
}
