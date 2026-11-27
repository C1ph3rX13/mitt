package spec

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Annotation keys record the declared contract on the live cobra tree. Build
// reads them back; no other package should write them by hand.
const (
	AnnotationEnum        = "mitt.io/enum"
	AnnotationConstraints = "mitt.io/constraints"
	AnnotationErrorCodes  = "mitt.io/error-codes"
	AnnotationPositionals = "mitt.io/positionals"
	AnnotationOutputs     = "mitt.io/outputs"
)

// CommonErrorCodes are declared at the document level because every command can
// emit them from argument validation: an unknown flag, an unexpected
// positional, or a violated constraint is a usage mistake no matter which
// command receives it. A command's own errors list only the codes its
// semantics add.
var CommonErrorCodes = []string{output.CodeInvalidRequest}

// Contract is everything one command promises its callers: the positional
// arguments it accepts, the metadata of its flags, the rules that relate them,
// the errors it may emit, and the shapes of its success output. Declare records
// it on the command.
type Contract struct {
	Args        []Positional
	Flags       []FlagRule
	Constraints []Constraint
	Errors      []string
	Outputs     []Output
}

// FlagRule adds contract metadata to one already-registered flag. Enum values
// usually come from the domain constant list that also drives runtime
// validation, so the flag and the document share one source.
type FlagRule struct {
	Name string
	Enum []string
}

