package cli

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/C1ph3rX13/mitt/internal/ca"
	"github.com/C1ph3rX13/mitt/internal/output"
	"github.com/C1ph3rX13/mitt/internal/spec"
	"github.com/spf13/cobra"
)

type caInitResult struct {
	CAPEM    string `json:"ca_pem"`
	CAKeyPEM string `json:"ca_key_pem"`
	Subject  string `json:"subject"`
	NotAfter string `json:"not_after" schema:"format=date-time"`
}

type caInstallResult struct {
	InstalledTo []string `json:"installed_to" schema:"item_enum=system|nss"`
	OutputPath  string   `json:"output_path"`
	SHA256      string   `json:"sha256" schema:"pattern=^[a-f0-9]{64}$"`
	Note        string   `json:"note"`
}

type caStatusResult struct {
	CAExists       bool     `json:"ca_exists"`
	CAPath         string   `json:"ca_path"`
	Installed      bool     `json:"installed"`
	InstallTargets []string `json:"install_targets" schema:"item_enum=system|nss"`
}

func newCACmd(opts *options, stdout io.Writer) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "ca",
		Short: "Generate, install and inspect the root CA used for HTTPS interception",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.PersistentFlags().StringVar(&dir, "dir", "", "CA directory (default <store>/ca)")
	spec.Declare(cmd, spec.Contract{Args: []spec.Positional{}})
	cmd.AddCommand(newCAInitCmd(opts, &dir, stdout))
	cmd.AddCommand(newCAInstallCmd(opts, &dir, stdout))
	cmd.AddCommand(newCAStatusCmd(opts, &dir, stdout))
	return cmd
}

func newCAInitCmd(opts *options, dir *string, stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a root CA or reuse the existing one",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCAInit(opts, *dir, stdout)
		},
	}
	spec.Declare(cmd, spec.Contract{
		Args:    []spec.Positional{},
		Errors:  []string{output.CodeCAMissing, output.CodeInternalError},
		Outputs: []spec.Output{spec.Schema[caInitResult]("ca-init", "ca init")},
	})
	return cmd
}

func newCAInstallCmd(opts *options, dir *string, stdout io.Writer) *cobra.Command {
	var outputPath string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the root CA into the environment trust stores",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCAInstall(opts, *dir, outputPath, stdout)
		},
	}
	cmd.Flags().StringVar(&outputPath, "output", "", "export the CA PEM to <path> instead of installing it")
	spec.Declare(cmd, spec.Contract{
		Args: []spec.Positional{},
		Errors: []string{
			output.CodeCAMissing,
			output.CodeCertInstallFailed,
			output.CodeInternalError,
		},
		Outputs: []spec.Output{spec.Schema[caInstallResult]("ca-install", "ca install")},
	})
	return cmd
}

func newCAStatusCmd(opts *options, dir *string, stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the root CA exists and is installed",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCAStatus(opts, *dir, stdout)
		},
	}
	spec.Declare(cmd, spec.Contract{
		Args:    []spec.Positional{},
		Errors:  []string{output.CodeCAMissing, output.CodeInternalError},
		Outputs: []spec.Output{spec.Schema[caStatusResult]("ca-status", "ca status")},
	})
	return cmd
}

func runCAInit(opts *options, explicitDir string, stdout io.Writer) error {
	dir, err := opts.caDir(explicitDir)
	if err != nil {
		return err
	}
	authority, err := ca.Init(dir)
	if err != nil {
		return caError(dir, "initialize", err)
	}
	return opts.writer().Object(caInitResult{
		CAPEM:    string(authority.CertPEM),
		CAKeyPEM: string(authority.KeyPEM),
		Subject:  ca.Subject,
		NotAfter: authority.Cert.NotAfter.UTC().Format(time.RFC3339),
	})
}

func runCAInstall(opts *options, explicitDir, outputArg string, stdout io.Writer) error {
	dir, err := opts.caDir(explicitDir)
	if err != nil {
		return err
	}
	authority, err := ca.Load(dir)
	if err != nil {
		return caError(dir, "install", err)
	}
	exportPath := ""
	if outputArg != "" {
		if exportPath, err = expandPath(outputArg); err != nil {
			return output.NewError(
				output.CodeCertInstallFailed,
				fmt.Sprintf("resolve --output path %q: %v", outputArg, err),
				"pass a valid file path, or omit --output to install into the system trust store",
			).WithDetail("ca_path", dir)
		}
	}
	result, err := ca.Install(authority, exportPath)
	if err != nil {
		return caInstallError(dir, exportPath, err)
	}
	return opts.writer().Object(caInstallResult{
		InstalledTo: result.InstalledTo,
		OutputPath:  result.OutputPath,
		SHA256:      result.SHA256,
		Note:        result.Note,
	})
}

func runCAStatus(opts *options, explicitDir string, stdout io.Writer) error {
	dir, err := opts.caDir(explicitDir)
	if err != nil {
		return err
	}
	authority, err := ca.Load(dir)
	if err != nil {
		return caError(dir, "inspect", err)
	}
	targets := ca.InstalledTargets(authority)
	return opts.writer().Object(caStatusResult{
		CAExists:       true,
		CAPath:         dir,
		Installed:      len(targets) > 0,
		InstallTargets: targets,
	})
}

func caInstallError(dir, exportPath string, err error) error {
	hint := "run `mitt ca install --output <path>` to export the PEM and install it manually"
	appErr := output.NewError(
		output.CodeCertInstallFailed,
		fmt.Sprintf("install CA: %v", err),
		hint,
	).WithDetail("ca_path", dir)
	if exportPath != "" {
		appErr = appErr.WithDetail("output_path", exportPath)
	}
	return appErr
}

func caError(dir, action string, err error) error {
	if errors.Is(err, ca.ErrNotFound) || errors.Is(err, ca.ErrInvalid) {
		message := "CA does not exist"
		reason := "missing"
		if !errors.Is(err, ca.ErrNotFound) {
			message = "CA is invalid or incomplete"
			reason = "invalid"
		}
		return output.NewError(
			output.CodeCAMissing,
			message,
			"back up the CA directory, remove it, then re-run `mitt ca init`; or pass --dir <dir> to use a new directory",
		).WithDetail("ca_path", dir).WithDetail("reason", reason)
	}
	return output.NewError(
		output.CodeInternalError,
		fmt.Sprintf("%s CA: %v", action, err),
		"check permissions on the CA directory",
	).WithDetail("ca_path", dir)
}
