package app

import "testing"

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
