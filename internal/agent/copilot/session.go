```go
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

// cwdKeys lists the field names Copilot CLI has used across versions to record
// the working directory a session was started from.
var cwdKeys = []string{
	"cwd", "cwdPath", "workingDirectory", "working_dir",
	"workspacePath", "workspace_path", "directory", "projectDir", "project_dir", "path",
}

// idKeys lists the field names Copilot CLI has used across versions for the session ID.
var idKeys = []string{"id", "sessionId", "session_id", "sessionID"}

// firstStringField extracts the first non-empty string value found under any of the given keys.
func firstStringField(raw map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
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
// Different Copilot CLI versions have used different field names for the
// working directory and session ID, so this parses into a generic map and
// tries a set of known key names (including a possible nested "workspace"
// object) rather than relying on a single fixed schema.
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
		ID:      firstStringField(raw, idKeys),
		CWD:     firstStringField(raw, cwdKeys),
		GitRoot: firstStringField(raw, []string{"git_root", "gitRoot"}),
	}

	if meta.CWD == "" || meta.ID == "" {
		if ws, ok := raw["workspace"].(map[string]interface{}); ok {
			if meta.CWD == "" {
				meta.CWD = firstStringField(ws, cwdKeys)
			}
			if meta.ID == "" {
				meta.ID = firstStringField(ws, idKeys)
			}
		}
	}

	return meta, nil
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
```
