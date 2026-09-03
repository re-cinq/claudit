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

// cwdKeyAliases and idKeyAliases are alternate metadata key names to try when
// the expected "cwd"/"id" fields come back empty. Newer Copilot CLI releases
// have been observed renaming workspace.yaml fields, which leaves the typed
// struct fields as zero values (YAML unmarshal doesn't error on unknown or
// missing keys), so we fall back to scanning the raw document for common
// alternates before giving up.
var cwdKeyAliases = []string{
	"cwd", "cwdPath", "cwd_path", "workingDirectory", "working_directory",
	"directory", "dir", "workspace", "workspacePath", "workspace_path",
	"projectDir", "project_dir", "path",
}

var idKeyAliases = []string{
	"id", "sessionId", "session_id", "sessionID",
}

// firstStringValue returns the first non-empty string value found in m for
// any of the given keys.
func firstStringValue(m map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
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

	if meta.CWD == "" || meta.ID == "" {
		var generic map[string]interface{}
		if err := yaml.Unmarshal(data, &generic); err == nil {
			if meta.CWD == "" {
				meta.CWD = firstStringValue(generic, cwdKeyAliases)
			}
			if meta.ID == "" {
				meta.ID = firstStringValue(generic, idKeyAliases)
			}
		}
	}

	// Copilot has always named session directories after the session ID;
	// fall back to that if the metadata has no usable id field.
	if meta.ID == "" {
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
