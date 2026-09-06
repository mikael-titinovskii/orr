package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	command := exec.Command(orrBinary, "version")
	command.Dir = t.TempDir()
	command.Env = isolatedEnvironment(command.Dir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("orr version: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "orr 0.2.0" {
		t.Fatalf("version output = %q, want %q", got, "orr 0.2.0")
	}
}

func TestCompletionCommandPrintsBashScript(t *testing.T) {
	command := exec.Command(orrBinary, "completion", "bash")
	command.Dir = t.TempDir()
	command.Env = isolatedEnvironment(command.Dir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("orr completion bash: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "complete -o default -F __start_orr orr") {
		t.Fatalf("unexpected completion output: %s", output)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	command := exec.Command(orrBinary, "not-a-command")
	command.Dir = t.TempDir()
	command.Env = isolatedEnvironment(command.Dir)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("unknown command succeeded: %s", output)
	}
	if !strings.Contains(string(output), `unknown command "not-a-command"`) {
		t.Fatalf("unexpected error output: %s", output)
	}
}

func TestResetCommandRemovesStateAndPreservesConfiguration(t *testing.T) {
	root := t.TempDir()
	providersPath := filepath.Join(root, "state", "providers.yaml")
	statsPath := filepath.Join(root, "orr", "stats.json")
	envPath := filepath.Join(root, ".env")
	dotenv := "OPENROUTER_API_KEY=do-not-print\nORR_PROVIDERS_FILE=state/providers.yaml\n"
	for path, contents := range map[string]string{
		providersPath: "version: 1\nmodels: {}\n",
		statsPath:     `{"version":1}`,
		envPath:       dotenv,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	command := exec.Command(orrBinary, "reset", "--env", envPath)
	command.Dir = root
	command.Env = isolatedEnvironment(root)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("orr reset: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "do-not-print") {
		t.Fatalf("reset output exposed dotenv contents: %s", output)
	}
	for _, path := range []string{providersPath, statsPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("reset left generated state at %s: %v", path, err)
		}
	}
	if got, err := os.ReadFile(envPath); err != nil || string(got) != dotenv {
		t.Fatalf("reset changed dotenv: contents=%q err=%v", got, err)
	}
}
