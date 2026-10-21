package app

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// runReset removes all persisted routing and measurement state. The dotenv
// file is deliberately only read: credentials and configuration are not part
// of the generated state and must survive a reset.
func runReset(envPath string, output io.Writer) error {
	return runResetWithOptions(envPath, false, output)
}

func runResetWithOptions(envPath string, preferEnvFile bool, output io.Writer) error {
	fileEnv, err := readDotEnv(envPath)
	if err != nil {
		return err
	}
	dbPath, err := configuredDBPathWithOptions(envPath, fileEnv, preferEnvFile)
	if err != nil {
		return fmt.Errorf("locate database: %w", err)
	}

	// SQLite keeps these beside the database while it is open; removing the
	// database alone would leave a WAL that a later start could replay. The
	// legacy stats files go too, but only when this installation owns them: they
	// live in the platform data directory, and a relocated database never
	// imports them, so they belong to the installation whose database is there.
	targets := []resetTarget{
		{label: "database", path: dbPath},
		{label: "database write-ahead log", path: dbPath + "-wal"},
		{label: "database shared-memory file", path: dbPath + "-shm"},
	}
	if legacy, ok := legacyStatsPath(dbPath); ok {
		targets = append(targets,
			resetTarget{label: "legacy statistics file", path: legacy},
			resetTarget{label: "imported legacy statistics file", path: legacy + ".imported"},
		)
	} else if platform, ok := platformLegacyStats(); ok {
		if _, err := os.Stat(platform); err == nil {
			log.Printf("left %s in place: it belongs to the installation whose database is in %s",
				platform, filepath.Dir(platform))
		}
	}
	// ORR_DB_FILE is user-configurable. Refuse any path collision that would
	// violate reset's promise to preserve the dotenv file.
	for _, target := range targets {
		same, err := sameFilePath(target.path, envPath)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("%s path points to the dotenv file; refusing to delete it", target.label)
		}
	}
	// Validate every target before deleting any of them so an invalid target
	// cannot leave a half-reset installation.
	for _, target := range targets {
		if err := validateResetTarget(target); err != nil {
			return err
		}
	}
	for _, target := range targets {
		if err := os.Remove(target.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s %s: %w", target.label, target.path, err)
		}
	}

	fmt.Fprintln(output, "Reset complete. If 'orr serve' was running, stop it and run reset again.")
	fmt.Fprintf(output, "  removed database: %s\n", dbPath)
	fmt.Fprintf(output, "  preserved dotenv: %s\n", filepath.Clean(envPath))
	return nil
}

type resetTarget struct {
	label string
	path  string
}

func validateResetTarget(target resetTarget) error {
	info, err := os.Lstat(target.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s %s: %w", target.label, target.path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s path is a directory, refusing to remove it: %s", target.label, target.path)
	}
	return nil
}

func sameFilePath(left, right string) (bool, error) {
	leftAbs, err := filepath.Abs(left)
	if err != nil {
		return false, fmt.Errorf("resolve path %s: %w", left, err)
	}
	rightAbs, err := filepath.Abs(right)
	if err != nil {
		return false, fmt.Errorf("resolve path %s: %w", right, err)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(leftAbs), filepath.Clean(rightAbs)), nil
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs), nil
}
