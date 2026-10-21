package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mikael-titinovskii/orr/internal/app"
	"github.com/mikael-titinovskii/orr/internal/store"
)

func TestVersionCommand(t *testing.T) {
	command := exec.Command(orrBinary, "version")
	command.Dir = t.TempDir()
	command.Env = isolatedEnvironment(command.Dir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("orr version: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "orr "+app.Version {
		t.Fatalf("version output = %q, want %q", got, "orr "+app.Version)
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
	dbPath := filepath.Join(root, "state", "orr.db")
	envPath := filepath.Join(root, ".env")
	dotenv := "OPENROUTER_API_KEY=do-not-print\nORR_DB_FILE=state/orr.db\n"
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open seeded database: %v", err)
	}
	if err := db.SaveModels(map[string]store.Model{"author/one": {}}); err != nil {
		t.Fatalf("seed models: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded database: %v", err)
	}
	if err := os.WriteFile(envPath, []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
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
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("reset left generated state at %s: %v", path, err)
		}
	}
	if got, err := os.ReadFile(envPath); err != nil || string(got) != dotenv {
		t.Fatalf("reset changed dotenv: contents=%q err=%v", got, err)
	}
}
