package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
type sessionMeta struct {
	ID  string
	CWD string
}

// cwdKeys are the field names Copilot CLI has used across releases to record
// a session's working directory in workspace.yaml, including one level of
// nesting under a "workspace" object.
var cwdKeys = []string{"cwd", "cwdPath", "cwd_path", "workingDirectory", "working_dir", "directory", "root", "workspaceRoot"}

// idKeys are the field names Copilot CLI has used across releases for a
// session's identifier in workspace.yaml.
var idKeys = []string{"id", "sessionId", "session_id"}

// stringFromKeys returns the first non-empty string value found in m for the
// given candidate keys.
func stringFromKeys(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
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
// It tolerates schema drift across Copilot CLI releases by looking for the
// session ID and working directory under several known field names,
// including one level of nesting (e.g. under a "workspace" key).
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
		ID:  stringFromKeys(raw, idKeys...),
		CWD: stringFromKeys(raw, cwdKeys...),
	}

	// Some releases nest workspace fields under a "workspace" object.
	if meta.CWD == "" {
		if nested, ok := raw["workspace"].(map[string]interface{}); ok {
			meta.CWD = stringFromKeys(nested, cwdKeys...)
			if meta.ID == "" {
				meta.ID = stringFromKeys(nested, idKeys...)
			}
		}
	}

	return meta, nil
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// FindTranscriptFile returns the path to a session's transcript file.
// It prefers events.jsonl but falls back to the most recently modified
// .jsonl file in the session directory, in case a future Copilot CLI
// release renames the transcript file.
func FindTranscriptFile(sessionDir string) string {
	preferred := GetTranscriptPath(sessionDir)
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return preferred
	}

	var bestName string
	var bestModTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if bestName == "" || info.ModTime().After(bestModTime) {
			bestName = entry.Name()
			bestModTime = info.ModTime()
		}
	}

	if bestName == "" {
		return preferred
	}
	return filepath.Join(sessionDir, bestName)
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
	meta := struct {
		ID string `yaml:"id"`
	}{ID: sessionID}
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
