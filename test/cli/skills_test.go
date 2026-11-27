package cli_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bundled "github.com/C1ph3rX13/mitt/skills"
)

var bundledSkillFiles = []string{
	"SKILL.md",
	"reference/commands.md",
	"reference/playwright-integration.md",
	"reference/workflows.md",
}

func skillsResults(t *testing.T, output map[string]any) []map[string]any {
	t.Helper()
	raw, ok := output["skills"].([]any)
	if !ok {
		t.Fatalf("skills output missing skills array: %v", output)
	}
	results := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("skills entry is not an object: %T", item)
		}
		results = append(results, entry)
	}
	return results
}

func onlySkillResult(t *testing.T, output map[string]any) map[string]any {
	t.Helper()
	results := skillsResults(t, output)
	if len(results) != 1 {
		t.Fatalf("skills results = %d, want 1: %v", len(results), results)
	}
	return results[0]
}

func TestSkillsInstallWritesBundledTree(t *testing.T) {
	env := newTestEnv(t)
	dest := filepath.Join(t.TempDir(), "skills")

	got := env.run(t, "skills", "install", "--dest", dest)

	requireExit(t, got, 0)
	output := decodeJSONObject(t, got.stdout)
	if output["skills_dir"] != dest {
		t.Errorf("skills_dir = %v, want %s", output["skills_dir"], dest)
	}
	entry := onlySkillResult(t, output)
	if entry["name"] != "mitt" {
		t.Errorf("name = %v, want mitt", entry["name"])
	}
	wantPath := filepath.Join(dest, "mitt")
	if entry["path"] != wantPath {
		t.Errorf("path = %v, want %s", entry["path"], wantPath)
	}
	if entry["action"] != "installed" {
		t.Errorf("action = %v, want installed", entry["action"])
	}

	for _, rel := range bundledSkillFiles {
		installedPath := filepath.Join(wantPath, filepath.FromSlash(rel))
		want, err := os.ReadFile(filepath.Join(repoRoot, "skills", "mitt", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read repository skill file %s: %v", rel, err)
		}
		got, err := os.ReadFile(installedPath)
		if err != nil {
			t.Fatalf("read installed skill file %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("installed %s does not match the repository copy", rel)
		}
	}
	assertMode(t, wantPath, 0o755)
	assertMode(t, filepath.Join(wantPath, "SKILL.md"), 0o644)

	doc := loadSpec(t, env.run(t, "spec"))
	command, ok := doc.Command.find("mitt skills install")
	if !ok {
		t.Fatal("spec missing mitt skills install")
	}
	requireSchemaValid(t, command.OutputSchema, output, "skills install output")
}

func TestSkillsInstallDefaultsToAgentsSkills(t *testing.T) {
	env := newTestEnv(t)

	got := env.run(t, "skills", "install")

	requireExit(t, got, 0)
	output := decodeJSONObject(t, got.stdout)
	wantDir := filepath.Join(env.home, ".agents", "skills")
	if output["skills_dir"] != wantDir {
		t.Errorf("skills_dir = %v, want %s", output["skills_dir"], wantDir)
	}
	if _, err := os.Stat(filepath.Join(wantDir, "mitt", "SKILL.md")); err != nil {
		t.Errorf("default install missing SKILL.md: %v", err)
	}
}

func TestSkillsInstallExpandsTildeDest(t *testing.T) {
	env := newTestEnv(t)

	got := env.run(t, "skills", "install", "--dest", "~/custom-skills")

	requireExit(t, got, 0)
	output := decodeJSONObject(t, got.stdout)
	wantDir := filepath.Join(env.home, "custom-skills")
	if output["skills_dir"] != wantDir {
		t.Errorf("skills_dir = %v, want %s", output["skills_dir"], wantDir)
	}
	if _, err := os.Stat(filepath.Join(wantDir, "mitt", "SKILL.md")); err != nil {
		t.Errorf("tilde install missing SKILL.md: %v", err)
	}
}

func TestSkillsInstallIsIdempotent(t *testing.T) {
	env := newTestEnv(t)
	dest := filepath.Join(t.TempDir(), "skills")
	requireExit(t, env.run(t, "skills", "install", "--dest", dest), 0)

	skillFile := filepath.Join(dest, "mitt", "SKILL.md")
	before, err := os.Stat(skillFile)
	if err != nil {
		t.Fatalf("stat installed SKILL.md: %v", err)
	}

	got := env.run(t, "skills", "install", "--dest", dest)

	requireExit(t, got, 0)
	output := decodeJSONObject(t, got.stdout)
	if action := onlySkillResult(t, output)["action"]; action != "unchanged" {
		t.Errorf("action = %v, want unchanged", action)
	}
	after, err := os.Stat(skillFile)
	if err != nil {
		t.Fatalf("stat installed SKILL.md after rerun: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("an identical install must not rewrite the installed files")
	}
}

func TestSkillsInstallRefusesChangedSkillWithoutForce(t *testing.T) {
	env := newTestEnv(t)
	dest := filepath.Join(t.TempDir(), "skills")
	requireExit(t, env.run(t, "skills", "install", "--dest", dest), 0)

	skillFile := filepath.Join(dest, "mitt", "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("locally edited\n"), 0o644); err != nil {
		t.Fatalf("edit installed SKILL.md: %v", err)
	}

	got := env.run(t, "skills", "install", "--dest", dest)

	requireExit(t, got, 73)
	errObj := errorEnvelope(t, got)
	if errObj["code"] != "SKILL_EXISTS" {
		t.Errorf("error.code = %v, want SKILL_EXISTS", errObj["code"])
	}
	if hint, _ := errObj["hint"].(string); !strings.Contains(hint, "--force") {
		t.Errorf("error.hint = %q, want a --force hint", hint)
	}
	if content := readFileString(t, skillFile); content != "locally edited\n" {
		t.Errorf("refused install must not touch the edited file, got %q", content)
	}

	forced := env.run(t, "skills", "install", "--dest", dest, "--force")

	requireExit(t, forced, 0)
	output := decodeJSONObject(t, forced.stdout)
	if action := onlySkillResult(t, output)["action"]; action != "updated" {
		t.Errorf("action = %v, want updated", action)
	}
	if content := readFileString(t, skillFile); !strings.Contains(content, "name: mitt") {
		t.Error("forced install did not restore the bundled SKILL.md")
	}
}

func TestSkillsInstallForceRemovesExtraFiles(t *testing.T) {
	env := newTestEnv(t)
	dest := filepath.Join(t.TempDir(), "skills")
	requireExit(t, env.run(t, "skills", "install", "--dest", dest), 0)

	extra := filepath.Join(dest, "mitt", "reference", "extra.md")
	if err := os.WriteFile(extra, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	got := env.run(t, "skills", "install", "--dest", dest)
	requireExit(t, got, 73)
	if errObj := errorEnvelope(t, got); errObj["code"] != "SKILL_EXISTS" {
		t.Errorf("error.code = %v, want SKILL_EXISTS", errObj["code"])
	}

	requireExit(t, env.run(t, "skills", "install", "--dest", dest, "--force"), 0)
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Errorf("forced install left an extra file behind, stat err = %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dest, ".mitt-skill-*"))
	if err != nil {
		t.Fatalf("glob install leftovers: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("install left temporary paths behind: %v", leftovers)
	}
}

func TestSkillsInstallRejectsDestFile(t *testing.T) {
	env := newTestEnv(t)
	dest := filepath.Join(t.TempDir(), "skills")
	if err := os.WriteFile(dest, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("write dest file: %v", err)
	}

	got := env.run(t, "skills", "install", "--dest", dest)

	requireExit(t, got, 2)
	if errObj := errorEnvelope(t, got); errObj["code"] != "INVALID_REQUEST" {
		t.Errorf("error.code = %v, want INVALID_REQUEST", errObj["code"])
	}
}

func TestEmbeddedSkillMatchesRepository(t *testing.T) {
	seen := map[string]bool{}
	err := fs.WalkDir(bundled.FS, "mitt", func(rel string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		seen[rel] = true
		want, err := os.ReadFile(filepath.Join(repoRoot, "skills", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read repository skill file %s: %v", rel, err)
		}
		got, err := fs.ReadFile(bundled.FS, rel)
		if err != nil {
			t.Fatalf("read embedded skill file %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("embedded %s does not match skills/%s", rel, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded skills: %v", err)
	}
	for _, rel := range bundledSkillFiles {
		if !seen["mitt/"+rel] {
			t.Errorf("embedded skill bundle is missing mitt/%s", rel)
		}
	}
}
