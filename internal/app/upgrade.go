package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runUpgrade(output io.Writer) error {

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}

	repository, cleanup, err := prepareUpgradeSource(output)
	if err != nil {
		return err
	}
	defer cleanup()

	temporary, err := os.CreateTemp(filepath.Dir(executable), ".orr-upgrade-*"+filepath.Ext(executable))
	if err != nil {
		return fmt.Errorf("create upgrade file next to %s: %w", executable, err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close upgrade file: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("prepare upgrade file: %w", err)
	}
	defer os.Remove(temporaryPath)

	fmt.Fprintln(output, "Building the new executable...")
	if err := runUpgradeCommand(output, repository, "go", "build", "-trimpath", "-o", temporaryPath, "./cmd/orr"); err != nil {
		return fmt.Errorf("build upgrade: %w", err)
	}
	if err := replaceExecutable(temporaryPath, executable); err != nil {
		return fmt.Errorf("install upgrade: %w", err)
	}

	fmt.Fprintf(output, "Upgrade complete: %s\n", executable)
	return nil
}

func prepareUpgradeSource(output io.Writer) (string, func(), error) {
	if repository, err := commandOutput("git", "rev-parse", "--show-toplevel"); err == nil {
		repository = strings.TrimSpace(repository)
		if isOrrRepository(repository) {
			fmt.Fprintln(output, "Pulling the latest changes...")
			if err := runUpgradeCommand(output, repository, "git", "pull", "--ff-only"); err != nil {
				return "", nil, fmt.Errorf("pull changes: %w", err)
			}
			return repository, func() {}, nil
		}
	}

	temporaryRoot, err := os.MkdirTemp("", "orr-upgrade-source-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary source directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(temporaryRoot) }
	repository := filepath.Join(temporaryRoot, "source")
	fmt.Fprintln(output, "Downloading the latest changes...")
	if err := runUpgradeCommand(output, "", "git", "clone", "--depth", "1", orrRepositoryURL(), repository); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("download changes: %w", err)
	}
	if !isOrrRepository(repository) {
		cleanup()
		return "", nil, errors.New("download changes: downloaded source is not an orr repository")
	}
	return repository, cleanup, nil
}

func orrRepositoryURL() string {
	if repositoryURL := os.Getenv("ORR_REPOSITORY_URL"); repositoryURL != "" {
		return repositoryURL
	}
	return "https://github.com/mikael-titinovskii/orr.git"
}

func isOrrRepository(directory string) bool {
	if directory == "" {
		return false
	}
	module, err := os.ReadFile(filepath.Join(directory, "go.mod"))
	if err != nil || !bytes.Contains(module, []byte("module github.com/mikael-titinovskii/orr")) {
		return false
	}
	info, err := os.Stat(filepath.Join(directory, "cmd", "orr"))
	return err == nil && info.IsDir()
}

func commandOutput(name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	result, err := command.Output()
	if err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return "", fmt.Errorf("%s: %w", message, err)
		}
		return "", err
	}
	return string(result), nil
}

func runUpgradeCommand(output io.Writer, directory, name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Dir = directory
	command.Stdout = output
	command.Stderr = output
	return command.Run()
}
