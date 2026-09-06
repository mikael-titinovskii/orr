//go:build windows

package app

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func replaceExecutable(source, destination string) error {
	backup := destination + "~"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove previous backup %s: %w", backup, err)
	}
	if err := moveExecutable(destination, backup); err != nil {
		return fmt.Errorf("move running executable to %s: %w", backup, err)
	}
	if err := moveExecutable(source, destination); err != nil {
		_ = moveExecutable(backup, destination)
		return fmt.Errorf("replace executable: %w", err)
	}

	// Windows may keep the renamed executable locked until this process exits.
	// If so, the next upgrade removes it before creating a new backup.
	_ = os.Remove(backup)
	return nil
}

func moveExecutable(source, destination string) error {
	sourceName, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationName, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourceName, destinationName, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
