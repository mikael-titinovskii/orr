package app

import (
	"os"
	"path/filepath"
)

const Version = "0.2.0"

// defaultEnvPath keeps repository-local development unchanged while allowing a
// system-installed binary to find the configuration created by the installer.
func defaultEnvPath() string {
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ".env"
	}
	return filepath.Join(configDir, "orr", ".env")
}
