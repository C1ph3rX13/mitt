package spec

import (
	"fmt"
	"sort"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Constraint kinds. A constraint relates a command's own flags, its inherited
// global flags, and its positionals; the same value is published in
// `mitt spec` and enforced by the PreRunE that Declare installs, so the
// document cannot describe a rule the runtime does not apply.
const (
	// ConstraintOneOf: exactly one member must be present.
	ConstraintOneOf = "one_of"
	// ConstraintRequires: when When is present, the single member must be too.
	ConstraintRequires = "requires"
	// ConstraintForbids: when When is present, no member may be.
	ConstraintForbids = "forbids"
	// ConstraintAlone: When must be the only flag or positional the command
	// itself contributes.
	ConstraintAlone = "alone"
)

// Constraint is one declared compatibility rule over a command's flags and
// positionals. Members and When name a flag as "--name" or a positional as
// "<name>".
//
// A flag ref resolves against the command's effective surface: the flags the
// command declares, the persistent flags of its own subtree, and every
// persistent flag it inherits, so a rule may forbid a global flag such as
// an inherited global flag. Positional refs resolve against the declared positionals.
//
// Except names a mode in which the rule does not apply at all: `send --schema`
// takes no base, so the base rule is inert in that mode (Except: "--schema").
// The exception is part of the declaration, not a carve-out in the checker.
//
// An alone constraint publishes no members: at check time it expands to every
// other flag the command itself declares and every positional, so a flag
// added later cannot slip past it. Inherited global flags are ambient — they
// describe the invocation, not the command's shape — so alone leaves them
// alone; a rule that forbids one names it explicitly.
type Constraint struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	When    string   `json:"when,omitempty"`
	Except  string   `json:"except,omitempty" desc:"Mode flag in which this rule does not apply."`
	Members []string `json:"members"`
	Code    string   `json:"code"`
}

// OneOf declares that exactly one of members must be present. It is the shape
// of a command's mutually exclusive bases.
func OneOf(name, code string, members ...string) Constraint {
	return Constraint{Kind: ConstraintOneOf, Name: name, Members: members, Code: code}
}

// Requires declares that when is present, then must be present as well.
func Requires(name, code, when, then string) Constraint {
	return Constraint{Kind: ConstraintRequires, Name: name, When: when, Members: []string{then}, Code: code}
}

// Forbids declares that when is present, none of members may be.
func Forbids(name, code, when string, members ...string) Constraint {
	return Constraint{Kind: ConstraintForbids, Name: name, When: when, Members: members, Code: code}
}

// Alone declares that when is the only flag the command itself contributes,
// and that no positional may accompany it.
func Alone(name, code, when string) Constraint {
	return Constraint{Kind: ConstraintAlone, Name: name, When: when, Members: []string{}, Code: code}
}

// Unless makes the constraint inert whenever ref is present.
func (c Constraint) Unless(ref string) Constraint {
	c.Except = ref
	return c
}

// refKind distinguishes the two things a constraint ref can name.
type refKind int

const (
	refFlag refKind = iota
	refPositional
)

// parseRef splits a ref into its kind and bare name. Members and When use the
// published "--flag" / "<positional>" spelling, so the declaration, the
// document, and an agent's argv all read the same.
func parseRef(ref string) (refKind, string, bool) {
	if name := strings.TrimPrefix(ref, "--"); name != ref {
		return refFlag, name, name != ""
	}
	if strings.HasPrefix(ref, "<") && strings.HasSuffix(ref, ">") && len(ref) > 2 {
		return refPositional, ref[1 : len(ref)-1], true
	}
	return refFlag, "", false
}

