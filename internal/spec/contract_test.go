package spec_test

import (
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/spec"
	"github.com/spf13/cobra"
)

func TestDeclareProjectsOneContract(t *testing.T) {
	root, cmd := newDemoCommand()
	spec.Declare(cmd, spec.Contract{
		Args:  []spec.Positional{{Name: "id", Required: true}},
		Flags: []spec.FlagRule{{Name: "kind", Enum: []string{"a", "b"}}},
		Constraints: []spec.Constraint{
			spec.Alone("kind-alone", output.CodeInvalidRequest, "--kind"),
		},
		Errors: []string{output.CodeInternalError},
		Outputs: []spec.Output{
			spec.Schema[sampleStruct]("demo", "demo"),
			spec.Schema[sampleStruct]("demo-dry", "demo --dry").
				Named("dry").
				WhenFlag("--detail").
				PublishAs("demo_dry"),
		},
	})

	document := spec.Build(root)
	if len(document.Command.Children) != 1 {
		t.Fatalf("children = %d, want 1", len(document.Command.Children))
	}
	node := document.Command.Children[0]

	wantArgs := []spec.Positional{{Name: "id", Required: true}}
	if !reflect.DeepEqual(node.Args, wantArgs) {
		t.Errorf("args = %+v, want %+v", node.Args, wantArgs)
	}
	flag, ok := flagNamed(node.Flags, "kind")
	if !ok {
		t.Fatal("flags missing kind")
	}
	if !reflect.DeepEqual(flag.Enum, []string{"a", "b"}) {
		t.Errorf("kind enum = %v", flag.Enum)
	}
	if !reflect.DeepEqual(node.ErrorCodes, []string{output.CodeInternalError}) {
		t.Errorf("error codes = %v", node.ErrorCodes)
	}
	defaultSchema, ok := node.OutputSchema.(map[string]any)
	if !ok {
		t.Fatalf("output_schema = %v", node.OutputSchema)
	}
	if id := defaultSchema["$id"]; id != "https://mitt.dev/schema/demo.json" {
		t.Errorf("default output $id = %v", id)
	}
	dryRun, ok := node.OutputSchemas["dry"]
	if !ok {
		t.Fatalf("output_schemas[dry] = %v", node.OutputSchemas)
	}
	if id := dryRun.Schema["$id"]; id != "https://mitt.dev/schema/demo-dry.json" {
		t.Errorf("dry output $id = %v", id)
	}
	if dryRun.Flag != "--detail" {
		t.Errorf("dry output flag = %q, want --detail", dryRun.Flag)
	}
	if _, ok := document.Schemas["demo_dry"]; !ok {
		t.Errorf("document.schemas missing demo_dry")
	}
	if _, ok := document.ErrorCodes[output.CodeInternalError]; !ok {
		t.Error("document.error_codes missing INTERNAL_ERROR")
	}
	if len(document.CommonErrorCodes) == 0 || document.CommonErrorCodes[0] != output.CodeInvalidRequest {
		t.Errorf("common_error_codes = %v, want INVALID_REQUEST", document.CommonErrorCodes)
	}
	if len(node.Constraints) != 1 || node.Constraints[0].Kind != spec.ConstraintAlone {
		t.Errorf("constraints = %+v, want the declared alone rule", node.Constraints)
	}
	if got := node.Constraints[0].Code; got != output.CodeInvalidRequest {
		t.Errorf("constraint code = %q, want %q", got, output.CodeInvalidRequest)
	}
}

func TestDeclareDerivesCobraArgumentValidator(t *testing.T) {
	cases := []struct {
		name     string
		args     []spec.Positional
		accepted [][]string
		rejected [][]string
	}{
		{
			name:     "no positionals",
			accepted: [][]string{{}},
			rejected: [][]string{{"x"}},
		},
		{
			name:     "one optional",
			args:     []spec.Positional{{Name: "id"}},
			accepted: [][]string{{}, {"a"}},
			rejected: [][]string{{"a", "b"}},
		},
		{
			name:     "one required",
			args:     []spec.Positional{{Name: "id", Required: true}},
			accepted: [][]string{{"a"}},
			rejected: [][]string{{}, {"a", "b"}},
		},
		{
			name:     "required then optional",
			args:     []spec.Positional{{Name: "id", Required: true}, {Name: "extra"}},
			accepted: [][]string{{"a"}, {"a", "b"}},
			rejected: [][]string{{}, {"a", "b", "c"}},
		},
		{
			name:     "variadic tail",
			args:     []spec.Positional{{Name: "id", Required: true}, {Name: "rest", Variadic: true}},
			accepted: [][]string{{"a"}, {"a", "b", "c"}},
			rejected: [][]string{{}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, cmd := newDemoCommand()
			spec.Declare(cmd, spec.Contract{Args: testCase.args})
			for _, args := range testCase.accepted {
				if err := cmd.ValidateArgs(args); err != nil {
					t.Errorf("ValidateArgs(%v) = %v, want nil", args, err)
				}
			}
			for _, args := range testCase.rejected {
				if err := cmd.ValidateArgs(args); err == nil {
					t.Errorf("ValidateArgs(%v) = nil, want an arity error", args)
				}
			}
		})
	}
}

