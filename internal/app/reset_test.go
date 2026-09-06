package app

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResetRemovesGeneratedStateAndPreservesDotenv(t *testing.T) {
	root := t.TempDir()
	providersPath := filepath.Join(root, "routing", "providers.yaml")
	statsPath := isolateResetStatsPath(t, root)
	envPath := filepath.Join(root, ".env")
	dotenv := []byte("OPENROUTER_API_KEY=keep-this-secret\n")
	t.Setenv("ORR_PROVIDERS_FILE", providersPath)

	for path, contents := range map[string][]byte{
		providersPath: []byte("version: 1\nmodels: {}\n"),
		statsPath:     []byte(`{"version":1}`),
		envPath:       dotenv,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runReset(envPath, &output); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{providersPath, statsPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("generated state still exists at %s: %v", path, err)
		}
	}
	if got, err := os.ReadFile(envPath); err != nil || !bytes.Equal(got, dotenv) {
		t.Fatalf("dotenv changed: contents=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(unrelated); err != nil || string(got) != "keep" {
		t.Fatalf("unrelated file changed: contents=%q err=%v", got, err)
	}
	if strings.Contains(output.String(), "keep-this-secret") {
		t.Fatal("reset output exposed dotenv contents")
	}

	// Reset is idempotent, which makes it safe to repeat between test runs.
	output.Reset()
	if err := runReset(envPath, &output); err != nil {
		t.Fatalf("second reset: %v", err)
	}
}

func TestResetRefusesToDeleteDotenvOrDirectories(t *testing.T) {
	root := t.TempDir()
	isolateResetStatsPath(t, root)
	envPath := filepath.Join(root, ".env")
	if err := os.WriteFile(envPath, []byte("OPENROUTER_API_KEY=preserved\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ORR_PROVIDERS_FILE", envPath)
	if err := runReset(envPath, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "refusing to delete it") {
		t.Fatalf("dotenv target error = %v", err)
	}
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("dotenv was removed: %v", err)
	}

	providersDirectory := filepath.Join(root, "providers-dir")
	if err := os.Mkdir(providersDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORR_PROVIDERS_FILE", providersDirectory)
	if err := runReset(envPath, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("directory target error = %v", err)
	}
}

func TestResetRejectsPositionalArguments(t *testing.T) {
	err := executeCommand(t, "reset", "unexpected")
	if err == nil || !strings.Contains(err.Error(), "accepts no positional arguments") {
		t.Fatalf("argument error = %v", err)
	}
}

func isolateResetStatsPath(t *testing.T, root string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", root)
	} else {
		t.Setenv("XDG_DATA_HOME", root)
	}
	return filepath.Join(root, "orr", "stats.json")
}
