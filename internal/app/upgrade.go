package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func runUpgrade(output io.Writer, envPath string, preferEnvFile bool) error {

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
	// Fail before the pull and build when the binary is in use, so the operator
	// is told to close orr instead of waiting for work that cannot be installed.
	if err := requireExecutableIdle(executable); err != nil {
		return err
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

	if err := buildExecutable(output, repository, temporaryPath); err != nil {
		return fmt.Errorf("build upgrade: %w", err)
	}
	// Re-check right before the replace: a long build gives the operator time
	// to start orr again, and the check is only useful if it reflects the
	// moment of replacement.
	if err := requireExecutableIdle(executable); err != nil {
		return err
	}
	// Prepare the guardrails environment with the new binary before swapping it
	// in. A failed install then leaves the working binary and environment in
	// place instead of replacing orr with one that cannot start.
	fmt.Fprintln(output, "Installing the guardrails sidecar...")
	if err := installGuardrailsForUpgrade(output, temporaryPath, envPath, preferEnvFile); err != nil {
		return err
	}
	if err := replaceExecutable(temporaryPath, executable); err != nil {
		return fmt.Errorf("install upgrade: %w", err)
	}

	fmt.Fprintf(output, "Upgrade complete: %s\n", executable)
	return nil
}

// installGuardrailsForUpgrade runs the new binary's installer before the binary
// replaces the running one. Installations made before guardrails became a
// requirement have no environment, and the upgrade is where they get one.
func installGuardrailsForUpgrade(output io.Writer, executable, envPath string, preferEnvFile bool) error {
	args := []string{"guardrails", "install"}
	if envPath != "" {
		args = append(args, "--env", envPath)
	}
	if preferEnvFile {
		args = append(args, "--prefer-env-file")
	}
	cmd := exec.Command(executable, args...)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install guardrails for the upgraded binary: %w (orr serve will not start without it)", err)
	}
	return nil
}

// buildExecutable builds with the host Go toolchain when it is new enough and
// otherwise cross-compiles for this platform with the repository's Dockerfile.
func buildExecutable(output io.Writer, repository, destination string) error {
	problem := goToolchainProblem()
	if problem == "" {
		fmt.Fprintln(output, "Building the new executable...")
		return runUpgradeCommand(output, repository, "go", "build", "-trimpath", "-o", destination, "./cmd/orr")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("%s install Go 1.21 or newer, or Docker", problem)
	}

	exportDirectory, err := os.MkdirTemp(filepath.Dir(destination), ".orr-upgrade-build-*")
	if err != nil {
		return fmt.Errorf("create build output directory: %w", err)
	}
	defer os.RemoveAll(exportDirectory)

	platform := runtime.GOOS + "/" + runtime.GOARCH
	fmt.Fprintf(output, "Building the new executable for %s with Docker...\n", platform)
	if err := runUpgradeCommand(output, repository, "docker", "build", "--platform", platform, "--output", "type=local,dest="+exportDirectory, "."); err != nil {
		return err
	}
	name := "orr"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return os.Rename(filepath.Join(exportDirectory, name), destination)
}

// goToolchainProblem explains why the host Go toolchain cannot build orr, or
// returns an empty string when `go env GOVERSION` reports Go 1.21 or newer.
// Go 1.21 and newer download the toolchain named in go.mod themselves
// (GOTOOLCHAIN=auto), so any of them can build orr.
func goToolchainProblem() string {
	if _, err := exec.LookPath("go"); err != nil {
		return "Go 1.21 or newer was not found."
	}
	version, err := commandOutput("go", "env", "GOVERSION")
	if err != nil {
		return fmt.Sprintf("Could not determine the installed Go version: %s.", err)
	}
	version = strings.TrimSpace(version)
	major, minor, ok := parseGoVersion(version)
	if !ok {
		return fmt.Sprintf("Could not understand installed Go version: %s.", version)
	}
	if major > 1 || major == 1 && minor >= 21 {
		return ""
	}
	return fmt.Sprintf("Go 1.21 or newer is required; found %s.", version)
}

// parseGoVersion parses a `go<major>.<minor>[.<patch>]` version string,
// ignoring any pre-release suffix on the minor such as in `go1.24rc1`.
func parseGoVersion(version string) (int, int, bool) {
	majorText, rest, found := strings.Cut(strings.TrimPrefix(version, "go"), ".")
	if !found {
		return 0, 0, false
	}
	minorText, _, _ := strings.Cut(rest, ".")
	minorEnd := 0
	for minorEnd < len(minorText) && minorText[minorEnd] >= '0' && minorText[minorEnd] <= '9' {
		minorEnd++
	}
	major, majorErr := strconv.Atoi(majorText)
	minor, minorErr := strconv.Atoi(minorText[:minorEnd])
	if majorErr != nil || minorErr != nil {
		return 0, 0, false
	}
	return major, minor, true
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
