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

// cwdKeys lists the field names Copilot CLI has used across versions to
// record a session's working directory. Newer releases have moved this
// value around (top-level rename, or nested under a "workspace" object), so
// several candidates are checked instead of a single fixed key.
var cwdKeys = []string{
	"cwd", "workspace_cwd", "workspaceCwd", "directory",
	"workingDirectory", "working_dir", "folder", "root", "path",
}

// extractCWD searches a parsed workspace.yaml document for a working
// directory value, checking known top-level keys and a nested "workspace" object.
func extractCWD(raw map[string]interface{}) string {
	for _, key := range cwdKeys {
		if v, ok := raw[key].(string); ok && v != "" {
			return v
		}
	}
	if ws, ok := raw["workspace"].(map[string]interface{}); ok {
		for _, key := range cwdKeys {
			if v, ok := ws[key].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
// It parses into a generic map first so it can tolerate field renames/moves
// (see extractCWD) rather than failing outright when Copilot's exact schema
// shifts between versions.
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

	meta := &sessionMeta{}
	if id, ok := raw["id"].(string); ok {
		meta.ID = id
	}
	meta.CWD = extractCWD(raw)
	if gitRoot, ok := raw["git_root"].(string); ok {
		meta.GitRoot = gitRoot
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