func TestDeclareRejectsLyingContracts(t *testing.T) {
	cases := []struct {
		name    string
		declare func(cmd *cobra.Command)
	}{
		{
			name: "unknown flag",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Flags: []spec.FlagRule{{Name: "missing"}}})
			},
		},
		{
			name: "unregistered error code",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Errors: []string{"NOT_A_CODE"}})
			},
		},
		{
			name: "constraint naming an unknown flag",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Constraints: []spec.Constraint{
					spec.Alone("alone", output.CodeInvalidRequest, "--missing"),
				}})
			},
		},
		{
			name: "constraint emitting an undeclared code",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Constraints: []spec.Constraint{
					spec.Alone("alone", output.CodeBaseConflict, "--kind"),
				}})
			},
		},
		{
			name: "one_of with a single member",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Constraints: []spec.Constraint{
					spec.OneOf("base", output.CodeInvalidRequest, "--kind"),
				}})
			},
		},
		{
			name: "duplicate constraint name",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Constraints: []spec.Constraint{
					spec.Alone("same", output.CodeInvalidRequest, "--kind"),
					spec.Requires("same", output.CodeInvalidRequest, "--kind", "--kind"),
				}})
			},
		},
		{
			name: "already has a PreRunE",
			declare: func(cmd *cobra.Command) {
				cmd.PreRunE = func(*cobra.Command, []string) error { return nil }
				spec.Declare(cmd, spec.Contract{})
			},
		},
		{
			name: "two default outputs",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Outputs: []spec.Output{
					spec.Schema[sampleStruct]("a", "a"),
					spec.Schema[sampleStruct]("b", "b"),
				}})
			},
		},
		{
			name: "duplicate output mode",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Outputs: []spec.Output{
					spec.Schema[sampleStruct]("a", "a"),
					spec.Schema[sampleStruct]("b", "b").Named("dry").WhenFlag("--detail"),
					spec.Schema[sampleStruct]("c", "c").Named("dry").WhenFlag("--detail"),
				}})
			},
		},
		{
			name: "duplicate published key",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Outputs: []spec.Output{
					spec.Schema[sampleStruct]("a", "a").Named("a").WhenFlag("--detail").PublishAs("key"),
					spec.Schema[sampleStruct]("b", "b").Named("b").WhenFlag("--full").PublishAs("key"),
				}})
			},
		},
		{
			name: "required positional after optional",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Args: []spec.Positional{
					{Name: "optional"},
					{Name: "required", Required: true},
				}})
			},
		},
		{
			name: "variadic before last",
			declare: func(cmd *cobra.Command) {
				spec.Declare(cmd, spec.Contract{Args: []spec.Positional{
					{Name: "rest", Variadic: true},
					{Name: "tail"},
				}})
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, cmd := newDemoCommand()
			// A lie is caught either when the declaration is recorded or when
			// the tree is serialized: flag refs may name an inherited global
			// flag, which only exists once the command is attached to its root.
			mustPanic(t, func() {
				testCase.declare(cmd)
				spec.Build(root)
			})
		})
	}
}

func TestBuildRejectsDuplicateSchemaIDsAndKeys(t *testing.T) {
	t.Run("schema id", func(t *testing.T) {
		root := &cobra.Command{Use: "mitt"}
		for _, name := range []string{"a", "b"} {
			cmd := &cobra.Command{Use: name, Short: name}
			spec.Declare(cmd, spec.Contract{Outputs: []spec.Output{
				spec.Schema[sampleStruct]("shared", name),
			}})
			root.AddCommand(cmd)
		}
		mustPanic(t, func() { spec.Build(root) })
	})

	t.Run("schema key", func(t *testing.T) {
		root := &cobra.Command{Use: "mitt"}
		for _, name := range []string{"a", "b"} {
			cmd := &cobra.Command{Use: name, Short: name}
			spec.Declare(cmd, spec.Contract{Outputs: []spec.Output{
				spec.Schema[sampleStruct](name, name).PublishAs("shared"),
			}})
			root.AddCommand(cmd)
		}
		mustPanic(t, func() { spec.Build(root) })
	})

	t.Run("shared schema id", func(t *testing.T) {
		root := &cobra.Command{Use: "mitt"}
		cmd := &cobra.Command{Use: "a", Short: "a"}
		spec.Declare(cmd, spec.Contract{Outputs: []spec.Output{
			spec.Schema[sampleStruct]("request-document", "request document"),
		}})
		root.AddCommand(cmd)
		mustPanic(t, func() { spec.Build(root) })
	})
}

