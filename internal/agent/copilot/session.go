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
// Copilot CLI has changed the exact key names/nesting for this metadata
// across versions, so this tolerates several known spellings instead of
// requiring an exact "id"/"cwd"/"git_root" schema.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	meta := &sessionMeta{
		ID:      firstStringField(raw, "id", "sessionId", "session_id"),
		CWD:     firstStringField(raw, "cwd", "workingDirectory", "working_directory", "directory"),
		GitRoot: firstStringField(raw, "git_root", "gitRoot"),
	}

	// Some versions nest these fields under a "workspace" key.
	if workspace, ok := raw["workspace"].(map[string]interface{}); ok {
		if meta.CWD == "" {
			meta.CWD = firstStringField(workspace, "cwd", "directory", "workingDirectory")
		}
		if meta.ID == "" {
			meta.ID = firstStringField(workspace, "id")
		}
	}

	return meta, nil
}

// firstStringField returns the first non-empty string value found in m for
// the given candidate keys, in order.
func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
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
