package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
type sessionMeta struct {
	ID      string `yaml:"id"`
	CWD     string `yaml:"cwd"`
	GitRoot string `yaml:"git_root,omitempty"`
}

// cwdFieldCandidates are known field names Copilot CLI has used (across
// versions) to record the session's working directory in workspace.yaml.
var cwdFieldCandidates = []string{
	"cwd", "workingDirectory", "working_directory", "directory",
	"workspaceFolder", "workspace_folder", "workspace_dir", "dir", "root",
}

// nestedCwdFieldCandidates are keys under which a nested "workspace" object
// may hold the working directory.
var nestedCwdFieldCandidates = []string{"cwd", "directory", "folder", "root"}

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

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var meta sessionMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	if meta.CWD == "" {
		// Newer Copilot CLI releases have shuffled the workspace.yaml schema
		// (renamed/nested keys). Fall back to scanning raw keys so session
		// discovery keeps working even if the exact field name changes.
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err == nil {
			for _, key := range cwdFieldCandidates {
				if v, ok := raw[key].(string); ok && v != "" {
					meta.CWD = v
					break
				}
			}
			if meta.CWD == "" {
				if ws, ok := raw["workspace"].(map[string]interface{}); ok {
					for _, key := range nestedCwdFieldCandidates {
						if v, ok := ws[key].(string); ok && v != "" {
							meta.CWD = v
							break
						}
					}
				}
			}
		}
	}

	if meta.ID == "" {
		// Some formats omit the session id from workspace.yaml since it's
		// already encoded in the containing directory name.
		meta.ID = filepath.Base(sessionDir)
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
