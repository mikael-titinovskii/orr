//go:build windows

package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Only the errors that clear on their own may be retried. Retrying a permanent
// failure would just delay reporting it.
func TestTransientReplaceErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"access denied", windows.ERROR_ACCESS_DENIED, true},
		{"sharing violation", windows.ERROR_SHARING_VIOLATION, true},
		{"lock violation", windows.ERROR_LOCK_VIOLATION, true},
		{"missing file", windows.ERROR_FILE_NOT_FOUND, false},
		{"missing path", windows.ERROR_PATH_NOT_FOUND, false},
		{"nil", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := transientReplaceError(test.err); got != test.want {
				t.Fatalf("transientReplaceError(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestReplaceFileOverwritesDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	destination := filepath.Join(dir, "destination")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("destination = %q, want new", data)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists after replace: %v", err)
	}
}

// The case the retry exists for: something else holds the destination open, so
// the first attempt fails, and it lets go a moment later. On Windows an open
// handle really does block the replace — which is why a single attempt loses
// the write.
func TestReplaceFileRetriesWhileDestinationIsHeld(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	destination := filepath.Join(dir, "destination")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	held, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	// A single attempt fails outright while the handle is open.
	if err := singleReplaceAttempt(source, destination); err == nil {
		held.Close()
		t.Skip("this platform allows replacing a file that is open for reading")
	} else if !transientReplaceError(err) {
		held.Close()
		t.Fatalf("holding the destination open gave %v, want a retryable error", err)
	}

	// Release it within the retry budget; the replace should then land.
	go func() {
		time.Sleep(10 * time.Millisecond)
		held.Close()
	}()
	if err := replaceFile(source, destination); err != nil {
		t.Fatalf("replace did not retry past a briefly held destination: %v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("destination = %q, want new", data)
	}
}
