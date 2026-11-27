package cli_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

type specDocument struct {
	Name             string                   `json:"name"`
	Version          string                   `json:"version"`
	Usage            string                   `json:"usage"`
	GlobalFlags      []specFlag               `json:"global_flags"`
	Command          specCommand              `json:"command"`
	CommonErrorCodes []string                 `json:"common_error_codes"`
	ErrorCodes       map[string]specErrorCode `json:"error_codes"`
	ExitCodes        []specExitCode           `json:"exit_codes"`
	Schemas          map[string]any           `json:"schemas"`
	Limits           map[string]int           `json:"limits"`
}

type specCommand struct {
	Path          string                `json:"path"`
	Name          string                `json:"name"`
	Usage         string                `json:"usage"`
	Short         string                `json:"short"`
	Args          []specPositional      `json:"args"`
	Flags         []specFlag            `json:"flags"`
	Constraints   []specConstraint      `json:"constraints"`
	ErrorCodes    []string              `json:"error_codes"`
	OutputSchema  map[string]any        `json:"output_schema"`
	OutputSchemas map[string]specOutput `json:"output_schemas"`
	Children      []specCommand         `json:"children"`
}

// specOutput is one non-default shape: its selector and, for JSON documents,
// the schema the command emits.
type specOutput struct {
	Schema      map[string]any `json:"schema"`
	Description string         `json:"description"`
	Flag        string         `json:"flag"`
	Tabular     bool           `json:"tabular"`
	Stream      bool           `json:"stream"`
	EventStream bool           `json:"event_stream"`
	Opaque      bool           `json:"opaque"`
}

type specFlag struct {
	Name        string   `json:"name"`
	Shorthand   string   `json:"shorthand"`
	Type        string   `json:"type"`
	Default     string   `json:"default"`
	Enum        []string `json:"enum"`
	Inherited   bool     `json:"inherited"`
	Description string   `json:"description"`
}

type specConstraint struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	When    string   `json:"when"`
	Except  string   `json:"except"`
	Members []string `json:"members"`
	Code    string   `json:"code"`
}

type specPositional struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Variadic bool   `json:"variadic"`
}

type specErrorCode struct {
	ExitCode    int    `json:"exit_code"`
	Retryable   bool   `json:"retryable"`
	Description string `json:"description"`
}

