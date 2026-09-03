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

// cwdFieldAliases lists alternate key names Copilot CLI may use for the
// session's working directory in workspace.yaml. Newer CLI versions have
// been observed renaming or nesting this field, so we probe common
// alternatives when the canonical "cwd" key isn't present.
var cwdFieldAliases = []string{
	"cwd", "workingDirectory", "working_directory", "directory",
	"workspaceFolder", "workspace_folder", "path", "root",
}

// findStringField looks up any of the given keys in raw, checking the top
// level first and then one level of nesting (e.g. a "workspace" sub-object).
func findStringField(raw map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := raw[k].(string); ok && v != "" {
			return v
		}
	}
	for _, v := range raw {
		nested, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		for _, k := range keys {
			if s, ok := nested[k].(string); ok && s != "" {
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

	// Copilot CLI has changed workspace.yaml's field names across versions.
	// If the canonical "cwd" field is missing, probe common alternate names
	// (including inside a nested sub-object) before giving up on it.
	if meta.CWD == "" {
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err == nil {
			meta.CWD = findStringField(raw, cwdFieldAliases)
		}
	}

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
