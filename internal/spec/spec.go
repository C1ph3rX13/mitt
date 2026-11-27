// Package spec owns the command contract. Every command declares its
// positionals, flag metadata, error codes, and success output shapes once,
// with Declare; Build serializes those declarations from the live cobra tree.
// Output schemas derive from the Go output structs, so a field cannot drift
// from the published contract without the compiler and the generator
// disagreeing.
package spec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/diff"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/replay"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Version is the command-contract version reported by spec. It tracks the
// meaning of the document, not the binary's release: adding a rule changes what
// callers must expect, so 0.3.0 widened constraint flag refs from "flags this
// command declares" to "every flag the command resolves", 0.4.0 made
// output_schemas a map of mode keys to selectors and schemas instead of a map
// of selector flags to schemas, 0.5.0 replaced the five entry projections
// with entry.View (history rows, show, show --detail, send) and narrowed the
// kind enums to the values each command can actually emit, 0.6.0 made the
// request document's one-body rule an executable allOf in the published
// request_document schema instead of a description a validator ignores, and
// 0.7.0 made stored-body availability one rule: a record that claims bytes
// whose file is missing or a different size is an INTERNAL_ERROR with id,
// direction, path, and store details instead of a silently empty body.
var Version = "0.7.0"

// Limit names registered in the command contract.
const (
	LimitRequestBytesPreviewMaxBytes = "request_bytes_preview_max_bytes"
	LimitDiffBodyMaxBytes            = "diff_body_max_bytes"
)

// Positional describes one command-line positional argument.
type Positional struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Variadic bool   `json:"variadic"`
}

// Flag describes one effective flag on a command.
type Flag struct {
	Name        string   `json:"name"`
	Shorthand   string   `json:"shorthand"`
	Type        string   `json:"type"`
	Default     string   `json:"default"`
	Enum        []string `json:"enum"`
	Inherited   bool     `json:"inherited"`
	Description string   `json:"description"`
}

// Command describes one node in the command tree.
type Command struct {
	Path          string                  `json:"path"`
	Name          string                  `json:"name"`
	Usage         string                  `json:"usage"`
	Short         string                  `json:"short"`
	Args          []Positional            `json:"args"`
	Flags         []Flag                  `json:"flags"`
	Constraints   []Constraint            `json:"constraints" desc:"Rules relating this command's flags and positionals; enforced by the same declaration."`
	ErrorCodes    []string                `json:"error_codes"`
	OutputSchema  any                     `json:"output_schema" desc:"Default success output schema; null when the command emits no structured object."`
	OutputSchemas map[string]OutputSchema `json:"output_schemas,omitempty" desc:"Non-default success shapes keyed by mode name."`
	Children      []Command               `json:"children"`
}

// OutputSchema is one non-default success output shape and how an invocation
// selects it: a bool flag's presence, the table formats, or both. Stream marks
// a record stream, Live marks one written as events happen (JSON only), and
// Opaque marks output that is not a JSON document at all.
type OutputSchema struct {
	Schema      map[string]any `json:"schema,omitempty"`
	Description string         `json:"description,omitempty" desc:"What this shape is; present instead of a schema for opaque output."`
	Flag        string         `json:"flag,omitempty" desc:"Bool flag whose presence selects this shape."`
	Tabular     bool           `json:"tabular,omitempty" desc:"Selects this shape for table and md formats."`
	Stream      bool           `json:"stream,omitempty" desc:"The shape is a record stream: NDJSON in JSON, a table or list otherwise."`
	EventStream bool           `json:"event_stream,omitempty" desc:"The stream is written as events happen; only -f json can render it."`
	Opaque      bool           `json:"opaque,omitempty" desc:"The shape is not a JSON document, so it has no schema."`
}

// Document is the complete machine-readable command contract.
type Document struct {
	Name             string               `json:"name"`
	Version          string               `json:"version"`
	Usage            string               `json:"usage"`
	GlobalFlags      []Flag               `json:"global_flags"`
	Command          Command              `json:"command"`
	CommonErrorCodes []string             `json:"common_error_codes" desc:"Codes any command can emit from argument validation; not repeated on each command."`
	ErrorCodes       map[string]ErrorCode `json:"error_codes"`
	ExitCodes        []ExitCode           `json:"exit_codes"`
	// Schemas registers shared output schemas callers can fetch by name.
	Schemas map[string]any `json:"schemas" desc:"Shared output schemas callers can fetch by name."`
	// Limits registers fixed protocol limits callers can rely on.
	Limits map[string]int `json:"limits" schema:"value_min=0"`
}

// ErrorCode is one entry in the error code registry.
type ErrorCode struct {
	ExitCode    int    `json:"exit_code"`
	Retryable   bool   `json:"retryable"`
	Description string `json:"description"`
}

