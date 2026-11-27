package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEmptyStoreProxyStatusReturnsNotRunning(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "proxy", "status")

	requireExit(t, got, 69)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "NOT_RUNNING" {
		t.Errorf("error.code = %v, want NOT_RUNNING", errObj["code"])
	}
	if errObj["retryable"] != true {
		t.Errorf("error.retryable = %v, want true", errObj["retryable"])
	}
	if hint, _ := errObj["hint"].(string); !strings.Contains(hint, "mitt proxy start") {
		t.Errorf("error.hint = %q, want an actionable proxy start hint", hint)
	}
}

func TestProxyStatusDoesNotCreateStore(t *testing.T) {
	env := newTestEnv(t)

	got := env.runWithStore(t, "proxy", "status")
	requireExit(t, got, 69)

	if _, err := os.Stat(env.store); !os.IsNotExist(err) {
		t.Fatalf("read-only status must not create the store, stat err = %v", err)
	}
}

func TestDefaultStoreIsHomeMitt(t *testing.T) {
	env := newTestEnv(t)

	got := env.run(t, "proxy", "status")
	requireExit(t, got, 69)

	errObj := errorEnvelope(t, got)
	details, _ := errObj["details"].(map[string]any)
	if details["store"] != filepath.Join(env.home, ".mitt") {
		t.Errorf("default store = %v, want %v", details["store"], filepath.Join(env.home, ".mitt"))
	}
	if _, err := os.Stat(filepath.Join(env.home, ".mitt")); !os.IsNotExist(err) {
		t.Fatalf("status must not create ~/.mitt, stat err = %v", err)
	}
}

func TestDefaultOutputIsJSON(t *testing.T) {
	env := newTestEnv(t)

	got := env.run(t, "spec")
	requireExit(t, got, 0)

	doc := decodeJSONObject(t, got.stdout)
	if doc["name"] != "mitt" {
		t.Errorf("spec.name = %v, want mitt", doc["name"])
	}
}

// TestJSONFlagIsGone pins the removal of the compatibility flag: --json used
// to mean "format", "detail", "event stream", and "dry-run", so it is now an
// unknown flag rather than a fourth way to select a shape.
func TestJSONFlagIsGone(t *testing.T) {
	env := newTestEnv(t)

	got := env.run(t, "spec", "--json")
	requireExit(t, got, 2)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("error.code = %v, want INVALID_REQUEST", errObj["code"])
	}
	if message, _ := errObj["message"].(string); !strings.Contains(message, "json") {
		t.Errorf("error.message = %q, want the unknown --json flag named", message)
	}
}

func TestObjectRendersHumanFormats(t *testing.T) {
	env := newTestEnv(t)
	env.mustInitCA(t)
	proxy := env.startProxy(t, "--addr", "127.0.0.1:0")
	status := proxy.waitReady(t, env)
	addr, _ := status["addr"].(string)
	t.Cleanup(func() {
		env.runWithStore(t, "proxy", "stop")
		proxy.waitExit(t, 5*time.Second)
	})

	t.Run("json", func(t *testing.T) {
		got := env.runWithStore(t, "proxy", "status")
		requireExit(t, got, 0)
		status := decodeJSONObject(t, got.stdout)
		if status["running"] != true {
			t.Errorf("running = %v, want true", status["running"])
		}
		if status["addr"] != addr {
			t.Errorf("addr = %v, want %s", status["addr"], addr)
		}
	})

	t.Run("yaml", func(t *testing.T) {
		got := env.runWithStore(t, "proxy", "status", "-f", "yaml")
		requireExit(t, got, 0)
		for _, want := range []string{`running: true`, `addr:`} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("yaml output missing %q:\n%s", want, got.stdout)
			}
		}
		if !strings.Contains(got.stdout, addr) {
			t.Errorf("yaml output missing bound addr %q:\n%s", addr, got.stdout)
		}
	})

	t.Run("table", func(t *testing.T) {
		got := env.runWithStore(t, "proxy", "status", "-f", "table")
		requireExit(t, got, 0)
		for _, want := range []string{"KEY", "VALUE", "running", "true"} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("table output missing %q:\n%s", want, got.stdout)
			}
		}
	})

	t.Run("markdown", func(t *testing.T) {
		got := env.runWithStore(t, "proxy", "status", "-f", "md")
		requireExit(t, got, 0)
		for _, want := range []string{"| KEY | VALUE |", "| --- | --- |", "| running | true |"} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("markdown output missing %q:\n%s", want, got.stdout)
			}
		}
	})
}

