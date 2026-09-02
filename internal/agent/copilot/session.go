package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace file.
// Newer Copilot CLI releases have varied the exact field name used for the
// working directory, so several aliases are accepted.
type sessionMeta struct {
	ID        string `yaml:"id" json:"id"`
	CWD       string `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Directory string `yaml:"directory,omitempty" json:"directory,omitempty"`
	Workspace string `yaml:"workspaceFolder,omitempty" json:"workspaceFolder,omitempty"`
	GitRoot   string `yaml:"git_root,omitempty" json:"git_root,omitempty"`
}

// cwdPath returns the best-known working directory for the session,
// checking known field aliases in order of preference.
func (m *sessionMeta) cwdPath() string {
	if m.CWD != "" {
		return m.CWD
	}
	if m.Directory != "" {
		return m.Directory
	}
	return m.Workspace
}

// GetCopilotDir returns the path to Copilot's config/data directory.
func GetCopilotDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".copilot"), nil
}

// GetSessionStateDir returns the session state directory.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-state"), nil
}

// parseSessionMeta reads a workspace.yaml (or workspace.json, used by some
// Copilot CLI releases) from a Copilot session directory.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		jsonPath := filepath.Join(sessionDir, "workspace.json")
		jsonData, jsonErr := os.ReadFile(jsonPath)
		if jsonErr != nil {
			return nil, err
		}
		var meta sessionMeta
		if err := json.Unmarshal(jsonData, &meta); err != nil {
			return nil, err
		}
		return &meta, nil
	}

	var meta sessionMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// WriteSessionFile writes a session directory structure to Copilot's session state directory.
// Creates <sessionDir>/<sessionID>/ with workspace.yaml and events.jsonl.
func WriteSessionFile(sessionID string, data []byte) (string, error) {
	stateDir, err := GetSessionStateDir()
	if err != nil {
		return "", err
	}

	sessionDir := filepath.Join(stateDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return "", fmt.Errorf("could not create session directory: %w", err)
	}

	// Write workspace.yaml
	meta := sessionMeta{ID: sessionID}
	yamlData, err := yaml.Marshal(&meta)
	if err != nil {
		return "", fmt.Errorf("could not marshal workspace.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "workspace.yaml"), yamlData, 0600); err != nil {
		return "", err
	}

	// Write events.jsonl
	eventsPath := GetTranscriptPath(sessionDir)
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}
