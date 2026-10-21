package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResetRemovesGeneratedStateAndPreservesDotenv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	t.Setenv("LOCALAPPDATA", root)
	// The database lives where a default install puts it, which is the only place
	// reset may treat the legacy stats file as generated state.
	dbPath := filepath.Join(root, "orr", DBFileName)
	envPath := filepath.Join(root, ".env")
	dotenv := []byte("OPENROUTER_API_KEY=keep-this-secret\n")
	t.Setenv("ORR_DB_FILE", dbPath)

	legacyPath := filepath.Join(filepath.Dir(dbPath), legacyStatsFileName)
	importedPath := legacyPath + ".imported"
	for path, contents := range map[string][]byte{
		dbPath:          []byte("database"),
		dbPath + "-wal": []byte("write-ahead log"),
		dbPath + "-shm": []byte("shared memory"),
		legacyPath:      []byte(`{"version":1}`),
		importedPath:    []byte(`{"version":1}`),
		envPath:         dotenv,
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
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", legacyPath, importedPath} {
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
	envPath := filepath.Join(root, ".env")
	if err := os.WriteFile(envPath, []byte("OPENROUTER_API_KEY=preserved\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ORR_DB_FILE", envPath)
	if err := runReset(envPath, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "refusing to delete it") {
		t.Fatalf("dotenv target error = %v", err)
	}
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("dotenv was removed: %v", err)
	}

	databaseDirectory := filepath.Join(root, "database-dir")
	if err := os.Mkdir(databaseDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORR_DB_FILE", databaseDirectory)
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

// A custom ORR_DB_FILE owns neither a stats.json beside it nor the platform
// data directory's legacy files: the pre-database version wrote those for the
// installation whose database is in that directory, and a relocated database
// never imports them. Reset must leave both alone.
func TestResetLeavesLegacyFilesOwnedByAnotherInstallation(t *testing.T) {
	root := t.TempDir()
	platform := filepath.Join(root, "platform")
	t.Setenv("XDG_DATA_HOME", platform)
	t.Setenv("LOCALAPPDATA", platform)

	dbPath := filepath.Join(root, "project", DBFileName)
	t.Setenv("ORR_DB_FILE", dbPath)
	foreign := filepath.Join(filepath.Dir(dbPath), legacyStatsFileName)
	platformLegacy := filepath.Join(platform, "orr", legacyStatsFileName)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(platformLegacy), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		dbPath:                       "database",
		foreign:                      `{"version":1}`,
		platformLegacy:               `{"version":1}`,
		platformLegacy + ".imported": `{"version":1}`,
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	envPath := filepath.Join(root, ".env")
	if err := os.WriteFile(envPath, []byte("OPENROUTER_API_KEY=keep-this\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runReset(envPath, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("reset removed a stats.json it does not own: %v", err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("reset left the database at %s: %v", dbPath, err)
	}
	for _, path := range []string{platformLegacy, platformLegacy + ".imported"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("reset removed another installation's %s: %v", path, err)
		}
	}
}

func TestLegacyStatsPathOnlyClaimsThePlatformDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	t.Setenv("LOCALAPPDATA", root)

	platformDB := filepath.Join(root, "orr", DBFileName)
	if path, ok := legacyStatsPath(platformDB); !ok || path != filepath.Join(root, "orr", legacyStatsFileName) {
		t.Fatalf("platform database legacy path = %q, %v", path, ok)
	}

	customDB := filepath.Join(root, "elsewhere", DBFileName)
	if path, ok := legacyStatsPath(customDB); ok {
		t.Fatalf("custom database claimed %q", path)
	}
}
