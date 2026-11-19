//go:build !windows

package app

// requireExecutableIdle is a no-op outside Windows: a running process keeps
// the old image through its own inode, so replacing the path is safe.
func requireExecutableIdle(string) error {
	return nil
}
