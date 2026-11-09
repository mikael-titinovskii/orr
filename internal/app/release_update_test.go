package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// isolateGitConfig pins the git configuration the release-check environment is
// derived from, so a core.sshCommand in the operator's own config cannot leak
// into these results.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
}

func TestGitEnvKeepsPassphrasePromptsOffTheTerminal(t *testing.T) {
	isolateGitConfig(t)
	env := gitEnv(t.TempDir())
	if !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("release check may prompt git for credentials: %v", env)
	}
	if !slices.Contains(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		t.Fatalf("release check may prompt ssh for a key passphrase: %v", env)
	}
}

func TestGitEnvLeavesAnExplicitSSHCommandAlone(t *testing.T) {
	isolateGitConfig(t)
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /tmp/deploy_key")
	env := gitEnv(t.TempDir())
	if !slices.Contains(env, "GIT_SSH_COMMAND=ssh -i /tmp/deploy_key") {
		t.Fatalf("configured ssh command missing from the release check: %v", env)
	}
	if hasBatchModeOverride(env) {
		t.Fatalf("release check overrode the configured ssh command: %v", env)
	}
}

func TestGitEnvLeavesGitSSHAndCoreSSHCommandAlone(t *testing.T) {
	isolateGitConfig(t)
	t.Setenv("GIT_SSH", "/usr/local/bin/plink")
	if env := gitEnv(t.TempDir()); hasBatchModeOverride(env) {
		t.Fatalf("release check overrode GIT_SSH: %v", env)
	}

	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config, []byte("[core]\n\tsshCommand = /usr/local/bin/plink\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	if env := gitEnv(t.TempDir()); hasBatchModeOverride(env) {
		t.Fatalf("release check overrode core.sshCommand: %v", env)
	}
}

func hasBatchModeOverride(env []string) bool {
	return slices.ContainsFunc(env, func(value string) bool {
		return strings.HasPrefix(value, "GIT_SSH_COMMAND=") && strings.Contains(value, "BatchMode")
	})
}

func TestNewerReleaseTag(t *testing.T) {
	tests := []struct {
		name    string
		tags    string
		current string
		want    string
	}{
		{name: "new patch", tags: "v0.1.0\nv0.2.1\n", current: "0.2.0", want: "v0.2.1"},
		{name: "same version", tags: "v0.2.0\nv0.1.0\n", current: "0.2.0"},
		{name: "only older", tags: "v0.1.9\n", current: "0.2.0"},
		{name: "new prerelease", tags: "v0.3.0-rc.1\n", current: "0.2.0", want: "v0.3.0-rc.1"},
		{name: "stable newer than prerelease", tags: "v0.3.0\n", current: "0.3.0-rc.2", want: "v0.3.0"},
		{name: "highest newer tag", tags: "v0.4.0\nv1.0.0\nv0.9.0\n", current: "0.2.0", want: "v1.0.0"},
		{name: "metadata ignored", tags: "v0.2.0+build.4\n", current: "0.2.0"},
		{name: "unrelated tag ignored", tags: "latest\nrelease-2026\n", current: "0.2.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newerReleaseTag(tt.tags, tt.current); got != tt.want {
				t.Fatalf("newerReleaseTag(%q, %q) = %q, want %q", tt.tags, tt.current, got, tt.want)
			}
		})
	}
}
