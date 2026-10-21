package app

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/mikael-titinovskii/orr/internal/store"
)

// DBFileName is the database holding routing configuration and measurement
// state. It replaces the providers file and the stats file.
const DBFileName = "orr.db"

// legacyStatsFileName is the pre-database stats file, imported once on start.
const legacyStatsFileName = "stats.json"

// dataDir is the per-user directory orr keeps its database and log in.
func dataDir() (string, error) {
	if runtime.GOOS != "windows" {
		if base := os.Getenv("XDG_DATA_HOME"); base != "" {
			return filepath.Join(base, "orr"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "orr"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "orr"), nil
}

// configuredDBPathWithOptions resolves the database path without opening it.
// ORR_DB_FILE wins when set; a relative value is resolved against the dotenv
// file's directory, the way the providers file used to be.
func configuredDBPathWithOptions(envPath string, fileEnv map[string]string, preferEnvFile bool) (string, error) {
	if configured := envValue(fileEnv, "ORR_DB_FILE", preferEnvFile); configured != "" {
		if !filepath.IsAbs(configured) {
			configured = filepath.Join(filepath.Dir(envPath), configured)
		}
		return filepath.Clean(configured), nil
	}
	directory, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, DBFileName), nil
}

// platformLegacyStats reports the pre-database stats file this installation
// owns. The older version always wrote it to the platform data directory, so
// that directory — not the database's own — is what defines ownership: a
// stats.json beside a custom ORR_DB_FILE belongs to whoever put it there.
func platformLegacyStats() (string, bool) {
	directory, err := dataDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(directory, legacyStatsFileName), true
}

// legacyStatsPath reports the legacy file to import from: only a database
// living in the platform data directory consults it, so relocating the database
// cannot pull in (and rename) the history of another installation.
func legacyStatsPath(dbPath string) (string, bool) {
	legacy, ok := platformLegacyStats()
	if !ok {
		return "", false
	}
	directory, err := dataDir()
	if err != nil {
		return "", false
	}
	same, err := sameFilePath(filepath.Dir(dbPath), directory)
	if err != nil || !same {
		return "", false
	}
	return legacy, true
}

// importLegacyStats imports the stats file a pre-database install left in the
// platform data directory.
//
// A file the database already supersedes is left in place and reported: the
// database wins, but a silently ignored stats.json would look imported.
func importLegacyStats(db *store.Store, dbPath string) error {
	candidate, ok := legacyStatsPath(dbPath)
	if !ok {
		// The database lives elsewhere. Say so when the platform directory still
		// holds history, so a relocation does not look like a silent import.
		if directory, err := dataDir(); err == nil {
			left := filepath.Join(directory, legacyStatsFileName)
			if _, statErr := os.Stat(left); statErr == nil {
				log.Printf("%s was not imported: %s is not in the platform data directory", left, dbPath)
			}
		}
		return nil
	}
	imported, err := db.ImportLegacyStats(candidate)
	if err != nil {
		return fmt.Errorf("import %s: %w", candidate, err)
	}
	if imported {
		return nil
	}
	if _, statErr := os.Stat(candidate); statErr == nil {
		log.Printf("left %s in place: %s already holds measurement state", candidate, dbPath)
	}
	return nil
}
