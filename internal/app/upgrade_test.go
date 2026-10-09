package app

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpgradeRejectsArgumentsBeforeRunningCommands(t *testing.T) {
	err := executeCommand(t, "upgrade", "unexpected")
	if err == nil || !strings.Contains(err.Error(), "accepts no positional arguments") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReplaceExecutable(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "new")
	destination := filepath.Join(directory, "orr")
	if err := os.WriteFile(source, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceExecutable(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Fatalf("destination contains %q", got)
	}
}

func TestBuildExecutableUsesDockerWithoutGo(t *testing.T) {
	commands := fakeCommandDirectory(t)
	writeFakeCommand(t, commands, "docker", `pwd -P > "$ORR_TEST_LOG"
printf '%s\n' "$@" >> "$ORR_TEST_LOG"
for argument do
  case "$argument" in
    type=local,dest=*) printf 'docker binary' > "${argument#type=local,dest=}/orr" ;;
  esac
done
`)
	repository := t.TempDir()
	binDirectory := t.TempDir()
	destination := filepath.Join(binDirectory, ".orr-upgrade-test")

	if err := buildExecutable(io.Discard, repository, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "docker binary" {
		t.Fatalf("destination contains %q", got)
	}

	log, err := os.ReadFile(os.Getenv("ORR_TEST_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	resolvedRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if lines[0] != resolvedRepository {
		t.Fatalf("docker ran in %q, want %q", lines[0], resolvedRepository)
	}
	arguments := lines[1:]
	want := []string{"build", "--platform", runtime.GOOS + "/" + runtime.GOARCH, "--output"}
	if len(arguments) != 6 || strings.Join(arguments[:4], " ") != strings.Join(want, " ") ||
		!strings.HasPrefix(arguments[4], "type=local,dest="+binDirectory+string(filepath.Separator)) || arguments[5] != "." {
		t.Fatalf("unexpected docker arguments: %q", arguments)
	}

	entries, err := os.ReadDir(binDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("build output directory was not removed: %v", entries)
	}
}

func TestBuildExecutablePrefersGo(t *testing.T) {
	testBuildExecutablePrefersGo(t, "go1.27.1")
}

func TestBuildExecutablePrefersGoWithToolchainSwitching(t *testing.T) {
	testBuildExecutablePrefersGo(t, "go1.22.4")
}

func TestBuildExecutablePrefersGoWithPrereleaseVersion(t *testing.T) {
	testBuildExecutablePrefersGo(t, "go1.24rc1")
}

// testBuildExecutablePrefersGo checks that buildExecutable builds with the
// host Go toolchain when the fake go script reports goversion. The docker
// script exits nonzero, so a stray docker call fails the test.
func testBuildExecutablePrefersGo(t *testing.T, goversion string) {
	t.Helper()
	commands := fakeCommandDirectory(t)
	writeFakeCommand(t, commands, "go", `if [ "$1" = env ]; then
  printf '%s\n' '`+goversion+`'
  exit 0
fi
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    printf 'go binary' > "$2"
  fi
  shift
done
`)
	writeFakeCommand(t, commands, "docker", "exit 1\n")
	destination := filepath.Join(t.TempDir(), ".orr-upgrade-test")

	if err := buildExecutable(io.Discard, t.TempDir(), destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "go binary" {
		t.Fatalf("destination contains %q", got)
	}
}

func TestBuildExecutableUsesDockerWithOldGo(t *testing.T) {
	testBuildExecutableUsesDocker(t, `if [ "$1" = env ]; then
  printf 'go1.20.5\n'
  exit 0
fi
exit 1
`)
}

func TestBuildExecutableUsesDockerWhenGoEnvFails(t *testing.T) {
	testBuildExecutableUsesDocker(t, "exit 1\n")
}

func TestBuildExecutableUsesDockerWithUnparseableGoVersion(t *testing.T) {
	testBuildExecutableUsesDocker(t, `if [ "$1" = env ]; then
  printf 'not-a-version\n'
  exit 0
fi
exit 1
`)
}

// testBuildExecutableUsesDocker checks that buildExecutable falls back to the
// Docker build when the fake go script makes the toolchain unusable. The go
// script exits nonzero for anything but its version probe, so a stray
// `go build` call fails the test.
func testBuildExecutableUsesDocker(t *testing.T, goScript string) {
	t.Helper()
	commands := fakeCommandDirectory(t)
	writeFakeCommand(t, commands, "go", goScript)
	writeFakeCommand(t, commands, "docker", `for argument do
  case "$argument" in
    type=local,dest=*) printf 'docker binary' > "${argument#type=local,dest=}/orr" ;;
  esac
done
`)
	destination := filepath.Join(t.TempDir(), ".orr-upgrade-test")

	if err := buildExecutable(io.Discard, t.TempDir(), destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "docker binary" {
		t.Fatalf("destination contains %q", got)
	}
}

func TestBuildExecutableRequiresGoOrDocker(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := buildExecutable(io.Discard, t.TempDir(), filepath.Join(t.TempDir(), "orr"))
	if err == nil || !strings.Contains(err.Error(), "install Go 1.21 or newer, or Docker") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// fakeCommandDirectory makes a directory the only PATH entry for the test.
func fakeCommandDirectory(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake commands are shell scripts")
	}
	directory := t.TempDir()
	t.Setenv("PATH", directory)
	t.Setenv("ORR_TEST_LOG", filepath.Join(t.TempDir(), "log"))
	return directory
}

func writeFakeCommand(t *testing.T, directory, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestIsOrrRepository(t *testing.T) {
	directory := t.TempDir()
	if isOrrRepository(directory) {
		t.Fatal("empty directory identified as orr repository")
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module example.com/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "cmd", "orr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if isOrrRepository(directory) {
		t.Fatal("unrelated Go module identified as orr repository")
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module github.com/mikael-titinovskii/orr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isOrrRepository(directory) {
		t.Fatal("orr repository was not recognized")
	}
}