// validateShape reports a contradictory declaration: a rule that cannot be
// evaluated at all. These are the checks that need no finished command tree, so
// Declare runs them and panics before publishing a lie.
func (c Constraint) validateShape(path string, positionals []Positional) {
	if c.Name == "" {
		panic(fmt.Sprintf("spec: %s declares a constraint without a name", path))
	}
	if _, ok := output.LookupCode(c.Code); !ok {
		panic(fmt.Sprintf("spec: %s constraint %q declares unregistered error code %s", path, c.Name, c.Code))
	}
	checkRef := func(ref string) {
		kind, name, ok := parseRef(ref)
		if !ok {
			panic(fmt.Sprintf("spec: %s constraint %q names %q; use --flag or <positional>", path, c.Name, ref))
		}
		if kind == refPositional {
			if _, ok := positionalIndex(positionals, name); !ok {
				panic(fmt.Sprintf("spec: %s constraint %q names unknown positional %s", path, c.Name, ref))
			}
		}
	}
	switch c.Kind {
	case ConstraintOneOf:
		if len(c.Members) < 2 {
			panic(fmt.Sprintf("spec: %s one_of constraint %q needs at least two members", path, c.Name))
		}
		if c.When != "" {
			panic(fmt.Sprintf("spec: %s one_of constraint %q must not declare when", path, c.Name))
		}
		seen := map[string]bool{}
		for _, member := range c.Members {
			if seen[member] {
				panic(fmt.Sprintf("spec: %s one_of constraint %q lists %s twice", path, c.Name, member))
			}
			seen[member] = true
		}
	case ConstraintRequires:
		if c.When == "" || len(c.Members) != 1 {
			panic(fmt.Sprintf("spec: %s requires constraint %q needs one when and one member", path, c.Name))
		}
		c.rejectSelfReference(path)
		checkRef(c.When)
	case ConstraintForbids:
		if c.When == "" || len(c.Members) == 0 {
			panic(fmt.Sprintf("spec: %s forbids constraint %q needs one when and at least one member", path, c.Name))
		}
		c.rejectSelfReference(path)
		checkRef(c.When)
	case ConstraintAlone:
		if c.When == "" || len(c.Members) != 0 {
			panic(fmt.Sprintf("spec: %s alone constraint %q needs one when and no members", path, c.Name))
		}
		// "Alone" means "the only thing on the command line besides this
		// flag", so the trigger has to be a flag; a positional is always
		// accompanied by itself and the rule could never hold.
		if kind, _, _ := parseRef(c.When); kind != refFlag {
			panic(fmt.Sprintf("spec: %s alone constraint %q must name a flag, not %s", path, c.Name, c.When))
		}
		checkRef(c.When)
	default:
		panic(fmt.Sprintf("spec: %s constraint %q has unknown kind %q", path, c.Name, c.Kind))
	}
	for _, member := range c.Members {
		checkRef(member)
	}
	if c.Except != "" {
		checkRef(c.Except)
		// A mode is selected by a flag; a positional cannot be "the mode".
		if kind, _, _ := parseRef(c.Except); kind != refFlag {
			panic(fmt.Sprintf("spec: %s constraint %q except must name a flag, not %s", path, c.Name, c.Except))
		}
		if c.Except == c.When {
			panic(fmt.Sprintf("spec: %s constraint %q is inert exactly when it would apply", path, c.Name))
		}
	}
}

// rejectSelfReference reports a rule that names its own trigger as the thing it
// requires or forbids: `requires` would be a tautology and `forbids` would make
// the flag unusable.
func (c Constraint) rejectSelfReference(path string) {
	for _, member := range c.Members {
		if member == c.When {
			panic(fmt.Sprintf("spec: %s %s constraint %q names its own trigger %s", path, c.Kind, c.Name, c.When))
		}
	}
}

// validateRefs reports a flag ref the command does not have. It needs the
// finished command tree because a ref may name an inherited global flag, which
// only exists once the command is attached to its root; Build therefore calls
// it and Declare cannot. Positional refs are already checked by validateShape.
func (c Constraint) validateRefs(path string, hasFlag func(string) bool) {
	check := func(ref string) {
		if kind, name, ok := parseRef(ref); ok && kind == refFlag && !hasFlag(name) {
			panic(fmt.Sprintf("spec: %s constraint %q names unknown flag %s", path, c.Name, ref))
		}
	}
	check(c.When)
	check(c.Except)
	for _, member := range c.Members {
		check(member)
	}
}

// invocation is one parsed command line: the command, its declared
// positionals, and the argv cobra matched. Every question about a constraint
// ref is answered here, so validation, enum checks, and constraint evaluation
// cannot disagree about what "present" means.
type invocation struct {
	cmd         *cobra.Command
	positionals []Positional
	args        []string
}

func newInvocation(cmd *cobra.Command, positionals []Positional, args []string) invocation {
	return invocation{cmd: cmd, positionals: positionals, args: args}
}

// present reports whether ref appears on this command line. A flag counts when
// pflag recorded it, which includes a global flag given before the subcommand.
func (inv invocation) present(ref string) bool {
	kind, name, ok := parseRef(ref)
	if !ok {
		return false
	}
	if kind == refFlag {
		if flag := inv.flag(name); flag != nil {
			return flag.Changed
		}
		return inv.cmd.Flags().Changed(name)
	}
	index, ok := positionalIndex(inv.positionals, name)
	return ok && index < len(inv.args)
}

// presentAll returns the refs that appear, in declaration order.
func (inv invocation) presentAll(refs []string) []string {
	found := make([]string, 0, len(refs))
	for _, ref := range refs {
		if inv.present(ref) {
			found = append(found, ref)
		}
	}
	return found
}

