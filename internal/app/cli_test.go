package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func executeCommand(t *testing.T, args ...string) error {
	t.Helper()
	var output bytes.Buffer
	root := newRootCommand(&output)
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(args)
	return root.Execute()
}

func executeForOutput(t *testing.T, args ...string) string {
	t.Helper()
	var output bytes.Buffer
	root := newRootCommand(&output)
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("orr %s: %v", strings.Join(args, " "), err)
	}
	return output.String()
}

func TestVersionIsReportedIdenticallyByCommandAndFlag(t *testing.T) {
	want := "orr " + Version + "\n"
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		if got := executeForOutput(t, args...); got != want {
			t.Errorf("orr %s = %q, want %q", strings.Join(args, " "), got, want)
		}
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	err := executeCommand(t, "not-a-command")
	if err == nil || !strings.Contains(err.Error(), `unknown command "not-a-command"`) {
		t.Fatalf("unknown command error = %v", err)
	}
}

func completionCandidates(t *testing.T, args ...string) []string {
	t.Helper()
	var candidates []string
	for _, line := range strings.Split(executeForOutput(t, append([]string{"__complete"}, args...)...), "\n") {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		candidates = append(candidates, strings.SplitN(line, "\t", 2)[0])
	}
	return candidates
}

func TestCompletionOffersEverySubcommand(t *testing.T) {
	var want []string
	for _, cmd := range newRootCommand(io.Discard).Commands() {
		want = append(want, cmd.Name())
	}
	got := completionCandidates(t, "")
	for _, name := range want {
		if !slices.Contains(got, name) {
			t.Errorf("completion omits subcommand %q, offered %v", name, got)
		}
	}
}

func TestCompletionOffersEveryFlagOfACommand(t *testing.T) {
	for _, name := range []string{"serve", "providers", "update", "reset", "integrate"} {
		var want []string
		for _, cmd := range newRootCommand(io.Discard).Commands() {
			if cmd.Name() != name {
				continue
			}
			cmd.Flags().VisitAll(func(f *pflag.Flag) { want = append(want, "--"+f.Name) })
		}
		got := completionCandidates(t, name, "--")
		for _, flag := range want {
			if !slices.Contains(got, flag) {
				t.Errorf("completion for %q omits %q, offered %v", name, flag, got)
			}
		}
	}
}

func TestCompletionOffersConfiguredModelsForProviders(t *testing.T) {
	root := t.TempDir()
	providersPath := filepath.Join(root, "providers.yaml")
	if err := os.WriteFile(providersPath, []byte("version: 1\nmodels:\n  author/one: {}\n  author/two: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(root, ".env")
	if err := os.WriteFile(envPath, []byte("ORR_PROVIDERS_FILE="+providersPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := completionCandidates(t, "providers", "--env", envPath, "")
	for _, model := range []string{"author/one", "author/two"} {
		if !slices.Contains(got, model) {
			t.Errorf("completion omits configured model %q, offered %v", model, got)
		}
	}
}

func TestCompletionDoesNotCreateAProvidersFile(t *testing.T) {
	root := t.TempDir()
	envPath := filepath.Join(root, ".env")
	if err := os.WriteFile(envPath, []byte("OPENROUTER_API_KEY=key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	completionCandidates(t, "providers", "--env", envPath, "")

	if _, err := os.Stat(filepath.Join(root, "providers.yaml")); !os.IsNotExist(err) {
		t.Fatalf("completing a model created the providers file: %v", err)
	}
}

func TestCompletionScriptsAreGeneratedForEveryShell(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		if script := executeForOutput(t, "completion", shell); !strings.Contains(script, "orr") {
			t.Errorf("%s completion script is empty", shell)
		}
	}
}

func TestCompletionRejectsUnsupportedShell(t *testing.T) {
	if err := executeCommand(t, "completion", "csh"); err == nil {
		t.Fatal("completion accepted an unsupported shell")
	}
}
