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

// sessionIDKeys and sessionCWDKeys list the field names known to have been used
// across Copilot CLI releases for a session's ID and working directory. Newer
// CLI versions have renamed or nested these fields, so several candidates are
// probed instead of relying on a single fixed key.
var (
	sessionIDKeys   = []string{"id", "session_id", "sessionId", "sessionID"}
	sessionCWDKeys  = []string{"cwd", "cwd_path", "cwdPath", "working_directory", "workingDirectory", "directory", "workspace_dir", "workspaceDir", "path", "workspace"}
	sessionNestKeys = []string{"workspace", "session", "meta", "metadata"}
)

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

// lookupYAMLString returns the first non-empty string value found in m under any of keys.
func lookupYAMLString(m map[string]interface{}, keys []string) string {
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
// It tolerates schema drift across Copilot CLI versions by probing several
// known field names/locations for the session ID and working directory
// instead of requiring an exact key match.
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
		ID:  lookupYAMLString(raw, sessionIDKeys),
		CWD: lookupYAMLString(raw, sessionCWDKeys),
	}
	if gr, ok := raw["git_root"].(string); ok {
		meta.GitRoot = gr
	}

	// Some Copilot CLI versions nest workspace metadata under a sub-key
	// instead of storing it at the top level.
	for _, nestKey := range sessionNestKeys {
		nested, ok := raw[nestKey].(map[string]interface{})
		if !ok {
			continue
		}
		if meta.ID == "" {
			meta.ID = lookupYAMLString(nested, sessionIDKeys)
		}
		if meta.CWD == "" {
			meta.CWD = lookupYAMLString(nested, sessionCWDKeys)
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
