package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const releaseUpdateCheckInterval = time.Hour

// checkForReleaseUpdate fetches release tags without blocking the TUI. When
// orr is running from its checkout, it updates that checkout's tags. Installed
// binaries use a short-lived bare repository so the check works from any
// working directory without leaving source files behind.
func checkForReleaseUpdate() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if repository := localOrrRepository(ctx); repository != "" {
		if _, err := gitOutput(ctx, repository, "fetch", "--quiet", "--tags", "--no-recurse-submodules"); err != nil {
			return "", err
		}
		return repositoryHasNewerRelease(ctx, repository)
	}

	temporary, err := os.MkdirTemp("", "orr-release-check-*")
	if err != nil {
		return "", fmt.Errorf("create release check directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	if _, err := gitOutput(ctx, "", "init", "--bare", "--quiet", temporary); err != nil {
		return "", err
	}
	if _, err := gitOutput(ctx, temporary, "fetch", "--quiet", "--depth=1", "--tags", "--no-recurse-submodules", orrRepositoryURL()); err != nil {
		return "", err
	}
	return repositoryHasNewerRelease(ctx, temporary)
}

func repositoryHasNewerRelease(ctx context.Context, repository string) (string, error) {
	tags, err := gitOutput(ctx, repository, "tag", "--list")
	if err != nil {
		return "", err
	}
	return newerReleaseTag(tags, Version), nil
}

func localOrrRepository(ctx context.Context) string {
	candidates := []string{"."}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(executable))
	}
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		root, err := gitOutput(ctx, "", "-C", candidate, "rev-parse", "--show-toplevel")
		if err != nil {
			continue
		}
		root = strings.TrimSpace(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		if isOrrRepository(root) {
			return root
		}
	}
	return ""
}

func gitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return "", fmt.Errorf("git: %s: %w", message, err)
		}
		return "", err
	}
	return string(output), nil
}

func newerReleaseTag(tags, current string) string {
	currentVersion, ok := parseReleaseVersion(current)
	if !ok {
		return ""
	}
	newest := currentVersion
	newestTag := ""
	for _, tag := range strings.Fields(tags) {
		candidate, ok := parseReleaseVersion(tag)
		if ok && candidate.compare(newest) > 0 {
			newest = candidate
			newestTag = tag
		}
	}
	return newestTag
}

type releaseVersion struct {
	core       [3]int
	prerelease []string
}

func parseReleaseVersion(value string) (releaseVersion, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	value, _, _ = strings.Cut(value, "+")
	core, prerelease, _ := strings.Cut(value, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return releaseVersion{}, false
	}
	var parsed releaseVersion
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return releaseVersion{}, false
		}
		parsed.core[i] = number
	}
	if prerelease != "" {
		parsed.prerelease = strings.Split(prerelease, ".")
	}
	return parsed, true
}

func (v releaseVersion) compare(other releaseVersion) int {
	for i := range v.core {
		if v.core[i] < other.core[i] {
			return -1
		}
		if v.core[i] > other.core[i] {
			return 1
		}
	}
	if len(v.prerelease) == 0 && len(other.prerelease) > 0 {
		return 1
	}
	if len(v.prerelease) > 0 && len(other.prerelease) == 0 {
		return -1
	}
	for i := 0; i < min(len(v.prerelease), len(other.prerelease)); i++ {
		if comparison := compareReleaseIdentifier(v.prerelease[i], other.prerelease[i]); comparison != 0 {
			return comparison
		}
	}
	if len(v.prerelease) < len(other.prerelease) {
		return -1
	}
	if len(v.prerelease) > len(other.prerelease) {
		return 1
	}
	return 0
}

func compareReleaseIdentifier(left, right string) int {
	leftNumber, leftNumeric := releaseIdentifierNumber(left)
	rightNumber, rightNumeric := releaseIdentifierNumber(right)
	switch {
	case leftNumeric && rightNumeric:
		if leftNumber < rightNumber {
			return -1
		}
		if leftNumber > rightNumber {
			return 1
		}
		return 0
	case leftNumeric:
		return -1
	case rightNumeric:
		return 1
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func releaseIdentifierNumber(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	number, err := strconv.Atoi(value)
	return number, err == nil
}
