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

// idKeyCandidates are possible YAML keys Copilot CLI may use to record a
// session's identifier, in preference order. Newer CLI releases have been
// observed to rename or nest these fields relative to older versions.
var idKeyCandidates = []string{"id", "sessionId", "session_id", "sessionID"}

// cwdKeyCandidates are possible YAML keys Copilot CLI may use to record a
// session's working directory, in preference order.
var cwdKeyCandidates = []string{"cwd", "directory", "workingDirectory", "working_dir", "project_dir", "projectPath", "root", "path"}

// lookupStringKeys returns the first non-empty string value found in m for
// any of the given candidate keys.
func lookupStringKeys(m map[string]interface{}, keys []string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// extractFromRaw searches a generically-parsed YAML document for a value
// matching one of the given candidate keys, checking the top level and one
// level of nesting under a "workspace" or "session" key.
func extractFromRaw(raw map[string]interface{}, keys []string) string {
	if v := lookupStringKeys(raw, keys); v != "" {
		return v
	}
	for _, nestKey := range []string{"workspace", "session"} {
		if nested, ok := raw[nestKey].(map[string]interface{}); ok {
			if v := lookupStringKeys(nested, keys); v != "" {
				return v
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
// Copilot CLI's workspace.yaml schema has drifted across releases (field
// renames and added nesting), so beyond the known "id"/"cwd" fields, this
// also falls back to a set of known alternate keys.
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

	if meta.ID == "" || meta.CWD == "" {
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err == nil {
			if meta.ID == "" {
				meta.ID = extractFromRaw(raw, idKeyCandidates)
			}
			if meta.CWD == "" {
				meta.CWD = extractFromRaw(raw, cwdKeyCandidates)
			}
		}
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
