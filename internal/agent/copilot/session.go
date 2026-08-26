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

// cwdKeyAliases are workspace.yaml keys that may hold the working directory
// across different Copilot CLI releases (the field has been renamed before).
var cwdKeyAliases = []string{"cwd", "cwdPath", "workingDirectory", "working_directory", "directory", "workdir", "path"}

// idKeyAliases are workspace.yaml keys that may hold the session ID across
// different Copilot CLI releases.
var idKeyAliases = []string{"id", "sessionId", "session_id", "sessionID"}

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
// It tolerates key renames across Copilot CLI releases by falling back to a
// set of known aliases for the id/cwd fields when the canonical names
// (id/cwd) are missing or empty.
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
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err == nil {
			if meta.CWD == "" {
				meta.CWD = firstStringValue(raw, cwdKeyAliases)
			}
			if meta.ID == "" {
				meta.ID = firstStringValue(raw, idKeyAliases)
			}
		}
	}

	return &meta, nil
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// transcriptFileNames are known Copilot CLI transcript filenames, in order
// of preference. Newer CLI releases may rename events.jsonl.
var transcriptFileNames = []string{"events.jsonl", "transcript.jsonl", "session.jsonl"}

// findTranscriptPath returns the path to the transcript file within a
// session directory, tolerating filename changes across Copilot CLI
// releases. Falls back to the canonical events.jsonl name if none exist.
func findTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFileNames {
		p := filepath.Join(sessionDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return GetTranscriptPath(sessionDir)
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
