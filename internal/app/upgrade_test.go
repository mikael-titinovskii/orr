package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpgradeRejectsArgumentsBeforeRunningCommands(t *testing.T) {
	err := executeCommand(t, "upgrade", "unexpected")
	if err == nil || !strings.Contains(err.Error(), "accepts no positional arguments") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReplaceExecutable(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "new")
	destination := filepath.Join(directory, "orr")
	if err := os.WriteFile(source, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceExecutable(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Fatalf("destination contains %q", got)
	}
}

func TestIsOrrRepository(t *testing.T) {
	directory := t.TempDir()
	if isOrrRepository(directory) {
		t.Fatal("empty directory identified as orr repository")
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module example.com/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "cmd", "orr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if isOrrRepository(directory) {
		t.Fatal("unrelated Go module identified as orr repository")
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module github.com/mikael-titinovskii/orr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isOrrRepository(directory) {
		t.Fatal("orr repository was not recognized")
	}
}
