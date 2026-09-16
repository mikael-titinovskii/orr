package app

import (
	"context"
	"os"
	"testing"
)

func TestNewerReleaseForRevision(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	ctx := context.Background()
	repository := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		output, err := gitOutput(ctx, repository, args...)
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	git("init", "--quiet")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "--allow-empty", "-m", "release")
	git("tag", "1.0.0")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "--allow-empty", "-m", "later release")
	git("tag", "v1.1.0")
	tags := git("tag", "--list")
	for _, tt := range []struct{ name, revision, want string }{
		{"current checkout", "HEAD", ""},
		{"older binary in newer checkout", "HEAD~1", "v1.1.0"},
		{"unknown revision uses version", "", "v1.1.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newerReleaseForRevision(ctx, repository, tags, "0.2.0", tt.revision)
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
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
