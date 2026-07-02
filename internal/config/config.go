package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/re-cinq/shift-log/internal/util"
)

const configFile = "config"
const shiftlogDir = ".shiftlog"

// Config represents the shiftlog configuration stored in .shiftlog/config
type Config struct {
	NotesRef string `json:"notes_ref"`
	Debug    bool   `json:"debug"`
	Agent    string `json:"agent,omitempty"` // coding agent name (empty = "claude" for backward compat)
}

// Read reads the config from .shiftlog/config in the project root.
// Returns a default config if the file doesn't exist.
func Read() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Write writes the config to .shiftlog/config in the project root.
func Write(cfg *Config) error {
	path, err := Path()
	if err != nil {
		return err
	}

	if err := util.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return os.WriteFile(path, data, 0644)
}

// Path returns the absolute path to the .shiftlog/config file.
func Path() (string, error) {
	root, err := util.GetProjectRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, shiftlogDir, configFile), nil
}

// DirExists returns true if the .shiftlog directory exists in the project root.
func DirExists() (bool, error) {
	root, err := util.GetProjectRoot()
	if err != nil {
		return false, err
	}
	info, err := os.Stat(filepath.Join(root, shiftlogDir))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}
