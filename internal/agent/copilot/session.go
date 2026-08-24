package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata extracted from a Copilot
// session's workspace.yaml.
type sessionMeta struct {
	ID  string
	CWD string
}

// cwdKeys lists the field names Copilot CLI has used (across releases) to
// record a session's working directory in workspace.yaml.
var cwdKeys = []string{"cwd", "workingDirectory", "working_dir", "workingDir", "directory", "workspace", "path", "root", "git_root", "gitRoot"}

// idKeys lists the field names Copilot CLI has used to record a session's ID.
var idKeys = []string{"id", "sessionId", "session_id", "sessionID"}

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

// parseSessionMetaFile reads a workspace.yaml file (at the given path) and
// extracts the session ID and working directory. Parsing is tolerant of
// field-name changes across Copilot CLI versions: several candidate keys are
// tried for each field, including one level of nesting.
func parseSessionMetaFile(path string) (*sessionMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	return &sessionMeta{
		ID:  findStringField(raw, idKeys),
		CWD: findStringField(raw, cwdKeys),
	}, nil
}

// findStringField looks up the first matching key (case-insensitive) in a
// generic YAML map, checking one level of nesting if not found at the top.
func findStringField(raw map[string]interface{}, keys []string) string {
	if v := findStringFieldFlat(raw, keys); v != "" {
		return v
	}
	for _, v := range raw {
		if nested, ok := v.(map[string]interface{}); ok {
			if s := findStringFieldFlat(nested, keys); s != "" {
				return s
			}
		}
	}
	return ""
}

// findStringFieldFlat checks only the top-level keys of a map.
func findStringFieldFlat(raw map[string]interface{}, keys []string) string {
	for k, v := range raw {
		for _, want := range keys {
			if strings.EqualFold(k, want) {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
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
