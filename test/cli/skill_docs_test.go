package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The Skill is distributed with the repository. These tests keep the prose
// aligned with the live command contract instead of trusting a hand-written
// table to stay current.
func readSkillDoc(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "skills", "mitt", rel))
	if err != nil {
		t.Fatalf("read skill document %s: %v", rel, err)
	}
	return string(data)
}

func markdownSection(content, heading string) string {
	start := strings.Index(content, heading)
	if start < 0 {
		return ""
	}
	rest := content[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

var backtickTokenPattern = regexp.MustCompile("`([^`]+)`")

func backtickTokens(cell string) []string {
	matches := backtickTokenPattern.FindAllStringSubmatch(cell, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match[1])
	}
	return out
}

func markdownCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func TestSkillMainPutsSafetyBeforeFirstRun(t *testing.T) {
	content := readSkillDoc(t, "SKILL.md")
	if !strings.HasPrefix(content, "---\n") {
		t.Fatal("SKILL.md must start with YAML frontmatter so it can be loaded as a skill")
	}
	safetyAt := strings.Index(content, "## ⚠️ 安全提醒")
	runAt := strings.Index(content, "## 快速开始")
	if safetyAt < 0 {
		t.Fatal("SKILL.md must contain a prominent `## ⚠️ 安全提醒` section")
	}
	if runAt < 0 {
		t.Fatal("SKILL.md must contain a `## 快速开始` section")
	}
	if safetyAt > runAt {
		t.Fatal("the safety section must appear before the first-run instructions")
	}

	safety := markdownSection(content, "## ⚠️ 安全提醒")
	for _, want := range []string{
		"0.0.0.0:8080",
		"--allow-targets",
		"--deny-targets",
		"--addr 127.0.0.1:8080",
		"ca.key.pem",
	} {
		if !strings.Contains(safety, want) {
			t.Errorf("safety section must mention %q", want)
		}
	}
}

var skillErrorRow = regexp.MustCompile(`^\|\s*` + "`" + `([A-Z_]+)` + "`" + `\s*\|\s*(\d+)\s*\|\s*(true|false)\s*\|`)

func TestSkillMainErrorTableCoversSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))
	content := readSkillDoc(t, "SKILL.md")
	section := markdownSection(content, "## 错误码表")
	if section == "" {
		t.Fatal("SKILL.md must contain a `## 错误码表` section")
	}

	type row struct {
		exit      string
		retryable string
	}
	rows := map[string]row{}
	for _, line := range strings.Split(section, "\n") {
		match := skillErrorRow.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		rows[match[1]] = row{exit: match[2], retryable: match[3]}
	}

	for code, info := range doc.ErrorCodes {
		got, ok := rows[code]
		if !ok {
			t.Errorf("error table is missing %s", code)
			continue
		}
		if got.exit != strconv.Itoa(info.ExitCode) {
			t.Errorf("%s exit code = %s, spec says %d", code, got.exit, info.ExitCode)
		}
		if got.retryable != boolString(info.Retryable) {
			t.Errorf("%s retryable = %s, spec says %t", code, got.retryable, info.Retryable)
		}
	}
	for code := range rows {
		if _, ok := doc.ErrorCodes[code]; !ok {
			t.Errorf("error table documents unregistered error code %s", code)
		}
	}
	for _, code := range []string{"NO_MATCH", "ADDR_IN_USE"} {
		if _, ok := rows[code]; !ok {
			t.Errorf("error table must explicitly include %s", code)
		}
	}
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

var (
	skillCommandHeading = regexp.MustCompile("(?m)^### `(mitt(?: [a-z]+)*)`$")
	skillExitRow        = regexp.MustCompile(`^\|\s*(\d+)\s*\|\s*([A-Z_]+)\s*\|`)
)

func skillCommandSections(t *testing.T, content string) map[string]string {
	t.Helper()
	matches := skillCommandHeading.FindAllStringSubmatchIndex(content, -1)
	sections := map[string]string{}
	for i, match := range matches {
		path := content[match[2]:match[3]]
		start := match[1]
		end := len(content)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		sections[path] = content[start:end]
	}
	return sections
}

func contractValue(section, field string) (string, bool) {
	for _, line := range strings.Split(section, "\n") {
		cells := markdownCells(line)
		if len(cells) >= 2 && cells[0] == field {
			return cells[1], true
		}
	}
	return "", false
}

func TestSkillCommandReferenceMatchesSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))
	content := readSkillDoc(t, "reference/commands.md")
	sections := skillCommandSections(t, content)

	paths := doc.Command.allPaths()
	sort.Strings(paths)
	for _, path := range paths {
		if _, ok := sections[path]; !ok {
			t.Errorf("reference/commands.md is missing a `### `%s`` section", path)
		}
	}
	for path := range sections {
		if !containsString(paths, path) {
			t.Errorf("reference/commands.md documents unknown command %q", path)
		}
	}

	for _, path := range paths {
		section, ok := sections[path]
		if !ok {
			continue
		}
		command, ok := doc.Command.find(path)
		if !ok {
			continue
		}

		positionals, ok := contractValue(section, "Positionals")
		if !ok {
			t.Errorf("%s: missing Positionals contract row", path)
		} else if err := checkPositionals(positionals, command.Args); err != nil {
			t.Errorf("%s: %v", path, err)
		}

		rules, ok := contractValue(section, "Constraints")
		if !ok {
			t.Errorf("%s: missing Constraints contract row", path)
		} else if err := checkConstraints(rules, command.Constraints); err != nil {
			t.Errorf("%s: %v", path, err)
		}

		codes, ok := contractValue(section, "Error codes")
		if !ok {
			t.Errorf("%s: missing Error codes contract row", path)
		} else if err := checkErrorCodes(codes, command.ErrorCodes); err != nil {
			t.Errorf("%s: %v", path, err)
		}

		documented, err := documentedFlags(section)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		for _, want := range command.Flags {
			got, ok := documented[want.Name]
			if !ok {
				t.Errorf("%s: flag table is missing --%s", path, want.Name)
				continue
			}
			if got.typ != want.Type {
				t.Errorf("%s --%s type = %q, spec says %q", path, want.Name, got.typ, want.Type)
			}
			if got.def != want.Default {
				t.Errorf("%s --%s default = %q, spec says %q", path, want.Name, got.def, want.Default)
			}
			if got.enum != strings.Join(want.Enum, ",") {
				t.Errorf("%s --%s enum = %q, spec says %q", path, want.Name, got.enum, strings.Join(want.Enum, ","))
			}
		}
		for name := range documented {
			if _, ok := flagByName(command.Flags, name); !ok {
				t.Errorf("%s: flag table documents unknown --%s", path, name)
			}
		}
	}
}

func TestSkillCommandReferenceGlobalTablesMatchSpec(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t, env.run(t, "spec"))
	content := readSkillDoc(t, "reference/commands.md")
	section := markdownSection(content, "## 错误码与退出码")
	if section == "" {
		t.Fatal("reference/commands.md must contain a `## 错误码与退出码` section")
	}

	errorRows := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		match := skillErrorRow.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		errorRows[match[1]] = match[2] + "/" + match[3]
	}
	for code, info := range doc.ErrorCodes {
		want := strconv.Itoa(info.ExitCode) + "/" + boolString(info.Retryable)
		if got, ok := errorRows[code]; !ok || got != want {
			t.Errorf("commands.md error %s = %q, spec says %q", code, got, want)
		}
	}
	for code := range errorRows {
		if _, ok := doc.ErrorCodes[code]; !ok {
			t.Errorf("commands.md documents unregistered error %s", code)
		}
	}

	exitRows := map[int]string{}
	for _, line := range strings.Split(section, "\n") {
		match := skillExitRow.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		code, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("parse exit code %q: %v", match[1], err)
		}
		exitRows[code] = match[2]
	}
	knownExit := map[int]bool{}
	for _, info := range doc.ExitCodes {
		knownExit[info.Code] = true
		if got, ok := exitRows[info.Code]; !ok || got != info.Name {
			t.Errorf("commands.md exit code %d = %q, spec says %q", info.Code, got, info.Name)
		}
	}
	for code := range exitRows {
		if !knownExit[code] {
			t.Errorf("commands.md documents unregistered exit code %d", code)
		}
	}
}