type specExitCode struct {
	Code        int    `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func loadSpec(t *testing.T, got result) specDocument {
	t.Helper()
	requireExit(t, got, 0)
	var doc specDocument
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("decode spec: %v\n%s", err, got.stdout)
	}
	return doc
}

func (c specCommand) find(path string) (specCommand, bool) {
	if c.Path == path {
		return c, true
	}
	for _, child := range c.Children {
		if found, ok := child.find(path); ok {
			return found, true
		}
	}
	return specCommand{}, false
}

func (c specCommand) allPaths() []string {
	paths := []string{c.Path}
	for _, child := range c.Children {
		paths = append(paths, child.allPaths()...)
	}
	return paths
}

func flagByName(flags []specFlag, name string) (specFlag, bool) {
	for _, f := range flags {
		if f.Name == name {
			return f, true
		}
	}
	return specFlag{}, false
}

func TestSpecDescribesImplementedCommandTree(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	if doc.Name != "mitt" {
		t.Errorf("name = %q, want mitt", doc.Name)
	}
	if doc.Version == "" {
		t.Error("version must not be empty")
	}

	paths := doc.Command.allPaths()
	sort.Strings(paths)
	for _, want := range []string{"mitt", "mitt proxy", "mitt proxy status", "mitt spec"} {
		if !containsString(paths, want) {
			t.Errorf("command tree missing %q; got %v", want, paths)
		}
	}
	if containsString(paths, "mitt help") || containsString(paths, "mitt completion") {
		t.Errorf("cobra helper commands must not leak into spec: %v", paths)
	}

	var format specFlag
	for _, f := range doc.GlobalFlags {
		if f.Name == "format" {
			format = f
		}
	}
	if format.Name == "" {
		t.Fatal("global_flags must include --format")
	}
	if format.Shorthand != "f" {
		t.Errorf("--format shorthand = %q, want f", format.Shorthand)
	}
	if format.Type != "string" {
		t.Errorf("--format type = %q, want string", format.Type)
	}
	if got, want := strings.Join(format.Enum, ","), "json,table,yaml,md"; got != want {
		t.Errorf("--format enum = %q, want %q", got, want)
	}

	proxyStatus, ok := doc.Command.find("mitt proxy status")
	if !ok {
		t.Fatal("spec missing mitt proxy status")
	}
	if !containsString(proxyStatus.ErrorCodes, "NOT_RUNNING") {
		t.Errorf("proxy status error_codes = %v, want NOT_RUNNING", proxyStatus.ErrorCodes)
	}
	for _, name := range []string{"store", "format"} {
		f, ok := flagByName(proxyStatus.Flags, name)
		if !ok {
			t.Errorf("proxy status effective flags missing %q", name)
			continue
		}
		if !f.Inherited {
			t.Errorf("global flag %q on proxy status must be marked inherited", name)
		}
	}

	specCmd, ok := doc.Command.find("mitt spec")
	if !ok {
		t.Fatal("spec missing mitt spec")
	}
	if specCmd.OutputSchema == nil {
		t.Fatal("mitt spec must register a success output schema")
	}
	if got := specCmd.OutputSchema["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("spec output schema $schema = %v", got)
	}
}

// TestSpecRegistersProxyStartEventOutputSchema pins the default-silence
// declaration: proxy start has no default shape, and its event stream is a
// named, flag-selected mode with a real schema.
func TestSpecRegistersProxyStartEventOutputSchema(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	start, ok := doc.Command.find("mitt proxy start")
	if !ok {
		t.Fatal("spec missing mitt proxy start")
	}
	if start.OutputSchema != nil {
		t.Errorf("mitt proxy start default output_schema = %v, want null: the default is silence", start.OutputSchema)
	}
	events, ok := start.OutputSchemas["events"]
	if !ok {
		t.Fatalf("mitt proxy start output_schemas = %v, want events", start.OutputSchemas)
	}
	if events.Flag != "--events" || !events.EventStream {
		t.Errorf("proxy start events mode = %+v, want --events selected and event-streaming", events)
	}
	if got := events.Schema["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("proxy start events schema $schema = %v", got)
	}
	if id, _ := events.Schema["$id"].(string); id == "" {
		t.Error("proxy start events schema $id must not be empty")
	}
	requireSchemaFields(t, events.Schema, "id", "ts", "kind")
}

var specCommandPaths = []string{
	"mitt",
	"mitt ca",
	"mitt ca init",
	"mitt ca install",
	"mitt ca status",
	"mitt diff",
	"mitt history",
	"mitt proxy",
	"mitt proxy start",
	"mitt proxy status",
	"mitt proxy stop",
	"mitt send",
	"mitt show",
	"mitt skills",
	"mitt skills install",
	"mitt spec",
}

func TestSpecCoversWholeCommandTree(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	paths := doc.Command.allPaths()
	sort.Strings(paths)
	want := append([]string(nil), specCommandPaths...)
	sort.Strings(want)
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("spec command tree mismatch\n got: %v\nwant: %v", paths, want)
	}
	for _, path := range paths {
		command, _ := doc.Command.find(path)
		if strings.TrimSpace(command.Usage) == "" {
			t.Errorf("%s usage must not be empty", path)
		}
		if command.Name == "" {
			t.Errorf("%s name must not be empty", path)
		}
	}
}

func TestSpecEveryLeafCommandRegistersDraftOutputSchema(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	ids := map[string]string{}
	for _, path := range specCommandPaths {
		command, ok := doc.Command.find(path)
		if !ok {
			t.Fatalf("spec missing %q", path)
		}
		if len(command.Children) > 0 {
			continue
		}
		// A leaf either declares a default shape or states its default is
		// silence by publishing flag-selected shapes only.
		schemas := map[string]map[string]any{}
		if command.OutputSchema != nil {
			schemas["default"] = command.OutputSchema
		}
		for mode, output := range command.OutputSchemas {
			if output.Schema != nil {
				schemas[mode] = output.Schema
			}
		}
		if len(schemas) == 0 {
			t.Errorf("%s must register at least one output schema", path)
			continue
		}
		for mode, schema := range schemas {
			// A meta-schema mode publishes only a $ref to the draft, because
			// its output is a JSON Schema document rather than a derived shape.
			if ref, isRef := schema["$ref"]; isRef {
				if ref != "https://json-schema.org/draft/2020-12/schema" {
					t.Errorf("%s %s output schema $ref = %v", path, mode, ref)
				}
				continue
			}
			if got := schema["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
				t.Errorf("%s %s output schema $schema = %v", path, mode, got)
			}
			id, _ := schema["$id"].(string)
			if id == "" {
				t.Errorf("%s %s output schema $id must not be empty", path, mode)
				continue
			}
			if previous, duplicate := ids[id]; duplicate {
				t.Errorf("%s and %s share output schema $id %q", path, previous, id)
			}
			ids[id] = path
			if title, _ := schema["title"].(string); title == "" {
				t.Errorf("%s %s output schema title must not be empty", path, mode)
			}
		}
	}
}

func TestSpecCommandNodesCarryFlagAndArgumentContract(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	globalNames := map[string]bool{}
	for _, flag := range doc.GlobalFlags {
		globalNames[flag.Name] = true
	}
	for _, name := range []string{"format", "store"} {
		if !globalNames[name] {
			t.Errorf("global_flags missing --%s", name)
		}
	}
	if globalNames["json"] {
		t.Error("global_flags must not include --json; format is the only output switch")
	}

	for _, path := range specCommandPaths {
		command, ok := doc.Command.find(path)
		if !ok {
			t.Fatalf("spec missing %q", path)
		}
		if command.Args == nil {
			t.Errorf("%s args must be an array", path)
		}
		if command.Flags == nil {
			t.Errorf("%s flags must be an array", path)
		}
		seen := map[string]bool{}
		for _, flag := range command.Flags {
			if seen[flag.Name] {
				t.Errorf("%s lists --%s twice", path, flag.Name)
			}
			seen[flag.Name] = true
			if flag.Type == "" {
				t.Errorf("%s --%s type must not be empty", path, flag.Name)
			}
			if flag.Description == "" {
				t.Errorf("%s --%s description must not be empty", path, flag.Name)
			}
			if flag.Name == "help" {
				t.Errorf("%s must not expose cobra's --help flag", path)
			}
			if globalNames[flag.Name] && path != "mitt" && !flag.Inherited {
				t.Errorf("%s global --%s must be marked inherited", path, flag.Name)
			}
		}
	}

	assertArgs := func(path string, want ...specPositional) {
		t.Helper()
		command, ok := doc.Command.find(path)
		if !ok {
			t.Fatalf("spec missing %q", path)
		}
		if len(command.Args) != len(want) {
			t.Fatalf("%s args = %+v, want %+v", path, command.Args, want)
		}
		for i := range want {
			if command.Args[i] != want[i] {
				t.Errorf("%s args[%d] = %+v, want %+v", path, i, command.Args[i], want[i])
			}
		}
	}
	assertArgs("mitt show", specPositional{Name: "id", Required: true})
	assertArgs("mitt diff",
		specPositional{Name: "idA", Required: true},
		specPositional{Name: "idB", Required: true},
	)
	assertArgs("mitt send", specPositional{Name: "id", Required: false})
	assertArgs("mitt history")
	assertArgs("mitt spec")

	send, _ := doc.Command.find("mitt send")
	base, ok := constraintNamed(send.Constraints, "base")
	if !ok {
		t.Fatalf("send constraints = %v, want a base rule", send.Constraints)
	}
	if base.Kind != "one_of" {
		t.Errorf("base constraint kind = %q, want one_of", base.Kind)
	}
	if got, want := strings.Join(base.Members, ","), "<id>,--url,--from-file"; got != want {
		t.Errorf("base constraint members = %q, want %q", got, want)
	}
	if base.Code != "BASE_CONFLICT" {
		t.Errorf("base constraint code = %q, want BASE_CONFLICT", base.Code)
	}
	if base.Except != "--schema" {
		t.Errorf("base constraint except = %q, want --schema", base.Except)
	}

	wantEnums := map[string]struct {
		path string
		want []string
	}{
		"format":     {path: "mitt", want: []string{"json", "table", "yaml", "md"}},
		"filter":     {path: "mitt history", want: []string{"captured", "replayed", "sent", "mitm_error"}},
		"http2-alpn": {path: "mitt proxy start", want: []string{"h1", "h2", "h2,h1"}},
		"tls-profile": {
			path: "mitt send",
			want: []string{"standard", "chrome", "firefox"},
		},
	}
	for name, want := range wantEnums {
		command, ok := doc.Command.find(want.path)
		if !ok {
			t.Fatalf("spec missing %q", want.path)
		}
		flag, ok := flagByName(command.Flags, name)
		if !ok {
			t.Errorf("%s missing --%s", want.path, name)
			continue
		}
		if strings.Join(flag.Enum, ",") != strings.Join(want.want, ",") {
			t.Errorf("%s --%s enum = %v, want %v", want.path, name, flag.Enum, want.want)
		}
	}
}

func TestSpecFlagsMirrorRuntimeHelp(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	for _, path := range specCommandPaths {
		command, ok := doc.Command.find(path)
		if !ok {
			t.Fatalf("spec missing %q", path)
		}
		args := []string{"--help"}
		if path != "mitt" {
			args = append(strings.Fields(strings.TrimPrefix(path, "mitt ")), "--help")
		}
		got := env.run(t, args...)
		requireExit(t, got, 0)

		helpFlags := helpFlagNames(got.stdout)
		specFlags := make([]string, 0, len(command.Flags))
		for _, flag := range command.Flags {
			specFlags = append(specFlags, flag.Name)
		}
		sort.Strings(specFlags)
		if strings.Join(specFlags, ",") != strings.Join(helpFlags, ",") {
			t.Errorf("%s spec flags = %v, --help flags = %v", path, specFlags, helpFlags)
		}
	}
}

var helpFlagPattern = regexp.MustCompile(`^\s+(?:-\w,\s+)?--([a-zA-Z0-9-]+)`)

func helpFlagNames(help string) []string {
	seen := map[string]bool{}
	names := make([]string, 0)
	for _, line := range strings.Split(help, "\n") {
		match := helpFlagPattern.FindStringSubmatch(line)
		if match == nil || match[1] == "help" || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		names = append(names, match[1])
	}
	sort.Strings(names)
	return names
}

func TestSpecOutputSchemasValidateRealCommandOutput(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	schemaFor := func(path string) map[string]any {
		t.Helper()
		command, ok := doc.Command.find(path)
		if !ok {
			t.Fatalf("spec missing %q", path)
		}
		if command.OutputSchema == nil {
			t.Fatalf("%s has no output schema", path)
		}
		return command.OutputSchema
	}

	init := env.runWithStore(t, "ca", "init")
	requireExit(t, init, 0)
	requireSchemaValid(t, schemaFor("mitt ca init"), decodeJSONObject(t, init.stdout), "ca init output")

	status := env.runWithStore(t, "ca", "status")
	requireExit(t, status, 0)
	requireSchemaValid(t, schemaFor("mitt ca status"), decodeJSONObject(t, status.stdout), "ca status output")

	install := env.runWithStore(t, "ca", "install", "--output", filepath.Join(t.TempDir(), "mitt-ca.crt"))
	requireExit(t, install, 0)
	requireSchemaValid(t, schemaFor("mitt ca install"), decodeJSONObject(t, install.stdout), "ca install output")

	specOut := env.run(t, "spec")
	requireExit(t, specOut, 0)
	requireSchemaValid(t, schemaFor("mitt spec"), decodeJSONObject(t, specOut.stdout), "spec output")

	targetResponse := "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nworld"
	targetAddr, requests := startRawTarget(t, targetResponse)
	proxyAddr, proxy := env.startCaptureProxy(t, "--events")

	runningStatus := env.runWithStore(t, "proxy", "status")
	requireExit(t, runningStatus, 0)
	requireSchemaValid(t, schemaFor("mitt proxy status"), decodeJSONObject(t, runningStatus.stdout), "proxy status output")

	rawRequest := "GET http://" + targetAddr + "/contract HTTP/1.1\r\n" +
		"Host: " + targetAddr + "\r\n" +
		"Connection: close\r\n\r\n"
	_ = sendRaw(t, proxyAddr, rawRequest)
	waitObserved(t, requests)

	stopped := env.runWithStore(t, "proxy", "stop")
	requireExit(t, stopped, 0)
	requireSchemaValid(t, schemaFor("mitt proxy stop"), decodeJSONObject(t, stopped.stdout), "proxy stop output")
	proxy.waitExit(t, 5*time.Second)

	startNode, ok := doc.Command.find("mitt proxy start")
	if !ok {
		t.Fatal("spec missing mitt proxy start")
	}
	eventsMode, ok := startNode.OutputSchemas["events"]
	if !ok {
		t.Fatalf("mitt proxy start output_schemas = %v, want events", startNode.OutputSchemas)
	}
	proxyEvents := historyLines(proxy.stdout.String())
	if len(proxyEvents) == 0 {
		t.Fatal("proxy --events emitted no events")
	}
	for _, line := range proxyEvents {
		requireSchemaValid(t, eventsMode.Schema, decodeJSONObject(t, line), "proxy start event")
	}

	history := env.runWithStore(t, "history")
	requireExit(t, history, 0)
	summaries := historyLines(history.stdout)
	if len(summaries) == 0 {
		t.Fatal("history returned no entries")
	}
	var id string
	for _, line := range summaries {
		summary := decodeJSONObject(t, line)
		requireSchemaValid(t, schemaFor("mitt history"), summary, "history summary")
		if id == "" {
			id, _ = summary["id"].(string)
		}
	}
	if id == "" {
		t.Fatal("history summary has no id")
	}

	show := env.runWithStore(t, "show", id)
	requireExit(t, show, 0)
	requireSchemaValid(t, schemaFor("mitt show"), decodeJSONObject(t, show.stdout), "show output")

	showDetail := env.runWithStore(t, "show", id, "--detail")
	requireExit(t, showDetail, 0)
	showNode, ok := doc.Command.find("mitt show")
	if !ok {
		t.Fatal("spec missing mitt show")
	}
	detailMode, ok := showNode.OutputSchemas["detail"]
	if !ok {
		t.Fatalf("mitt show output_schemas = %v, want a detail mode", showNode.OutputSchemas)
	}
	if detailMode.Flag != "--detail" {
		t.Errorf("show detail selector = %q, want --detail", detailMode.Flag)
	}
	requireSchemaValid(t, detailMode.Schema, decodeJSONObject(t, showDetail.stdout), "show --detail output")

	send := env.runWithStore(t, "send", id)
	requireExit(t, send, 0)
	sendOut := decodeJSONObject(t, send.stdout)
	requireSchemaValid(t, schemaFor("mitt send"), sendOut, "send output")

	replayID, _ := sendOut["id"].(string)
	diff := env.runWithStore(t, "diff", id, replayID)
	requireExit(t, diff, 0)
	requireSchemaValid(t, schemaFor("mitt diff"), decodeJSONObject(t, diff.stdout), "diff output")

	diffAgainst := env.runWithStore(t, "send", id, "--diff-against")
	requireExit(t, diffAgainst, 0)
	diffAgainstOut := decodeJSONObject(t, diffAgainst.stdout)
	requireSchemaValid(t, schemaFor("mitt send"), diffAgainstOut, "send --diff-against output")
	summarySchema, ok := doc.Schemas["send_diff_against"].(map[string]any)
	if !ok {
		t.Fatal("spec schemas missing send_diff_against")
	}
	requireSchemaValid(t, summarySchema, diffAgainstOut["diff_against"], "send --diff-against summary")

	dryRun := env.runWithStore(t, "send", "--url", "http://"+targetAddr+"/dry", "--dry-run")
	requireExit(t, dryRun, 0)
	dryRunSchema, ok := doc.Schemas["send_dry_run"].(map[string]any)
	if !ok {
		t.Fatal("spec schemas missing send_dry_run")
	}
	dryRunOutput := decodeJSONObject(t, dryRun.stdout)
	requireSchemaValid(t, dryRunSchema, dryRunOutput, "send --dry-run output")

	sendNode, ok := doc.Command.find("mitt send")
	if !ok {
		t.Fatal("spec missing mitt send")
	}
	dryRunMode, ok := sendNode.OutputSchemas["dry-run"]
	if !ok {
		t.Fatal("mitt send missing dry-run output mode")
	}
	if dryRunMode.Flag != "--dry-run" {
		t.Errorf("send dry-run selector = %q, want --dry-run", dryRunMode.Flag)
	}
	requireSchemaValid(t, dryRunMode.Schema, dryRunOutput, "send --dry-run mode output")
}

func TestSpecRegistersFlagSelectedOutputs(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	send, ok := doc.Command.find("mitt send")
	if !ok {
		t.Fatal("spec missing mitt send")
	}
	if len(send.OutputSchemas) != 2 {
		t.Fatalf("mitt send output_schemas = %v, want dry-run and schema", send.OutputSchemas)
	}
	dryRun, ok := send.OutputSchemas["dry-run"]
	if !ok {
		t.Fatal("mitt send missing dry-run output schema")
	}
	if id := dryRun.Schema["$id"]; id != "https://mitt.dev/schema/send-dry-run.json" {
		t.Errorf("dry-run output $id = %v", id)
	}
	published, ok := doc.Schemas["send_dry_run"].(map[string]any)
	if !ok {
		t.Fatal("spec schemas missing send_dry_run")
	}
	if !reflect.DeepEqual(dryRun.Schema, published) {
		t.Error("send_dry_run registry entry and dry-run output schema must come from one declaration")
	}
	metaSchema, ok := send.OutputSchemas["schema"]
	if !ok {
		t.Fatal("mitt send missing schema output mode")
	}
	if ref := metaSchema.Schema["$ref"]; ref != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("schema output $ref = %v", ref)
	}
}

func TestSpecRegistersErrorAndExitCodeTables(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))

	wantCodes := map[string]struct {
		exit      int
		retryable bool
	}{
		"INVALID_REQUEST":      {2, false},
		"INVALID_PATCH":        {2, false},
		"BASE_CONFLICT":        {2, false},
		"ENTRY_NOT_FOUND":      {66, false},
		"NO_MATCH":             {66, false},
		"NOT_RUNNING":          {69, true},
		"LOCKED":               {69, true},
		"ADDR_IN_USE":          {69, false},
		"UPSTREAM_TLS":         {1, false},
		"CA_MISSING":           {78, false},
		"CERT_INSTALL_FAILED":  {78, false},
		"SKILL_EXISTS":         {73, false},
		"SKILL_INSTALL_FAILED": {78, false},
		"SCOPE_DENIED":         {77, false},
		"CLIENT_CERT_REJECTED": {77, false},
		"TEMP_FAILURE":         {75, true},
	}
	for code, want := range wantCodes {
		got, ok := doc.ErrorCodes[code]
		if !ok {
			t.Errorf("error_codes missing %s", code)
			continue
		}
		if got.ExitCode != want.exit {
			t.Errorf("%s exit_code = %d, want %d", code, got.ExitCode, want.exit)
		}
		if got.Retryable != want.retryable {
			t.Errorf("%s retryable = %v, want %v", code, got.Retryable, want.retryable)
		}
		if got.Description == "" {
			t.Errorf("%s description must not be empty", code)
		}
	}

	wantExits := []int{0, 1, 2, 66, 69, 73, 75, 77, 78, 130}
	registeredExits := map[int]bool{}
	for _, exit := range doc.ExitCodes {
		registeredExits[exit.Code] = true
		if exit.Name == "" || exit.Description == "" {
			t.Errorf("exit code %d missing name/description", exit.Code)
		}
	}
	for _, want := range wantExits {
		if !registeredExits[want] {
			t.Errorf("exit_codes missing %d", want)
		}
	}

	for code, info := range doc.ErrorCodes {
		if !registeredExits[info.ExitCode] {
			t.Errorf("error code %s uses unregistered exit code %d", code, info.ExitCode)
		}
	}
	for _, path := range specCommandPaths {
		command, ok := doc.Command.find(path)
		if !ok {
			t.Fatalf("spec missing %q", path)
		}
		for _, code := range command.ErrorCodes {
			if _, ok := doc.ErrorCodes[code]; !ok {
				t.Errorf("%s error_codes lists unregistered %s", path, code)
			}
		}
	}
}

func TestGlobalFlagsAcceptedBeforeAndAfterSubcommand(t *testing.T) {
	env := newTestEnv(t)

	before := env.run(t, "--store", env.store, "spec")
	requireExit(t, before, 0)
	after := env.run(t, "spec", "--store", env.store)
	requireExit(t, after, 0)

	var a, b map[string]any
	if err := json.Unmarshal([]byte(before.stdout), &a); err != nil {
		t.Fatalf("decode before: %v", err)
	}
	if err := json.Unmarshal([]byte(after.stdout), &b); err != nil {
		t.Fatalf("decode after: %v", err)
	}
	if a["name"] != b["name"] {
		t.Errorf("flag position changed output: %v vs %v", a["name"], b["name"])
	}
}

func constraintNamed(constraints []specConstraint, name string) (specConstraint, bool) {
	for _, constraint := range constraints {
		if constraint.Name == name {
			return constraint, true
		}
	}
	return specConstraint{}, false
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
