//go:build !windows

package app

func replaceExecutable(source, destination string) error {
	return replaceFile(source, destination)
}
