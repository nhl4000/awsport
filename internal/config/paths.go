package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const appName = "awsport"

// Environment overrides for the config and state directories.
const (
	EnvConfigDir = "AWSPORT_CONFIG_DIR"
	EnvStateDir  = "AWSPORT_STATE_DIR"
)

// ConfigDir returns the directory holding config.yaml and team.yaml.
func ConfigDir() (string, error) {
	if d := os.Getenv(EnvConfigDir); d != "" {
		return d, nil
	}
	if runtime.GOOS == "linux" {
		return xdgDir("XDG_CONFIG_HOME", ".config")
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(base, appName), nil
}

// StateDir returns the directory holding machine-written state.
func StateDir() (string, error) {
	if d := os.Getenv(EnvStateDir); d != "" {
		return d, nil
	}
	if runtime.GOOS == "linux" {
		return xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state"))
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(base, appName, "state"), nil
}

// ConfigPath is ConfigDir()/config.yaml.
func ConfigPath() (string, error) {
	return inConfigDir("config.yaml")
}

// TeamConfigPath is ConfigDir()/team.yaml, the imported team config copy.
func TeamConfigPath() (string, error) {
	return inConfigDir("team.yaml")
}

func inConfigDir(name string) (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, name), nil
}

// xdgDir resolves $env/awsport, falling back to ~/<fallback>/awsport when the
// variable is unset or not absolute (per the XDG base directory spec).
func xdgDir(env, fallback string) (string, error) {
	if d := os.Getenv(env); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, appName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	if home == "" {
		return "", errors.New("resolve home dir: empty")
	}
	return filepath.Join(home, fallback, appName), nil
}

// WriteFileAtomic writes data to path via a temp file in the same directory
// followed by rename, with mode 0600, creating parent directories (0700).
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	ok = true
	return nil
}
