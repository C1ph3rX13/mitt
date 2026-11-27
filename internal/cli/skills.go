package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/skillinstall"
	"github.com/C1ph3rX13/mitt/internal/spec"
	bundled "github.com/C1ph3rX13/mitt/skills"
	"github.com/spf13/cobra"
)

const defaultSkillsDir = "~/.agents/skills"

type skillsInstallResult struct {
	SkillsDir string                `json:"skills_dir"`
	Skills    []skillinstall.Result `json:"skills"`
}

func newSkillsCmd(opts *options, stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Install the agent skill documentation bundled with mitt",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	spec.Declare(cmd, spec.Contract{Args: []spec.Positional{}})
	cmd.AddCommand(newSkillsInstallCmd(opts, stdout))
	return cmd
}

func newSkillsInstallCmd(opts *options, stdout io.Writer) *cobra.Command {
	var dest string
	var force bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the bundled agent skills into a skill root",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillsInstall(opts, dest, force, stdout)
		},
	}
	cmd.Flags().StringVar(&dest, "dest", defaultSkillsDir, "skill root directory (one subdirectory per skill)")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing skill directory that differs")
	spec.Declare(cmd, spec.Contract{
		Args: []spec.Positional{},
		Errors: []string{
			output.CodeInvalidRequest,
			output.CodeSkillExists,
			output.CodeSkillInstallFailed,
			output.CodeInternalError,
		},
		Outputs: []spec.Output{
			spec.Schema[skillsInstallResult]("skills-install", "skills install"),
		},
	})
	return cmd
}

func runSkillsInstall(opts *options, dest string, force bool, stdout io.Writer) error {
	if strings.TrimSpace(dest) == "" {
		return output.NewError(
			output.CodeInvalidRequest,
			"--dest must not be empty",
			"pass a skill root directory such as ~/.agents/skills",
		)
	}
	root, err := expandPath(dest)
	if err != nil {
		return output.NewError(
			output.CodeInvalidRequest,
			fmt.Sprintf("resolve --dest %q: %v", dest, err),
			"pass a writable directory path",
		)
	}
	results, err := skillinstall.Install(bundled.FS, root, force)
	if err != nil {
		return err
	}
	return opts.writer().Object(skillsInstallResult{
		SkillsDir: root,
		Skills:    results,
	})
}