// TestDeclareRejectsContradictoryConstraintShapes covers the ways a rule can be
// written so that no command line can ever satisfy it, or so that the runtime
// has no branch to evaluate it. Each one is a declaration bug, so it must fail
// loudly at construction rather than publish a rule the CLI does not apply.
func TestDeclareRejectsContradictoryConstraintShapes(t *testing.T) {
	cases := []struct {
		name  string
		rules []spec.Constraint
	}{
		{
			name:  "one_of repeating a member",
			rules: []spec.Constraint{spec.OneOf("base", output.CodeInvalidRequest, "--kind", "--kind")},
		},
		{
			name:  "requires naming its own trigger",
			rules: []spec.Constraint{spec.Requires("self", output.CodeInvalidRequest, "--kind", "--kind")},
		},
		{
			name:  "forbids naming its own trigger",
			rules: []spec.Constraint{spec.Forbids("self", output.CodeInvalidRequest, "--kind", "--kind")},
		},
		{
			name:  "alone on a positional",
			rules: []spec.Constraint{spec.Alone("lone", output.CodeInvalidRequest, "<id>")},
		},
		{
			name:  "except naming a positional",
			rules: []spec.Constraint{spec.OneOf("base", output.CodeInvalidRequest, "--kind", "--dry").Unless("<id>")},
		},
		{
			name:  "unknown kind",
			rules: []spec.Constraint{{Kind: "xor", Name: "base", Members: []string{"--kind", "--dry"}, Code: output.CodeInvalidRequest}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, cmd := newDemoCommand()
			mustPanic(t, func() {
				spec.Declare(cmd, spec.Contract{
					Args:        []spec.Positional{{Name: "id"}},
					Constraints: testCase.rules,
					Errors:      []string{output.CodeInvalidRequest},
				})
			})
		})
	}
}

// TestDeclareRejectsDuplicatePositionalNames pins the ambiguity a duplicate
// name would create: constraints address positionals by name, so two
// positionals called <id> would make every ref to them mean both.
func TestDeclareRejectsDuplicatePositionalNames(t *testing.T) {
	_, cmd := newDemoCommand()
	mustPanic(t, func() {
		spec.Declare(cmd, spec.Contract{Args: []spec.Positional{{Name: "id"}, {Name: "id"}}})
	})
}

func newDemoCommand() (*cobra.Command, *cobra.Command) {
	root := &cobra.Command{Use: "mitt"}
	cmd := &cobra.Command{Use: "demo <id>", Short: "demo"}
	cmd.Flags().String("kind", "", "kind of thing")
	cmd.Flags().String("dry", "", "dry mode")
	cmd.Flags().Bool("detail", false, "detail mode")
	cmd.Flags().Bool("full", false, "full mode")
	root.AddCommand(cmd)
	return root, cmd
}