// flag returns the effective flag with this bare name: one the command
// declares, or one it inherits from an ancestor. The returned flag is the same
// value pflag parsed, so Changed and the parsed value agree.
func (inv invocation) flag(name string) *pflag.Flag {
	inherited := inv.cmd.InheritedFlags()
	if flag := inherited.Lookup(name); flag != nil {
		return flag
	}
	return inv.cmd.Flags().Lookup(name)
}

// check applies one constraint to the parsed command line. The returned error
// is always an AppError carrying the code the declaration promised.
func (c Constraint) check(inv invocation) error {
	if c.Except != "" && inv.present(c.Except) {
		return nil
	}
	path := inv.cmd.CommandPath()
	switch c.Kind {
	case ConstraintOneOf:
		present := inv.presentAll(c.Members)
		switch {
		case len(present) == 1:
			return nil
		case len(present) == 0:
			return c.violation(
				fmt.Sprintf("%s requires exactly one of %s", path, c.memberList()),
				map[string]any{"present": present},
				fmt.Sprintf("pass exactly one of %s", c.memberList()),
			)
		default:
			return c.violation(
				fmt.Sprintf("%s accepts only one of %s; got %s", path, c.memberList(), strings.Join(present, ", ")),
				map[string]any{"present": present},
				fmt.Sprintf("pass exactly one of %s", c.memberList()),
			)
		}
	case ConstraintRequires:
		if !inv.present(c.When) {
			return nil
		}
		then := c.Members[0]
		if inv.present(then) {
			return nil
		}
		return c.violation(
			fmt.Sprintf("%s: %s requires %s", path, c.When, then),
			map[string]any{"when": c.When, "requires": then},
			fmt.Sprintf("add %s, or drop %s", then, c.When),
		)
	case ConstraintForbids:
		if !inv.present(c.When) {
			return nil
		}
		conflicts := inv.presentAll(c.Members)
		if len(conflicts) == 0 {
			return nil
		}
		return c.violation(
			fmt.Sprintf("%s: %s cannot be combined with %s", path, c.When, strings.Join(conflicts, ", ")),
			map[string]any{"when": c.When, "conflicts": conflicts},
			fmt.Sprintf("drop %s or %s", c.When, strings.Join(conflicts, ", ")),
		)
	case ConstraintAlone:
		if !inv.present(c.When) {
			return nil
		}
		conflicts := c.aloneConflicts(inv)
		if len(conflicts) == 0 {
			return nil
		}
		return c.violation(
			fmt.Sprintf("%s: %s must be used alone; drop %s", path, c.When, strings.Join(conflicts, ", ")),
			map[string]any{"when": c.When, "conflicts": conflicts},
			fmt.Sprintf("use %s alone, without %s", c.When, strings.Join(conflicts, ", ")),
		)
	default:
		// validateShape rejects unknown kinds, so this is unreachable from a
		// declaration. A rule this build cannot evaluate must not pass
		// silently, though: an unenforced published rule is the exact lie this
		// module exists to prevent.
		return output.InternalErrorf(
			"%s constraint %q has unknown kind %q", path, c.Name, c.Kind)
	}
}

func (c Constraint) violation(message string, details map[string]any, hint string) error {
	appErr := output.NewError(c.Code, message, hint).
		WithDetail("constraint", c.Name).
		WithDetail("kind", c.Kind)
	for key, value := range details {
		appErr = appErr.WithDetail(key, value)
	}
	return appErr
}

// aloneConflicts lists everything the command itself contributes besides the
// alone flag: its own flags in name order, then its positionals. Inherited
// global flags stay out of the expansion — they are ambient, not part of the
// command's shape.
func (c Constraint) aloneConflicts(inv invocation) []string {
	conflicts := []string{}
	_, whenName, _ := parseRef(c.When)
	names := make([]string, 0)
	inv.cmd.LocalFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Name == "help" || flag.Name == whenName {
			return
		}
		names = append(names, flag.Name)
	})
	sort.Strings(names)
	for _, name := range names {
		if inv.cmd.Flags().Changed(name) {
			conflicts = append(conflicts, "--"+name)
		}
	}
	for index, arg := range inv.positionals {
		if index < len(inv.args) {
			conflicts = append(conflicts, "<"+arg.Name+">")
		}
	}
	return conflicts
}

func (c Constraint) memberList() string {
	return strings.Join(c.Members, ", ")
}

func positionalIndex(positionals []Positional, name string) (int, bool) {
	for index, arg := range positionals {
		if arg.Name == name {
			return index, true
		}
	}
	return 0, false
}
