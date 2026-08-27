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

// idKeys, cwdKeys, and gitRootKeys list the field names Copilot CLI has used
// (or may use) for workspace.yaml metadata across versions. Newer releases
// have been observed renaming these fields, so we probe several candidates
// instead of relying on a single fixed key.
var (
	idKeys      = []string{"id", "session_id", "sessionId", "sessionID"}
	cwdKeys     = []string{"cwd", "cwd_path", "cwdPath", "workspace", "workspaceFolder", "working_dir", "workingDirectory", "directory", "path"}
	gitRootKeys = []string{"git_root", "gitRoot", "git_dir", "gitDir"}
)

// firstStringKey returns the first non-empty string value found in m for any of the given keys.
func firstStringKey(m map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
// Metadata is decoded into a generic map first so that field-name changes
// across Copilot CLI versions can be tolerated via fallback key lookups.
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
		ID:      firstStringKey(raw, idKeys),
		CWD:     firstStringKey(raw, cwdKeys),
		GitRoot: firstStringKey(raw, gitRootKeys),
	}

	if meta.ID == "" {
		// Fall back to the containing directory name, which is the session
		// ID in the flat <session-state>/<id>/workspace.yaml layout.
		meta.ID = filepath.Base(sessionDir)
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
