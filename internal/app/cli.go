package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/mikael-titinovskii/orr/internal/store"
	"github.com/spf13/cobra"
)

func newRootCommand(output io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "orr",
		Short:         "orr routes OpenRouter requests through a selected provider",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(output)
	root.SetVersionTemplate("orr {{.Version}}\n")
	root.AddCommand(
		newServeCommand(),
		newProvidersCommand(output),
		newResetCommand(output),
		newUpgradeCommand(output),
		newIntegrateCommand(output),
		newGuardrailsCommand(output),
		newVersionCommand(output),
	)
	root.InitDefaultCompletionCmd()
	rejectUnknownShell(root)
	return root
}

// Cobra's generated completion command exits 0 on an unknown shell.
func rejectUnknownShell(root *cobra.Command) {
	for _, cmd := range root.Commands() {
		if cmd.Name() == "completion" {
			cmd.Args = cobra.NoArgs
			cmd.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
		}
	}
}

func Run(args []string) error {
	root := newRootCommand(os.Stdout)
	root.SetArgs(args)
	return root.Execute()
}

func addEnvFlags(cmd *cobra.Command, envPath *string, preferEnvFile *bool) {
	cmd.Flags().StringVar(envPath, "env", defaultEnvPath(), "path to a dotenv file")
	cmd.Flags().BoolVar(preferEnvFile, "prefer-env-file", false, "prefer dotenv values over process environment variables")
	_ = cmd.MarkFlagFilename("env")
}

func newServeCommand() *cobra.Command {
	var (
		noTUI         bool
		envPath       string
		preferEnvFile bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "run the routing proxy",
		Args:  noPositionalArgs("serve"),
		RunE: func(*cobra.Command, []string) error {
			cfg, err := loadConfigWithOptions(envPath, preferEnvFile)
			if err != nil {
				return err
			}
			if noTUI {
				disabled := false
				cfg.TUI = &disabled
			}
			return serve(cfg)
		},
	}
	addEnvFlags(cmd, &envPath, &preferEnvFile)
	cmd.Flags().BoolVar(&noTUI, "no-tui", false, "disable the terminal dashboard")
	return cmd
}

func newProvidersCommand(output io.Writer) *cobra.Command {
	var (
		envPath       string
		preferEnvFile bool
	)
	cmd := &cobra.Command{
		Use:   "providers <author/model>",
		Short: "list compatible endpoints for a model",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return errors.New("usage: orr providers [--env .env] [--prefer-env-file] <author/model>")
			}
			return nil
		},
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return configuredModelsWithOptions(envPath, preferEnvFile), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(_ *cobra.Command, args []string) error {
			return runProvidersWithOptions(envPath, preferEnvFile, args[0], output)
		},
	}
	addEnvFlags(cmd, &envPath, &preferEnvFile)
	return cmd
}

func newResetCommand(output io.Writer) *cobra.Command {
	var (
		envPath       string
		preferEnvFile bool
	)
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "delete the local database",
		Args:  noPositionalArgs("reset"),
		RunE: func(*cobra.Command, []string) error {
			return runResetWithOptions(envPath, preferEnvFile, output)
		},
	}
	addEnvFlags(cmd, &envPath, &preferEnvFile)
	return cmd
}

func newUpgradeCommand(output io.Writer) *cobra.Command {
	var (
		envPath       string
		preferEnvFile bool
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "upgrade orr to the latest version",
		Args:  noPositionalArgs("upgrade"),
		RunE: func(*cobra.Command, []string) error {
			return runUpgrade(output, envPath, preferEnvFile)
		},
	}
	addEnvFlags(cmd, &envPath, &preferEnvFile)
	return cmd
}

func newIntegrateCommand(output io.Writer) *cobra.Command {
	var (
		envPath       string
		preferEnvFile bool
	)
	cmd := &cobra.Command{
		Use:   "integrate",
		Short: "point Kimi Code and OpenCode at orr",
		Args:  noPositionalArgs("integrate"),
		RunE: func(*cobra.Command, []string) error {
			return runIntegrateWithOptions(envPath, preferEnvFile, output)
		},
	}
	addEnvFlags(cmd, &envPath, &preferEnvFile)
	return cmd
}

func newGuardrailsCommand(output io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guardrails",
		Short: "manage the optional NeMo Guardrails sidecar",
		Args:  noPositionalArgs("guardrails"),
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newGuardrailsInstallCommand(output),
		newGuardrailsStatusCommand(output),
		newGuardrailsConfigCommand(output),
	)
	return cmd
}

func newGuardrailsInstallCommand(output io.Writer) *cobra.Command {
	var (
		envPath       string
		preferEnvFile bool
		python        string
		force         bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "create the Python environment for the guardrails sidecar",
		Args:  noPositionalArgs("guardrails install"),
		RunE: func(*cobra.Command, []string) error {
			return runGuardrailsInstall(envPath, preferEnvFile, guardrailsInstallOptions{
				Python: python,
				Force:  force,
				Output: output,
			})
		},
	}
	addEnvFlags(cmd, &envPath, &preferEnvFile)
	cmd.Flags().StringVar(&python, "python", "", "path to a Python 3.11 through 3.13 interpreter")
	cmd.Flags().BoolVar(&force, "force", false, "reinstall the environment and reset the policy")
	return cmd
}

func newGuardrailsStatusCommand(output io.Writer) *cobra.Command {
	var (
		envPath       string
		preferEnvFile bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "show the guardrails installation",
		Args:  noPositionalArgs("guardrails status"),
		RunE: func(*cobra.Command, []string) error {
			settings, err := loadGuardrailsConfigWithOptions(envPath, preferEnvFile)
			if err != nil {
				return err
			}
			return runGuardrailsStatus(settings, output)
		},
	}
	addEnvFlags(cmd, &envPath, &preferEnvFile)
	return cmd
}

func newGuardrailsConfigCommand(output io.Writer) *cobra.Command {
	var (
		envPath       string
		preferEnvFile bool
	)
	cmd := &cobra.Command{
		Use:   "config",
		Short: "print the active guardrails policy",
		Args:  noPositionalArgs("guardrails config"),
		RunE: func(*cobra.Command, []string) error {
			settings, err := loadGuardrailsConfigWithOptions(envPath, preferEnvFile)
			if err != nil {
				return err
			}
			return runGuardrailsConfig(settings, output)
		},
	}
	addEnvFlags(cmd, &envPath, &preferEnvFile)
	return cmd
}

func newVersionCommand(output io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print the version",
		Args:  noPositionalArgs("version"),
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintln(output, "orr", Version)
			return nil
		},
	}
}

func noPositionalArgs(name string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("%s accepts no positional arguments", name)
		}
		return nil
	}
}

// configuredModelsWithOptions lists the configured models for shell completion.
// It opens the database read-only and never creates it: completion runs on every
// tab press, and a missing file simply offers nothing.
func configuredModelsWithOptions(envPath string, preferEnvFile bool) []string {
	fileEnv, err := readDotEnv(envPath)
	if err != nil {
		return nil
	}
	path, err := configuredDBPathWithOptions(envPath, fileEnv, preferEnvFile)
	if err != nil {
		return nil
	}
	db, err := store.OpenReadOnly(path)
	if err != nil {
		return nil
	}
	defer db.Close()
	models, err := db.Models()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(models))
	for model := range models {
		names = append(names, model)
	}
	sort.Strings(names)
	return names
}