func checkPositionals(cell string, args []specPositional) error {
	if len(args) == 0 {
		if strings.Contains(cell, "<") {
			return fmt.Errorf("Positionals = %q, spec says none", cell)
		}
		return nil
	}
	for _, arg := range args {
		token := "<" + arg.Name + ">"
		if !strings.Contains(cell, token) {
			return fmt.Errorf("Positionals = %q, missing %s", cell, token)
		}
		if arg.Required && !strings.Contains(cell, "required") {
			return fmt.Errorf("Positionals = %q, %s must be marked required", cell, token)
		}
		if !arg.Required && !strings.Contains(cell, "optional") {
			return fmt.Errorf("Positionals = %q, %s must be marked optional", cell, token)
		}
	}
	return nil
}

// checkConstraints compares the documented rule names with the declared ones:
// the prose is a mirror of the contract, so a rule cannot be added, renamed or
// dropped in one place only.
func checkConstraints(cell string, constraints []specConstraint) error {
	got := map[string]bool{}
	for _, token := range backtickTokens(cell) {
		got[token] = true
	}
	want := map[string]bool{}
	for _, constraint := range constraints {
		want[constraint.Name] = true
	}
	for name := range want {
		if !got[name] {
			return fmt.Errorf("Constraints = %q, missing %s", cell, name)
		}
	}
	for name := range got {
		if name == "none" || name == "无" {
			continue
		}
		if !want[name] {
			return fmt.Errorf("Constraints = %q, documents unexpected %s", cell, name)
		}
	}
	return nil
}

func checkErrorCodes(cell string, codes []string) error {
	got := map[string]bool{}
	for _, token := range backtickTokens(cell) {
		if token == "none" || token == "无" {
			continue
		}
		got[token] = true
	}
	want := map[string]bool{}
	for _, code := range codes {
		want[code] = true
	}
	for code := range want {
		if !got[code] {
			return fmt.Errorf("Error codes = %q, missing %s", cell, code)
		}
	}
	for code := range got {
		if !want[code] {
			return fmt.Errorf("Error codes = %q, documents unexpected %s", cell, code)
		}
	}
	return nil
}

type documentedFlag struct {
	typ  string
	def  string
	enum string
}

func documentedFlags(section string) (map[string]documentedFlag, error) {
	rows := map[string]documentedFlag{}
	for _, line := range strings.Split(section, "\n") {
		cells := markdownCells(line)
		if len(cells) < 4 {
			continue
		}
		if !strings.HasPrefix(cells[0], "`--") {
			continue
		}
		name := strings.Trim(strings.TrimPrefix(cells[0], "`--"), "`")
		if name == "" || strings.Contains(name, " ") {
			continue
		}
		if _, exists := rows[name]; exists {
			return nil, fmt.Errorf("flag table lists --%s twice", name)
		}
		enum := backtickTokens(cells[3])
		rows[name] = documentedFlag{
			typ:  strings.Trim(cells[1], "`"),
			def:  normalizeDefault(strings.Trim(cells[2], "`")),
			enum: strings.Join(enum, ","),
		}
	}
	return rows, nil
}

// normalizeDefault lets the table render an empty string default as "" while
// the machine contract reports it as an empty string.
func normalizeDefault(value string) string {
	if value == `""` {
		return ""
	}
	return value
}

func TestSkillWorkflowsCoverRequiredScenarios(t *testing.T) {
	content := readSkillDoc(t, "reference/workflows.md")
	for _, want := range []string{
		"SQLi",
		"JWT",
		"IDOR",
		"mitt history",
		"mitt show",
		"--patch",
		"mitt diff",
		"--diff-against",
		"--dry-run",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("reference/workflows.md must cover %q", want)
		}
	}
}

func TestPlaywrightGuideCoversLanguagesTopologiesAndIsolation(t *testing.T) {
	content := readSkillDoc(t, "reference/playwright-integration.md")
	for _, want := range []string{
		"Node.js",
		"Python",
		"Go",
		"```js",
		"```python",
		"```go",
		"X-Repeater-Context",
		"extraHTTPHeaders",
		"extra_http_headers",
		"ExtraHttpHeaders",
		"http://127.0.0.1:8080",
		"同环境",
		"共享 network namespace",
		"--port-range",
		"--session-per-port",
		"--http2-alpn",
		"scope_denied",
		"LOCKED",
		"ca install",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("reference/playwright-integration.md must cover %q", want)
		}
	}
}