func TestStreamRendersHumanFormats(t *testing.T) {
	env := newTestEnv(t)

	t.Run("table", func(t *testing.T) {
		got := env.run(t, "spec", "-f", "table")
		requireExit(t, got, 0)
		for _, want := range []string{"PATH", "FLAGS", "mitt proxy status", "NOT_RUNNING"} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("table output missing %q:\n%s", want, got.stdout)
			}
		}
	})

	t.Run("markdown", func(t *testing.T) {
		got := env.run(t, "spec", "-f", "md")
		requireExit(t, got, 0)
		for _, want := range []string{"| PATH | FLAGS |", "| --- | --- |", "mitt proxy status"} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("markdown output missing %q:\n%s", want, got.stdout)
			}
		}
	})

	t.Run("yaml", func(t *testing.T) {
		got := env.run(t, "spec", "-f", "yaml")
		requireExit(t, got, 0)
		for _, want := range []string{`path: "mitt"`, `path: "mitt proxy status"`} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("yaml output missing %q:\n%s", want, got.stdout)
			}
		}
	})
}

func TestUsageErrorsExitTwo(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"spec", "--bogus"}},
		{name: "invalid format enum", args: []string{"spec", "-f", "xml"}},
		{name: "invalid format yaml typo", args: []string{"spec", "-f", "yml"}},
		{name: "unknown command", args: []string{"bogus"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := env.run(t, tc.args...)
			requireExit(t, got, 2)
			errObj := errorEnvelope(t, got)
			if errObj["code"] != "INVALID_REQUEST" {
				t.Errorf("error.code = %v, want INVALID_REQUEST", errObj["code"])
			}
		})
	}
}

func TestExplicitJSONFormatIsAccepted(t *testing.T) {
	env := newTestEnv(t)

	got := env.run(t, "spec", "-f", "json")
	requireExit(t, got, 0)
	if !json.Valid([]byte(got.stdout)) {
		t.Fatalf("-f json must produce JSON, got:\n%s", got.stdout)
	}
}

func TestErrorsStayJSONForEveryOutputFormat(t *testing.T) {
	env := newTestEnv(t)

	for _, format := range []string{"table", "yaml", "md"} {
		t.Run(format, func(t *testing.T) {
			got := env.runWithStore(t, "proxy", "status", "-f", format)
			requireExit(t, got, 69)
			errObj := errorEnvelope(t, got)
			if errObj["code"] != "NOT_RUNNING" {
				t.Errorf("error.code = %v, want NOT_RUNNING", errObj["code"])
			}
		})
	}
}

// TestSummaryModeIsSelectedByRowFormats proves the spec summary is a declared
// shape rather than a second renderer: -f table and -f md select it, while JSON
// and YAML keep the full document.
func TestSummaryModeIsSelectedByRowFormats(t *testing.T) {
	env := newTestEnv(t)

	table := env.run(t, "spec", "-f", "table")
	requireExit(t, table, 0)
	if !strings.Contains(table.stdout, "PATH") {
		t.Fatalf("spec -f table must render the summary rows, got:\n%s", table.stdout)
	}
	doc := env.run(t, "spec")
	requireExit(t, doc, 0)
	if !strings.Contains(doc.stdout, `"constraints"`) {
		t.Fatalf("spec must default to the full document, got:\n%s", doc.stdout)
	}
	if strings.Contains(table.stdout, `"constraints"`) {
		t.Fatal("the table summary must not carry the full document")
	}
}