// TestDeclareEnforcesConstraints is the declaration's own test surface: the
// rules are exercised through the installed PreRunE, exactly as the CLI runs
// them, not through a copy of the checker.
func TestDeclareEnforcesConstraints(t *testing.T) {
	type testCase struct {
		name    string
		fixture spec.Contract
		args    []string
		flags   map[string]string
		code    string
		detail  string
		detailV any
	}
	cases := []testCase{
		{
			name:    "one_of accepts exactly one",
			fixture: spec.Contract{Constraints: []spec.Constraint{spec.OneOf("base", output.CodeBaseConflict, "--kind", "--dry")}},
			flags:   map[string]string{"kind": "a"},
		},
		{
			name:    "one_of rejects none",
			fixture: spec.Contract{Constraints: []spec.Constraint{spec.OneOf("base", output.CodeBaseConflict, "--kind", "--dry")}},
			code:    output.CodeBaseConflict,
			detail:  "constraint",
			detailV: "base",
		},
		{
			name:    "one_of rejects two",
			fixture: spec.Contract{Constraints: []spec.Constraint{spec.OneOf("base", output.CodeBaseConflict, "--kind", "--dry")}},
			flags:   map[string]string{"kind": "a", "dry": "b"},
			code:    output.CodeBaseConflict,
			detail:  "constraint",
			detailV: "base",
		},
		{
			name: "one_of except mode is inert",
			fixture: spec.Contract{
				Args:        []spec.Positional{{Name: "id"}},
				Constraints: []spec.Constraint{spec.OneOf("base", output.CodeBaseConflict, "<id>", "--kind").Unless("--dry")},
			},
			flags: map[string]string{"dry": "b"},
		},
		{
			name:    "requires rejects a missing companion",
			fixture: spec.Contract{Constraints: []spec.Constraint{spec.Requires("needs", output.CodeInvalidRequest, "--kind", "--dry")}},
			flags:   map[string]string{"kind": "a"},
			code:    output.CodeInvalidRequest,
			detail:  "requires",
		},
		{
			name:    "requires is inert when the trigger is absent",
			fixture: spec.Contract{Constraints: []spec.Constraint{spec.Requires("needs", output.CodeInvalidRequest, "--kind", "--dry")}},
			flags:   map[string]string{"dry": "b"},
		},
		{
			name:    "forbids rejects a conflict",
			fixture: spec.Contract{Constraints: []spec.Constraint{spec.Forbids("nope", output.CodeInvalidRequest, "--kind", "--dry")}},
			flags:   map[string]string{"kind": "a", "dry": "b"},
			code:    output.CodeInvalidRequest,
			detail:  "constraint",
			detailV: "nope",
		},
		{
			name:    "alone rejects any other flag",
			fixture: spec.Contract{Constraints: []spec.Constraint{spec.Alone("lone", output.CodeInvalidRequest, "--kind")}},
			flags:   map[string]string{"kind": "a", "dry": "b"},
			code:    output.CodeInvalidRequest,
			detail:  "constraint",
			detailV: "lone",
		},
		{
			name: "alone rejects any positional",
			fixture: spec.Contract{
				Args:        []spec.Positional{{Name: "id"}},
				Constraints: []spec.Constraint{spec.Alone("lone", output.CodeInvalidRequest, "--kind")},
			},
			flags:  map[string]string{"kind": "a"},
			args:   []string{"entry"},
			code:   output.CodeInvalidRequest,
			detail: "conflicts",
		},
		{
			name:    "alone accepts the flag by itself",
			fixture: spec.Contract{Constraints: []spec.Constraint{spec.Alone("lone", output.CodeInvalidRequest, "--kind")}},
			flags:   map[string]string{"kind": "a"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root, cmd := newDemoCommand()
			fixture := test.fixture
			for _, constraint := range fixture.Constraints {
				fixture.Errors = append(fixture.Errors, constraint.Code)
			}
			spec.Declare(cmd, fixture)
			for name, value := range test.flags {
				if err := cmd.Flags().Set(name, value); err != nil {
					t.Fatalf("set --%s: %v", name, err)
				}
			}
			err := cmd.PreRunE(cmd, test.args)
			if test.code == "" {
				if err != nil {
					t.Fatalf("PreRunE = %v, want nil", err)
				}
				return
			}
			var appErr *output.AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("PreRunE = %v, want a typed AppError", err)
			}
			if appErr.Code != test.code {
				t.Errorf("code = %s, want %s", appErr.Code, test.code)
			}
			if test.detail != "" {
				if _, ok := appErr.Details[test.detail]; !ok {
					t.Errorf("details missing %q: %v", test.detail, appErr.Details)
				}
			}
			if test.detailV != nil {
				if got := appErr.Details[test.detail]; !reflect.DeepEqual(got, test.detailV) {
					t.Errorf("details.%s = %v, want %v", test.detail, got, test.detailV)
				}
			}
			_ = root
		})
	}
}

func TestDeclareEnforcesEnum(t *testing.T) {
	root, cmd := newDemoCommand()
	spec.Declare(root, spec.Contract{})
	spec.Declare(cmd, spec.Contract{
		Flags: []spec.FlagRule{{Name: "kind", Enum: []string{"a", "b"}}},
	})
	if err := cmd.Flags().Set("kind", "z"); err != nil {
		t.Fatalf("set --kind: %v", err)
	}
	err := cmd.PreRunE(cmd, nil)
	var appErr *output.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("PreRunE = %v, want a typed AppError", err)
	}
	if appErr.Code != output.CodeInvalidRequest {
		t.Errorf("code = %s, want INVALID_REQUEST", appErr.Code)
	}
	if got := appErr.Details["allowed"]; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("details.allowed = %v, want [a b]", got)
	}
}

