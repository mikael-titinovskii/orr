//go:build windows

package app

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestRunningHelperProcess is not a test: it is the child instance that the
// running-instance tests start from the same image.
func TestRunningHelperProcess(t *testing.T) {
	if os.Getenv("ORR_TEST_RUNNING_HELPER") != "1" {
		t.Skip("helper for the running-instance tests")
	}
	time.Sleep(time.Minute)
}

// startRunningHelper starts a second process from executable and stops it
// when the test ends.
func startRunningHelper(t *testing.T, executable string) *exec.Cmd {
	t.Helper()
	command := exec.Command(executable, "-test.run=TestRunningHelperProcess")
	command.Env = append(os.Environ(), "ORR_TEST_RUNNING_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	return command
}

func TestRequireExecutableIdleIgnoresCurrentProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := requireExecutableIdle(executable); err != nil {
		t.Fatalf("the current process was reported as another instance: %v", err)
	}
}

func TestSameExecutablePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{"identical", `C:\Tools\orr.exe`, `C:\Tools\orr.exe`, true},
		{"case difference", `C:\Tools\ORR.EXE`, `c:\tools\orr.exe`, true},
		{"redundant separator", `C:\Tools\..\Tools\orr.exe`, `C:\Tools\orr.exe`, true},
		{"long path prefix", `\\?\C:\Tools\orr.exe`, `C:\Tools\orr.exe`, true},
		{"long UNC prefix", `\\?\UNC\server\share\orr.exe`, `\\server\share\orr.exe`, true},
		{"different directory", `C:\Tools\orr.exe`, `C:\Other\orr.exe`, false},
		{"different name", `C:\Tools\orr.exe`, `C:\Tools\other.exe`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sameExecutablePath(test.left, test.right); got != test.want {
				t.Fatalf("sameExecutablePath(%q, %q) = %v, want %v", test.left, test.right, got, test.want)
			}
		})
	}
}

// The backup from an earlier upgrade matters: a process still running it
// blocks removing it and must be reported just like one on the executable.
func TestMatchesInstallImage(t *testing.T) {
	executable := `C:\Tools\orr.exe`
	for _, test := range []struct {
		name string
		path string
		want bool
	}{
		{"executable", `C:\Tools\orr.exe`, true},
		{"backup", `C:\Tools\orr.exe~`, true},
		{"backup with different case", `c:\tools\ORR.EXE~`, true},
		{"another install", `C:\Other\orr.exe`, false},
		{"unrelated image", `C:\Tools\other.exe`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesInstallImage(test.path, executable); got != test.want {
				t.Fatalf("matchesInstallImage(%q, %q) = %v, want %v", test.path, executable, got, test.want)
			}
		})
	}
}

// The runner's temp directory can be reached through an 8.3 short name, and a
// process image keeps whichever spelling it was started with. String equality
// misses that; file identity does not.
func TestFileIdentityIgnoresPathSpelling(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "a-very-long-directory-name-for-short-paths")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(directory, "orr-test.exe")
	if err := os.WriteFile(long, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	short := shortPathName(t, long)
	if sameExecutablePath(short, long) {
		t.Skip("the volume does not expose 8.3 short names")
	}
	left, ok := fileIdentityOf(short)
	if !ok {
		t.Fatalf("identity of %q was not resolved", short)
	}
	right, ok := fileIdentityOf(long)
	if !ok {
		t.Fatalf("identity of %q was not resolved", long)
	}
	if left != right {
		t.Fatalf("identity of %q = %+v, want %+v from %q", short, left, right, long)
	}
}

func shortPathName(t *testing.T, path string) string {
	t.Helper()
	long, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	size, err := windows.GetShortPathName(long, &buffer[0], uint32(len(buffer)))
	if err != nil {
		t.Fatal(err)
	}
	return windows.UTF16ToString(buffer[:size])
}

// The guard's real case: a second orr runs from the same image — typically
// `orr serve` in another terminal — and the upgrade must refuse, naming that
// PID, before replacing the binary underneath it.
func TestRequireExecutableIdleDetectsRunningInstance(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := startRunningHelper(t, executable)
	pid := command.Process.Pid

	waitFor(t, func() bool {
		err := requireExecutableIdle(executable)
		return err != nil && strings.Contains(err.Error(), strconv.Itoa(pid))
	}, 5*time.Second, "running instance was not detected")

	_ = command.Process.Kill()
	_, _ = command.Process.Wait()
	waitFor(t, func() bool {
		return requireExecutableIdle(executable) == nil
	}, 5*time.Second, "closed instance was still reported")
}

// A regression that drops the call from runUpgrade would leave the helper
// functions tested but the command unprotected. PATH is emptied first so a
// missing guard fails here instead of reaching git or a build.
func TestUpgradeRefusesWhileInstanceIsRunning(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	command := startRunningHelper(t, executable)
	pid := command.Process.Pid

	waitFor(t, func() bool {
		return requireExecutableIdle(executable) != nil
	}, 5*time.Second, "running instance was not detected")

	err = runUpgrade(io.Discard, "", false)
	if err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), strconv.Itoa(pid)) {
		t.Fatalf("runUpgrade with a running instance = %v, want an error naming PID %d", err, pid)
	}
}

func waitFor(t *testing.T, condition func() bool, timeout time.Duration, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(message)
}
