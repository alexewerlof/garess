// Package config loads and validates garess configuration (TOML).
package config

import (
	"os"
	"path/filepath"
)

// DirName is the name of the garess config directory.
const DirName = "garess"

// GlobalDir returns the directory holding global garess state: the config
// file, global memory notes and logs. On Linux this resolves to
// $XDG_CONFIG_HOME/garess or ~/.config/garess.
func GlobalDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, DirName), nil
}

// GlobalConfigPath returns the path of the global config file.
func GlobalConfigPath() (string, error) {
	d, err := GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.toml"), nil
}

// GlobalMemoryDir returns the directory of global memory notes.
func GlobalMemoryDir() (string, error) {
	d, err := GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "memory"), nil
}

// GlobalLogPath returns the path of the log file.
func GlobalLogPath() (string, error) {
	d, err := GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "garess.log"), nil
}

// ProjectDir returns the .garess directory in the current working directory.
func ProjectDir() string {
	return filepath.Join(".", ".garess")
}

// ProjectConfigPath returns the path of the project-local config file.
func ProjectConfigPath() string {
	return filepath.Join(ProjectDir(), "config.toml")
}

// RootConfigPath returns ./config.toml in the working directory. It is used
// as a fallback project config when .garess/config.toml does not exist.
func RootConfigPath() string {
	return filepath.Join(".", "config.toml")
}

// ProjectMemoryDir returns the directory of project-local memory notes.
func ProjectMemoryDir() string {
	return filepath.Join(ProjectDir(), "memory")
}

// ProjectSessionsDir returns the directory of project session transcripts.
func ProjectSessionsDir() string {
	return filepath.Join(ProjectDir(), "sessions")
}
