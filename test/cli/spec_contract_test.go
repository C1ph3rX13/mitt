package cli_test

import "testing"

// TestSpecConstraintsAreEnforced is the other half of the published contract:
// every rule in `constraints` must be the rule the CLI actually applies, and it
// must say so in the error details. A rule that is published but unenforced is
// exactly the drift this declaration exists to prevent, so each case here picks
// an argv that violates one rule and asserts that rule — by name and code — is
// what the command reports.
//
// The show case covers a rule between two local flags; the ability of a rule
// to name an inherited global flag is pinned by the spec package's own tests.
func TestSpecConstraintsAreEnforced(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	cases := []struct {
		name       string
		path       string
		constraint string
		args       []string
	}{
		{
			name:       "alone",
			path:       "mitt send",
			constraint: "schema-alone",
			args:       []string{"send", "--schema", "--dry-run"},
		},
		{
			name:       "one_of none",
			path:       "mitt send",
			constraint: "base",
			args:       []string{"send"},
		},
		{
			name:       "one_of two",
			path:       "mitt send",
			constraint: "base",
			args:       []string{"send", "entry", "--url", "http://127.0.0.1:1/"},
		},
		{
			name:       "requires",
			path:       "mitt send",
			constraint: "diff-against-needs-id",
			args:       []string{"send", "--diff-against", "--url", "http://127.0.0.1:1/"},
		},
		{
			name:       "forbids",
			path:       "mitt send",
			constraint: "diff-against-bases",
			args:       []string{"send", "entry", "--diff-against", "--dry-run"},
		},
		{
			name:       "requires local flags",
			path:       "mitt send",
			constraint: "var-needs-patch",
			args:       []string{"send", "--var", "K=V", "--url", "http://127.0.0.1:1/"},
		},
		{
			name:       "forbids an inherited global flag",
			path:       "mitt show",
			constraint: "raw-excludes-detail",
			args:       []string{"show", "entry", "--raw", "--detail"},
		},
		{
			name:       "requires on a subcommand",
			path:       "mitt proxy start",
			constraint: "session-per-port-needs-range",
			args:       []string{"proxy", "start", "--session-per-port"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.path+"/"+tc.name, func(t *testing.T) {
			command, ok := doc.Command.find(tc.path)
			if !ok {
				t.Fatalf("spec missing %q", tc.path)
			}
			declared, ok := constraintNamed(command.Constraints, tc.constraint)
			if !ok {
				t.Fatalf("%s publishes no constraint %q; got %v", tc.path, tc.constraint, command.Constraints)
			}

			got := env.runWithStore(t, tc.args...)
			if got.exit == 0 {
				t.Fatalf("%v unexpectedly succeeded:\n%s", tc.args, got.stdout)
			}
			errObj := errorEnvelope(t, got)
			if code := errObj["code"]; code != declared.Code {
				t.Errorf("%v code = %v, want %s (the code %q declares)", tc.args, code, declared.Code, tc.constraint)
			}
			details, _ := errObj["details"].(map[string]any)
			if details["constraint"] != tc.constraint {
				t.Errorf("%v details.constraint = %v, want %s", tc.args, details["constraint"], tc.constraint)
			}
			if details["kind"] != declared.Kind {
				t.Errorf("%v details.kind = %v, want %s", tc.args, details["kind"], declared.Kind)
			}
		})
	}
}

// TestSpecExceptionDisablesOnlyItsRule proves `except` is part of the rule and
// not a hole in the checker: --schema turns the base rule off, and the rules
// that still apply keep applying.
func TestSpecExceptionDisablesOnlyItsRule(t *testing.T) {
	env := newTestEnv(t)

	ok := env.runWithStore(t, "send", "--schema")
	requireExit(t, ok, 0)

	for _, args := range [][]string{
		{"send", "--schema", "--url", "http://127.0.0.1:1/"},
		{"send", "--schema", "entry"},
	} {
		got := env.runWithStore(t, args...)
		requireExit(t, got, 2)
		details, _ := errorEnvelope(t, got)["details"].(map[string]any)
		if details["constraint"] != "schema-alone" {
			t.Errorf("%v details.constraint = %v, want schema-alone", args, details["constraint"])
		}
	}
}

// TestSpecErrorCodesCoverEmittedCodes is the machine-checkable half of the
// command contract: every code a command can actually emit has to be declared,
// either on the command or in the document's common set. Without this, the
// published error_codes are a description of what someone remembered rather
// than a promise.
//
// The cases below are chosen to make each command emit its usage failure, which
// is the class that was previously missing from every command's declaration.
func TestSpecErrorCodesCoverEmittedCodes(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	common := map[string]bool{}
	for _, code := range doc.CommonErrorCodes {
		common[code] = true
	}
	declared := func(path string) map[string]bool {
		command, ok := doc.Command.find(path)
		if !ok {
			t.Fatalf("spec missing %q", path)
		}
		codes := map[string]bool{}
		for _, code := range command.ErrorCodes {
			codes[code] = true
		}
		for code := range common {
			codes[code] = true
		}
		return codes
	}

	cases := []struct {
		name string
		path string
		args []string
	}{
		{name: "unknown flag", path: "mitt spec", args: []string{"spec", "--bogus"}},
		{name: "unexpected positional", path: "mitt spec", args: []string{"spec", "extra"}},
		{name: "unexpected positional", path: "mitt ca init", args: []string{"ca", "init", "extra"}},
		{name: "unknown subcommand", path: "mitt ca", args: []string{"ca", "bogus"}},
		{name: "unknown subcommand", path: "mitt", args: []string{"bogus"}},
		{name: "unexpected positional", path: "mitt proxy status", args: []string{"proxy", "status", "extra"}},
		{name: "missing positional", path: "mitt show", args: []string{"show"}},
		{name: "missing positional", path: "mitt diff", args: []string{"diff", "a"}},
		{name: "enum violation", path: "mitt history", args: []string{"history", "--filter", "bogus"}},
		{name: "enum violation", path: "mitt send", args: []string{"send", "--url", "http://127.0.0.1:1/", "--tls-profile", "safari"}},
		{name: "enum violation", path: "mitt proxy start", args: []string{"proxy", "start", "--http2-alpn", "http3", "--addr", "127.0.0.1:0"}},
	}
	for _, tc := range cases {
		t.Run(tc.path+"/"+tc.name, func(t *testing.T) {
			got := env.runWithStore(t, tc.args...)
			if got.exit == 0 {
				t.Fatalf("%v unexpectedly succeeded:\n%s", tc.args, got.stdout)
			}
			code, _ := errorEnvelope(t, got)["code"].(string)
			if !declared(tc.path)[code] {
				t.Errorf("%v emitted %s, which %s does not declare and common_error_codes does not cover", tc.args, code, tc.path)
			}
		})
	}
}

// TestSpecConstraintCodesAreDeclared proves the other direction for rules: a
// constraint's code is part of the contract it is declared in, so it must be
// declared there too (or be common). The check runs over spec output rather
// than over the Go declaration, so a future constraint added without its code
// is caught by the same test that reads the published document.
func TestSpecConstraintCodesAreDeclared(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	common := map[string]bool{}
	for _, code := range doc.CommonErrorCodes {
		common[code] = true
	}
	for _, path := range doc.Command.allPaths() {
		command, _ := doc.Command.find(path)
		own := map[string]bool{}
		for _, code := range command.ErrorCodes {
			own[code] = true
		}
		for _, constraint := range command.Constraints {
			if constraint.Code == "" {
				t.Errorf("%s constraint %q declares no code", path, constraint.Name)
				continue
			}
			if _, registered := doc.ErrorCodes[constraint.Code]; !registered {
				t.Errorf("%s constraint %q uses unregistered code %s", path, constraint.Name, constraint.Code)
			}
			if !own[constraint.Code] && !common[constraint.Code] {
				t.Errorf("%s constraint %q emits %s but neither error_codes nor common_error_codes list it", path, constraint.Name, constraint.Code)
			}
		}
	}
}

// TestSpecConstraintsNameOnlyRealThings keeps the published rules honest: a
// constraint that points at a flag or positional the command does not have is
// a lie the JSON schema cannot detect.
func TestSpecConstraintsNameOnlyRealThings(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	for _, path := range doc.Command.allPaths() {
		command, _ := doc.Command.find(path)
		flags := map[string]bool{}
		for _, flag := range command.Flags {
			flags[flag.Name] = true
		}
		args := map[string]bool{}
		for _, arg := range command.Args {
			args[arg.Name] = true
		}
		checkRef := func(constraint, ref string) {
			switch {
			case len(ref) > 2 && ref[:2] == "--":
				if !flags[ref[2:]] {
					t.Errorf("%s constraint %q names unknown flag %s", path, constraint, ref)
				}
			case len(ref) > 2 && ref[0] == '<':
				if !args[ref[1:len(ref)-1]] {
					t.Errorf("%s constraint %q names unknown positional %s", path, constraint, ref)
				}
			default:
				t.Errorf("%s constraint %q names %q; want --flag or <positional>", path, constraint, ref)
			}
		}
		for _, constraint := range command.Constraints {
			if constraint.Kind == "alone" && len(constraint.Members) != 0 {
				t.Errorf("%s alone constraint %q must publish no members, got %v", path, constraint.Name, constraint.Members)
			}
			if constraint.When != "" {
				checkRef(constraint.Name, constraint.When)
			}
			if constraint.Except != "" {
				checkRef(constraint.Name, constraint.Except)
			}
			for _, member := range constraint.Members {
				checkRef(constraint.Name, member)
			}
		}
	}
}
