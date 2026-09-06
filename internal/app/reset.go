package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// runReset removes all persisted routing and measurement state. The dotenv
// file is deliberately only read: credentials and configuration are not part
// of the generated state and must survive a reset.
func runReset(envPath string, output io.Writer) error {

	fileEnv, err := readDotEnv(envPath)
	if err != nil {
		return err
	}
	providersPath := configuredProvidersPath(envPath, fileEnv)
	statsPath, err := statsFilePath()
	if err != nil {
		return fmt.Errorf("locate statistics file: %w", err)
	}

	targets := []resetTarget{
		{label: "providers", path: providersPath},
		{label: "statistics", path: statsPath},
	}
	// ORR_PROVIDERS_FILE is user-configurable. Refuse any path collision that
	// would violate reset's promise to preserve the dotenv file.
	for _, target := range targets {
		same, err := sameFilePath(target.path, envPath)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("%s path points to the dotenv file; refusing to delete it", target.label)
		}
	}
	// Validate every target before deleting either one so an invalid directory
	// target cannot leave a half-reset installation.
	for _, target := range targets {
		if err := validateResetTarget(target); err != nil {
			return err
		}
	}
	for _, target := range targets {
		if err := os.Remove(target.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s file %s: %w", target.label, target.path, err)
		}
	}

	fmt.Fprintln(output, "Reset complete. If 'orr serve' was running, stop it and run reset again.")
	fmt.Fprintf(output, "  removed providers: %s\n", providersPath)
	fmt.Fprintf(output, "  removed statistics: %s\n", statsPath)
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
		return fmt.Errorf("inspect %s file %s: %w", target.label, target.path, err)
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