// ExitCode is one entry in the process exit code registry.
type ExitCode struct {
	Code        int    `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Build walks the live cobra command tree and serializes the declarations
// recorded by Declare into the machine-readable command contract.
func Build(root *cobra.Command) Document {
	state := &buildState{
		schemas:   sharedSchemas(),
		schemaIDs: map[string]string{},
	}
	for key, raw := range state.schemas {
		schema, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := schema["$id"].(string); ok {
			state.schemaIDs[id] = "document.schemas." + key
		}
	}
	doc := Document{
		Name:             root.Name(),
		Version:          Version,
		Usage:            root.UseLine(),
		Command:          buildCommand(root, root.Name(), state),
		CommonErrorCodes: append([]string(nil), CommonErrorCodes...),
		ErrorCodes:       map[string]ErrorCode{},
		Schemas:          state.schemas,
		Limits: map[string]int{
			LimitRequestBytesPreviewMaxBytes: replay.PreviewLimitBytes,
			LimitDiffBodyMaxBytes:            diff.BodyLimitBytes,
		},
	}
	for _, info := range output.Codes() {
		doc.ErrorCodes[info.Code] = ErrorCode{
			ExitCode:    info.ExitCode,
			Retryable:   info.Retryable,
			Description: info.Description,
		}
	}
	for _, info := range output.ExitCodes() {
		doc.ExitCodes = append(doc.ExitCodes, ExitCode{
			Code:        info.Code,
			Name:        info.Name,
			Description: info.Description,
		})
	}
	doc.GlobalFlags = collectGlobalFlags(root)
	return doc
}

type buildState struct {
	schemas   map[string]any
	schemaIDs map[string]string
}

func buildCommand(cmd *cobra.Command, path string, state *buildState) Command {
	node := Command{
		Path:        path,
		Name:        cmd.Name(),
		Usage:       cmd.UseLine(),
		Short:       cmd.Short,
		Args:        positionals(cmd),
		Flags:       collectFlags(cmd),
		Constraints: constraints(cmd),
		ErrorCodes:  errorCodes(cmd),
		Children:    []Command{},
	}
	// Re-validate the decoded rules against the live tree: the annotations are
	// plain JSON, so a stale or hand-edited one must not reach the document as
	// a rule the runtime never applies. Shape is checked against the published
	// positionals, refs against the command's effective flag surface.
	publishedArgs := node.Args
	for _, constraint := range node.Constraints {
		constraint.validateShape(path, publishedArgs)
		constraint.validateRefs(path, func(name string) bool { return lookupFlag(cmd, name) != nil })
	}
	for _, out := range commandOutputs(cmd) {
		if out.Mode == "" {
			node.OutputSchema = out.Schema
		} else {
			if node.OutputSchemas == nil {
				node.OutputSchemas = map[string]OutputSchema{}
			}
			node.OutputSchemas[out.Mode] = OutputSchema{
				Schema:      out.Schema,
				Description: out.Description,
				Flag:        out.Flag,
				Tabular:     out.Tabular,
				Stream:      out.Stream,
				EventStream: out.EventStream,
				Opaque:      out.Opaque,
			}
		}
		registerOutput(cmd, out, state)
	}
	for _, child := range cmd.Commands() {
		if child.Hidden || child.Name() == "help" || child.Name() == "completion" {
			continue
		}
		childPath := path + " " + child.Name()
		node.Children = append(node.Children, buildCommand(child, childPath, state))
	}
	return node
}

func commandOutputs(cmd *cobra.Command) []Output {
	raw := cmd.Annotations[AnnotationOutputs]
	if raw == "" {
		return nil
	}
	var outputs []Output
	if err := json.Unmarshal([]byte(raw), &outputs); err != nil {
		panic(fmt.Sprintf("spec: decode outputs for %s: %v", cmd.CommandPath(), err))
	}
	return outputs
}

func registerOutput(cmd *cobra.Command, out Output, state *buildState) {
	if id, ok := out.Schema["$id"].(string); ok && id != "" {
		if previous, duplicate := state.schemaIDs[id]; duplicate {
			panic(fmt.Sprintf("spec: %s and %s share output schema $id %q", previous, cmd.CommandPath(), id))
		}
		state.schemaIDs[id] = cmd.CommandPath()
	}
	if out.Key == "" {
		return
	}
	if _, duplicate := state.schemas[out.Key]; duplicate {
		panic(fmt.Sprintf("spec: %s publishes duplicate schema key %q", cmd.CommandPath(), out.Key))
	}
	state.schemas[out.Key] = out.Schema
}

func collectGlobalFlags(root *cobra.Command) []Flag {
	flags := make([]Flag, 0)
	root.PersistentFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Name == "help" {
			return
		}
		flags = append(flags, describeFlag(flag, false))
	})
	sort.Slice(flags, func(i, j int) bool { return flags[i].Name < flags[j].Name })
	return flags
}

func collectFlags(cmd *cobra.Command) []Flag {
	inherited := map[string]bool{}
	cmd.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
		inherited[flag.Name] = true
	})
	flags := make([]Flag, 0)
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if flag.Name == "help" {
			return
		}
		flags = append(flags, describeFlag(flag, inherited[flag.Name]))
	})
	sort.Slice(flags, func(i, j int) bool { return flags[i].Name < flags[j].Name })
	return flags
}

func describeFlag(flag *pflag.Flag, inherited bool) Flag {
	enum := append([]string{}, flag.Annotations[AnnotationEnum]...)
	return Flag{
		Name:        flag.Name,
		Shorthand:   flag.Shorthand,
		Type:        flag.Value.Type(),
		Default:     flag.DefValue,
		Enum:        enum,
		Inherited:   inherited,
		Description: flag.Usage,
	}
}

func constraints(cmd *cobra.Command) []Constraint {
	result := []Constraint{}
	raw := cmd.Annotations[AnnotationConstraints]
	if raw == "" {
		return result
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		panic(fmt.Sprintf("spec: decode constraints for %s: %v", cmd.CommandPath(), err))
	}
	if result == nil {
		result = []Constraint{}
	}
	return result
}

func positionals(cmd *cobra.Command) []Positional {
	args := []Positional{}
	raw := cmd.Annotations[AnnotationPositionals]
	if raw == "" {
		return args
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return []Positional{}
	}
	if args == nil {
		args = []Positional{}
	}
	return args
}

func errorCodes(cmd *cobra.Command) []string {
	codes := []string{}
	raw := cmd.Annotations[AnnotationErrorCodes]
	if raw == "" {
		return codes
	}
	for _, code := range strings.Split(raw, ",") {
		if code = strings.TrimSpace(code); code != "" {
			codes = append(codes, code)
		}
	}
	return codes
}