// TestDeclareEnforcesInheritedGlobalFlagRules proves a rule may name a flag the
// command inherits rather than one it declares. A global output flag on the
// root is the realistic case: a command that forbids it must be able to name
// it even though the flag is attached to an ancestor. The test drives the real
// cobra execution path so the flag is parsed by the same pflag set the CLI
// runs.
func TestDeclareEnforcesInheritedGlobalFlagRules(t *testing.T) {
	run := func(t *testing.T, args ...string) error {
		t.Helper()
		root, cmd := newDemoCommand()
		// cobra only reaches PreRunE for a runnable command; the demo leaf has
		// no behaviour of its own, so give it one for this test.
		cmd.RunE = func(*cobra.Command, []string) error { return nil }
		root.PersistentFlags().Bool("json", false, "emit JSON")
		spec.Declare(cmd, spec.Contract{
			Flags: []spec.FlagRule{{Name: "kind", Enum: []string{"a", "b"}}},
			Constraints: []spec.Constraint{
				spec.Forbids("kind-excludes-json", output.CodeInvalidRequest, "--kind", "--json"),
			},
			Errors: []string{output.CodeInvalidRequest},
		})
		root.SetArgs(args)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		return root.Execute()
	}

	t.Run("rejects the inherited flag", func(t *testing.T) {
		err := run(t, "--json", "demo", "--kind", "a")
		var appErr *output.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("Execute() = %v, want a typed AppError", err)
		}
		if appErr.Code != output.CodeInvalidRequest {
			t.Errorf("code = %s, want INVALID_REQUEST", appErr.Code)
		}
		if got := appErr.Details["constraint"]; got != "kind-excludes-json" {
			t.Errorf("details.constraint = %v, want kind-excludes-json", got)
		}
		if got := appErr.Details["conflicts"]; !reflect.DeepEqual(got, []string{"--json"}) {
			t.Errorf("details.conflicts = %v, want [--json]", got)
		}
	})

	t.Run("accepts the trigger alone", func(t *testing.T) {
		if err := run(t, "demo", "--kind", "a"); err != nil {
			t.Fatalf("Execute() = %v, want nil", err)
		}
	})
}

// TestAloneIgnoresInheritedFlags pins the boundary of the alone expansion:
// alone covers what the command itself contributes, so an ambient global flag
// does not make `--schema` plus an ambient global flag a contradiction. A rule
// that does want to forbid a global flag names it explicitly.
func TestAloneIgnoresInheritedFlags(t *testing.T) {
	root, cmd := newDemoCommand()
	cmd.RunE = func(*cobra.Command, []string) error { return nil }
	root.PersistentFlags().Bool("json", false, "emit JSON")
	spec.Declare(cmd, spec.Contract{
		Constraints: []spec.Constraint{spec.Alone("lone", output.CodeInvalidRequest, "--kind")},
		Errors:      []string{output.CodeInvalidRequest},
	})
	root.SetArgs([]string{"--json", "demo", "--kind", "a"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil: a global flag must not violate alone", err)
	}
}

// TestBuildRevalidatesRecordedConstraints pins the one path into the document
// that does not go through Declare: the constraints annotation is plain JSON,
// so a stale or hand-edited one could otherwise publish a rule the runtime
// never evaluates. Build re-checks shape and refs before serializing.
func TestBuildRevalidatesRecordedConstraints(t *testing.T) {
	cases := []struct {
		name     string
		recorded string
	}{
		{
			name:     "unknown kind",
			recorded: `[{"kind":"xor","name":"base","members":["--kind","--dry"],"code":"INVALID_REQUEST"}]`,
		},
		{
			name:     "unknown flag",
			recorded: `[{"kind":"forbids","name":"nope","when":"--kind","members":["--missing"],"code":"INVALID_REQUEST"}]`,
		},
		{
			name:     "alone with members",
			recorded: `[{"kind":"alone","name":"lone","when":"--kind","members":["--dry"],"code":"INVALID_REQUEST"}]`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, cmd := newDemoCommand()
			spec.Declare(cmd, spec.Contract{})
			cmd.Annotations["mitt.io/constraints"] = testCase.recorded
			mustPanic(t, func() { spec.Build(root) })
		})
	}
}

func flagNamed(flags []spec.Flag, name string) (spec.Flag, bool) {
	for _, flag := range flags {
		if flag.Name == name {
			return flag, true
		}
	}
	return spec.Flag{}, false
}

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	fn()
}
