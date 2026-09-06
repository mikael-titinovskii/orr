//go:build windows

package app

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// replaceAttempts and replaceRetryDelay bound a short retry of the atomic
// replace. Windows fails MoveFileEx with a sharing or access error when
// anything else momentarily holds the destination — a virus scanner, the
// search indexer, a backup agent, or a reader that opened it without
// FILE_SHARE_DELETE. The failure is transient and clears in milliseconds, but
// without a retry it loses the write, and for the providers file that means
// losing a freshly measured provider order and its pin.
const replaceAttempts = 5
const replaceRetryDelay = 5 * time.Millisecond

func replaceFile(source, destination string) error {
	delay := replaceRetryDelay
	for attempt := 0; ; attempt++ {
		err := singleReplaceAttempt(source, destination)
		if err == nil || attempt == replaceAttempts-1 || !transientReplaceError(err) {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
}

func singleReplaceAttempt(source, destination string) error {
	sourceName, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationName, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourceName, destinationName,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// transientReplaceError reports whether the replace failed for a reason that
// waiting can resolve, rather than one that will fail every time.
func transientReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