// Output is one success output shape. Mode "" is the command's default shape;
// any other Mode is the shape's key in the published contract. A non-default
// shape declares how an invocation selects it: Flag is a bool flag whose
// presence selects it, Tabular selects it for the formats that cannot render
// the default shape (table and md), or both. Stream marks a record stream
// (NDJSON in JSON, rows otherwise); EventStream marks a stream written as
// events happen, which only JSON can render. Opaque marks output that is not a JSON
// document at all, such as bytes written verbatim.
type Output struct {
	Mode        string         `json:"mode,omitempty"`
	Flag        string         `json:"flag,omitempty"`
	Tabular     bool           `json:"tabular,omitempty"`
	Stream      bool           `json:"stream,omitempty"`
	EventStream bool           `json:"event_stream,omitempty"`
	Opaque      bool           `json:"opaque,omitempty"`
	Key         string         `json:"key,omitempty"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
}

// Schema derives the output schema for T and returns it as a command output.
func Schema[T any](slug, title string) Output {
	return Output{Schema: SchemaOf[T](slug, title)}
}

// Opaque declares output that is not a JSON document, such as raw message
// bytes. Its mode carries a description instead of a schema.
func Opaque(mode, description string) Output {
	return Output{Mode: mode, Opaque: true, Description: description}
}

// Named sets the mode key this output publishes under. The key is how callers
// name the shape, so a non-default output without one is only reachable by its
// selector, not by name.
func (o Output) Named(mode string) Output {
	o.Mode = mode
	return o
}

// WhenFlag marks this output as the shape emitted when flag is present. The
// flag must be a bool flag declared by the command.
func (o Output) WhenFlag(flag string) Output {
	o.Flag = flag
	return o
}

// ForTabularFormats marks this output as the shape emitted for the formats
// that cannot render the default shape (table and md). It may be combined
// with WhenFlag so the same shape is also addressable in JSON.
func (o Output) ForTabularFormats() Output {
	o.Tabular = true
	return o
}

// Streamed marks this output as a record stream: JSON renders NDJSON and the
// human formats render the same records as a table or list.
func (o Output) Streamed() Output {
	o.Stream = true
	return o
}

// Live marks this output as a stream written as events happen. It can only be
// rendered as JSON, because buffering it would defeat the point.
func (o Output) Live() Output {
	o.Stream = true
	o.EventStream = true
	return o
}

// PublishAs also registers this schema under document.schemas[key], keeping
// the long-standing named-schema registry entry for the same declaration.
func (o Output) PublishAs(key string) Output {
	o.Key = key
	return o
}

// Described sets the schema's top-level description, or the mode description
// for opaque output. A schema already carries its own description, so only
// opaque output needs the mode-level copy.
func (o Output) Described(text string) Output {
	if o.Opaque {
		o.Description = text
		return o
	}
	Describe(o.Schema, text)
	return o
}

// Declare records the contract on cmd and installs the checks it implies. A
// declaration is static data, so a lying contract is a programmer error:
// unregistered error codes, contradictory outputs, and constraints naming
// positionals or kinds the command does not have all panic at command
// construction instead of leaking into `mitt spec`. Constraint flag refs are
// checked by Build instead: a rule may name an inherited global flag, which
// does not exist until the command is attached to its root.
//
// Declare owns the command's PreRunE: the declared constraints are the only
// compatibility rules the command applies, so a second hand-written validator
// cannot disagree with the document.
func Declare(cmd *cobra.Command, contract Contract) {
	if cmd.Args != nil {
		panic(fmt.Sprintf("spec: %s already has a cobra Args validator; declare positionals instead", cmd.CommandPath()))
	}
	if cmd.PreRunE != nil {
		panic(fmt.Sprintf("spec: %s already has a PreRunE; declare constraints instead", cmd.CommandPath()))
	}
	cmd.Args = argsFor(cmd, contract.Args)
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		inv := newInvocation(cmd, contract.Args, args)
		if err := checkEnums(inv, contract.Flags); err != nil {
			return err
		}
		for _, constraint := range contract.Constraints {
			if err := constraint.check(inv); err != nil {
				return err
			}
		}
		return nil
	}
	declarePositionals(cmd, contract.Args)
	declareFlags(cmd, contract.Flags)
	declareConstraints(cmd, contract)
	declareErrors(cmd, contract.Errors)
	declareOutputs(cmd, contract)
}

func declarePositionals(cmd *cobra.Command, args []Positional) {
	data, err := json.Marshal(args)
	if err != nil {
		panic(fmt.Sprintf("spec: encode positionals for %s: %v", cmd.CommandPath(), err))
	}
	ensureCommandAnnotations(cmd)
	cmd.Annotations[AnnotationPositionals] = string(data)
}

// argsFor derives the cobra argument validator from the declared positionals,
// so the command's runtime arity and its published contract cannot disagree.
// Arity failures are INVALID_REQUEST by construction: the CLI no longer
// classifies usage errors by matching message prefixes.
func argsFor(cmd *cobra.Command, args []Positional) cobra.PositionalArgs {
	required := 0
	variadic := false
	seenOptional := false
	names := map[string]bool{}
	for index, arg := range args {
		if arg.Name == "" {
			panic(fmt.Sprintf("spec: %s declares a positional without a name", cmd.CommandPath()))
		}
		// A constraint names a positional by name, so two positionals sharing
		// one would make every ref to it ambiguous.
		if names[arg.Name] {
			panic(fmt.Sprintf("spec: %s declares positional %s twice", cmd.CommandPath(), arg.Name))
		}
		names[arg.Name] = true
		if arg.Variadic && index != len(args)-1 {
			panic(fmt.Sprintf("spec: %s declares a variadic positional before the last one", cmd.CommandPath()))
		}
		if arg.Variadic {
			variadic = true
		}
		if arg.Required {
			if seenOptional {
				panic(fmt.Sprintf("spec: %s declares a required positional after an optional one", cmd.CommandPath()))
			}
			required++
			continue
		}
		seenOptional = true
	}
	limit := len(args)
	return func(cmd *cobra.Command, got []string) error {
		count := len(got)
		path := cmd.CommandPath()
		if len(args) == 0 && count > 0 {
			if cmd.HasSubCommands() {
				return output.UsageError(fmt.Sprintf("unknown command %q for %q", got[0], path))
			}
			return output.UsageError(fmt.Sprintf("%s takes no positional arguments; received %q", path, got[0])).
				WithDetail("received", count)
		}
		if count < required {
			expected := positionalTokens(args[:required])
			message := fmt.Sprintf("%s requires %s", path, expected)
			if variadic {
				message = fmt.Sprintf("%s requires at least %s", path, expected)
			}
			return output.UsageError(message).
				WithDetail("requires", positionalNames(args[:required])).
				WithDetail("received", count)
		}
		if !variadic && count > limit {
			return output.UsageError(fmt.Sprintf("%s accepts at most %d positional argument(s); received %d", path, limit, count)).
				WithDetail("max", limit).
				WithDetail("received", count)
		}
		return nil
	}
}

func positionalTokens(args []Positional) string {
	tokens := make([]string, 0, len(args))
	for _, arg := range args {
		tokens = append(tokens, "<"+arg.Name+">")
	}
	return strings.Join(tokens, " ")
}

func positionalNames(args []Positional) []string {
	names := make([]string, 0, len(args))
	for _, arg := range args {
		names = append(names, arg.Name)
	}
	return names
}

func declareFlags(cmd *cobra.Command, rules []FlagRule) {
	seen := map[string]bool{}
	for _, rule := range rules {
		if rule.Name == "" {
			panic(fmt.Sprintf("spec: %s declares a flag rule without a name", cmd.CommandPath()))
		}
		if seen[rule.Name] {
			panic(fmt.Sprintf("spec: %s declares flag --%s twice", cmd.CommandPath(), rule.Name))
		}
		seen[rule.Name] = true
		flag := lookupFlag(cmd, rule.Name)
		if flag == nil {
			panic(fmt.Sprintf("spec: %s declares unknown flag --%s", cmd.CommandPath(), rule.Name))
		}
		if len(rule.Enum) > 0 {
			ensureFlagAnnotations(flag)
			flag.Annotations[AnnotationEnum] = append([]string(nil), rule.Enum...)
		}
	}
}

// checkEnums enforces every declared enum at the command's PreRunE, so the
// published list of accepted values is the list the command actually accepts.
// An empty value means "unset" and is skipped, matching pflag's default
// semantics; the command's own parser decides what unset means. Presence goes
// through the same invocation value the constraints use, so an inherited flag
// is checked exactly like a local one.
func checkEnums(inv invocation, rules []FlagRule) error {
	for _, rule := range rules {
		if len(rule.Enum) == 0 || !inv.present("--"+rule.Name) {
			continue
		}
		flag := inv.flag(rule.Name)
		if flag == nil {
			continue
		}
		if flag.Value.String() == "" {
			continue
		}
		if slices.Contains(rule.Enum, flag.Value.String()) {
			continue
		}
		return output.NewError(
			output.CodeInvalidRequest,
			fmt.Sprintf("--%s %q is not supported", rule.Name, flag.Value.String()),
			"use one of: "+strings.Join(rule.Enum, ", "),
		).WithDetail("flag", rule.Name).
			WithDetail("value", flag.Value.String()).
			WithDetail("allowed", rule.Enum)
	}
	return nil
}

// declareConstraints validates each rule's shape against the command's live
// flags and positionals, then records the whole set for Build. A constraint
// that emits a code the command does not declare is a lying contract, so it
// panics here; flag refs are checked by Build, which has the full tree.
func declareConstraints(cmd *cobra.Command, contract Contract) {
	names := map[string]bool{}
	declared := map[string]bool{}
	for _, code := range contract.Errors {
		declared[code] = true
	}
	for _, code := range CommonErrorCodes {
		declared[code] = true
	}
	for _, constraint := range contract.Constraints {
		if names[constraint.Name] {
			panic(fmt.Sprintf("spec: %s declares constraint %q twice", cmd.CommandPath(), constraint.Name))
		}
		names[constraint.Name] = true
		constraint.validateShape(cmd.CommandPath(), contract.Args)
		if !declared[constraint.Code] {
			panic(fmt.Sprintf("spec: %s constraint %q emits %s but the command does not declare it", cmd.CommandPath(), constraint.Name, constraint.Code))
		}
	}
	data, err := json.Marshal(contract.Constraints)
	if err != nil {
		panic(fmt.Sprintf("spec: encode constraints for %s: %v", cmd.CommandPath(), err))
	}
	ensureCommandAnnotations(cmd)
	cmd.Annotations[AnnotationConstraints] = string(data)
}

// lookupFlag resolves a bare flag name against the command's effective
// surface: a flag it declares, its own persistent flags, or one it inherits
// from an ancestor. A constraint may name any of the three, so the answer here
// is the same one the runtime uses to decide whether the flag is present.
func lookupFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return flag
	}
	if flag := cmd.PersistentFlags().Lookup(name); flag != nil {
		return flag
	}
	return cmd.InheritedFlags().Lookup(name)
}

func declareErrors(cmd *cobra.Command, codes []string) {
	for _, code := range codes {
		if _, ok := output.LookupCode(code); !ok {
			panic(fmt.Sprintf("spec: %s declares unregistered error code %s", cmd.CommandPath(), code))
		}
	}
	ensureCommandAnnotations(cmd)
	cmd.Annotations[AnnotationErrorCodes] = strings.Join(codes, ",")
}

func declareOutputs(cmd *cobra.Command, contract Contract) {
	defaults := 0
	modes := map[string]bool{}
	keys := map[string]bool{}
	flagSelectors := map[string]string{}
	tabularMode := ""
	for _, out := range contract.Outputs {
		if out.Opaque {
			if out.Schema != nil {
				panic(fmt.Sprintf("spec: %s opaque output mode %s cannot carry a schema", cmd.CommandPath(), out.Mode))
			}
			if out.Description == "" {
				panic(fmt.Sprintf("spec: %s opaque output mode %s needs a description", cmd.CommandPath(), out.Mode))
			}
		} else if out.Schema == nil {
			panic(fmt.Sprintf("spec: %s declares an empty output schema", cmd.CommandPath()))
		}
		if out.EventStream && !out.Stream {
			panic(fmt.Sprintf("spec: %s output mode %s is live but not a stream", cmd.CommandPath(), out.Mode))
		}
		if out.Mode == "" {
			if out.Flag != "" || out.Tabular || out.EventStream || out.Opaque {
				panic(fmt.Sprintf("spec: %s default output cannot have a selector, live or opaque flag", cmd.CommandPath()))
			}
			defaults++
		} else {
			if modes[out.Mode] {
				panic(fmt.Sprintf("spec: %s declares output mode %s twice", cmd.CommandPath(), out.Mode))
			}
			if out.Flag == "" && !out.Tabular {
				panic(fmt.Sprintf("spec: %s declares output mode %s without a selector", cmd.CommandPath(), out.Mode))
			}
			if out.Flag != "" {
				name := strings.TrimPrefix(out.Flag, "--")
				if !strings.HasPrefix(out.Flag, "--") || name == "" {
					panic(fmt.Sprintf("spec: %s output mode %s has invalid selector %q; use --flag", cmd.CommandPath(), out.Mode, out.Flag))
				}
				flag := cmd.Flags().Lookup(name)
				if flag == nil {
					panic(fmt.Sprintf("spec: %s output mode %s selects with unknown flag %s", cmd.CommandPath(), out.Mode, out.Flag))
				}
				if flag.Value.Type() != "bool" {
					panic(fmt.Sprintf("spec: %s output mode %s selects with non-bool flag %s", cmd.CommandPath(), out.Mode, out.Flag))
				}
				if previous, ok := flagSelectors[name]; ok {
					panic(fmt.Sprintf("spec: %s output modes %s and %s both select with %s", cmd.CommandPath(), previous, out.Mode, out.Flag))
				}
				flagSelectors[name] = out.Mode
			}
			if out.Tabular {
				if tabularMode != "" {
					panic(fmt.Sprintf("spec: %s output modes %s and %s are both selected by table formats", cmd.CommandPath(), tabularMode, out.Mode))
				}
				tabularMode = out.Mode
			}
		}
		modes[out.Mode] = true
		if out.Key != "" {
			if keys[out.Key] {
				panic(fmt.Sprintf("spec: %s publishes schema key %s twice", cmd.CommandPath(), out.Key))
			}
			keys[out.Key] = true
		}
	}
	if defaults > 1 {
		panic(fmt.Sprintf("spec: %s declares more than one default output", cmd.CommandPath()))
	}
	// Two flag-selected shapes on one command line would be ambiguous, so the
	// published constraints must make every pair mutually exclusive.
	selectors := make([]string, 0, len(flagSelectors))
	for name := range flagSelectors {
		selectors = append(selectors, "--"+name)
	}
	sort.Strings(selectors)
	for i := 0; i < len(selectors); i++ {
		for j := i + 1; j < len(selectors); j++ {
			if !outputsExclusive(contract.Constraints, selectors[i], selectors[j]) {
				panic(fmt.Sprintf("spec: %s output modes selected by %s and %s are not mutually exclusive in constraints", cmd.CommandPath(), selectors[i], selectors[j]))
			}
		}
	}
	data, err := json.Marshal(contract.Outputs)
	if err != nil {
		panic(fmt.Sprintf("spec: encode outputs for %s: %v", cmd.CommandPath(), err))
	}
	ensureCommandAnnotations(cmd)
	cmd.Annotations[AnnotationOutputs] = string(data)
}

// outputsExclusive reports whether the declared constraints make two mode
// flags impossible to give together. A rule with Except only applies to some
// modes, so it cannot make the pair exclusive for every invocation.
func outputsExclusive(constraints []Constraint, a, b string) bool {
	for _, constraint := range constraints {
		if constraint.Except != "" {
			continue
		}
		switch constraint.Kind {
		case ConstraintAlone:
			if constraint.When == a || constraint.When == b {
				return true
			}
		case ConstraintForbids:
			if (constraint.When == a && slices.Contains(constraint.Members, b)) ||
				(constraint.When == b && slices.Contains(constraint.Members, a)) {
				return true
			}
		case ConstraintOneOf:
			if slices.Contains(constraint.Members, a) && slices.Contains(constraint.Members, b) {
				return true
			}
		}
	}
	return false
}

// SelectOutput returns the success shape one invocation selects. An explicit
// mode flag wins over the format, because the flag names a shape the caller
// asked for; a row-rendering format wins over the default, because the default
// shape has no row form. ok is false when the command declares no shape at
// all, which is how a command states that its default output is silence. This
// is the one place a flag, the format, and the declared shapes turn into an
// output mode.
func SelectOutput(cmd *cobra.Command, format output.Format) (Output, bool) {
	outputs := commandOutputs(cmd)
	for _, out := range outputs {
		if out.Mode != "" && out.Flag != "" && flagChanged(cmd, out.Flag) {
			return out, true
		}
	}
	for _, out := range outputs {
		if out.Mode != "" && out.Tabular && !format.Structural() {
			return out, true
		}
	}
	for _, out := range outputs {
		if out.Mode == "" {
			return out, true
		}
	}
	return Output{}, false
}

// flagChanged reports whether a flag was present on the command line,
// regardless of whether it appeared before or after the subcommand. Presence,
// not the parsed value, selects a mode: --detail=false still asks for the
// detail shape, matching pflag's Changed semantics.
func flagChanged(cmd *cobra.Command, ref string) bool {
	flag := lookupFlag(cmd, strings.TrimPrefix(ref, "--"))
	return flag != nil && flag.Changed
}

func ensureFlagAnnotations(flag *pflag.Flag) {
	if flag.Annotations == nil {
		flag.Annotations = map[string][]string{}
	}
}

func ensureCommandAnnotations(cmd *cobra.Command) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
}
