package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func writeProvidersFileAtomic(path string, models map[string]providerConfig) error {
	data, err := yaml.Marshal(providersFile{Version: providersFileVersion, Models: models})
	if err != nil {
		return fmt.Errorf("encode providers file: %w", err)
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create providers directory: %w", err)
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat providers file: %w", statErr)
	}
	temp, err := os.CreateTemp(directory, ".orr-providers-*")
	if err != nil {
		return fmt.Errorf("create temporary providers file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary providers file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tempPath, path); err != nil {
		return fmt.Errorf("replace providers file: %w", err)
	}
	return nil
}
