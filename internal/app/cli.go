package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
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
		newUpdateCommand(output),
		newResetCommand(output),
		newUpgradeCommand(output),
		newIntegrateCommand(output),
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

func addEnvFlag(cmd *cobra.Command, envPath *string) {
	cmd.Flags().StringVar(envPath, "env", defaultEnvPath(), "path to a dotenv file")
	_ = cmd.MarkFlagFilename("env")
}

func newServeCommand() *cobra.Command {
	var envPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "run the routing proxy",
		Args:  noPositionalArgs("serve"),
		RunE: func(*cobra.Command, []string) error {
			cfg, err := loadConfig(envPath)
			if err != nil {
				return err
			}
			return serve(cfg)
		},
	}
	addEnvFlag(cmd, &envPath)
	return cmd
}

func newProvidersCommand(output io.Writer) *cobra.Command {
	var envPath string
	cmd := &cobra.Command{
		Use:   "providers <author/model>",
		Short: "list compatible endpoints for a model",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return errors.New("usage: orr providers [--env .env] <author/model>")
			}
			return nil
		},
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return configuredModels(envPath), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(_ *cobra.Command, args []string) error {
			return runProviders(envPath, args[0], output)
		},
	}
	addEnvFlag(cmd, &envPath)
	return cmd
}

func newUpdateCommand(output io.Writer) *cobra.Command {
	var (
		envPath      string
		maxProviders int
		cacheOnly    bool
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "refresh every model provider order",
		Args:  noPositionalArgs("update"),
		RunE: func(*cobra.Command, []string) error {
			return runUpdate(envPath, maxProviders, cacheOnly, output)
		},
	}
	addEnvFlag(cmd, &envPath)
	cmd.Flags().IntVar(&maxProviders, "max", 20, "maximum providers per model")
	cmd.Flags().BoolVar(&cacheOnly, "cache-only", false, "only select endpoints that support prompt caching")
	return cmd
}

func newResetCommand(output io.Writer) *cobra.Command {
	var envPath string
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "delete generated routing state",
		Args:  noPositionalArgs("reset"),
		RunE: func(*cobra.Command, []string) error {
			return runReset(envPath, output)
		},
	}
	addEnvFlag(cmd, &envPath)
	return cmd
}

func newUpgradeCommand(output io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "upgrade orr to the latest version",
		Args:  noPositionalArgs("upgrade"),
		RunE: func(*cobra.Command, []string) error {
			return runUpgrade(output)
		},
	}
}

func newIntegrateCommand(output io.Writer) *cobra.Command {
	var envPath string
	cmd := &cobra.Command{
		Use:   "integrate",
		Short: "point Kimi Code and OpenCode at orr",
		Args:  noPositionalArgs("integrate"),
		RunE: func(*cobra.Command, []string) error {
			return runIntegrate(envPath, output)
		},
	}
	addEnvFlag(cmd, &envPath)
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

// Read directly: loadConfig creates the providers file when absent.
func configuredModels(envPath string) []string {
	fileEnv, err := readDotEnv(envPath)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(configuredProvidersPath(envPath, fileEnv))
	if err != nil {
		return nil
	}
	var saved providersFile
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&saved); err != nil {
		return nil
	}
	models := make([]string, 0, len(saved.Models))
	for model := range saved.Models {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}
