//go:build !windows

package app

import (
	"os"
	"path/filepath"
)

func replaceFile(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	// Best-effort durability: fsync the parent directory so the rename itself
	// is persisted across a crash. Directory fsync is unsupported on some
	// platforms and filesystems, so a failure here is not fatal.
	if dir, err := os.Open(filepath.Dir(destination)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
