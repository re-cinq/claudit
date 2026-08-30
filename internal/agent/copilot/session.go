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

// cwdKeys are the field names newer/older Copilot CLI releases have used to
// record the session's working directory in workspace.yaml.
var cwdKeys = []string{
	"cwd", "cwd_path", "workingDirectory", "working_directory",
	"directory", "project_dir", "projectDir", "root", "path",
}

// cwdNestedKeys are container keys under which cwdKeys may be nested.
var cwdNestedKeys = []string{"workspace", "project", "meta", "metadata"}

// extractCWD searches a generically-parsed workspace.yaml document for a
// working-directory value, tolerating key renames/nesting across Copilot
// CLI versions.
func extractCWD(raw map[string]interface{}) string {
	if v := lookupStringKey(raw, cwdKeys); v != "" {
		return v
	}
	for _, nk := range cwdNestedKeys {
		if nested, ok := raw[nk].(map[string]interface{}); ok {
			if v := lookupStringKey(nested, cwdKeys); v != "" {
				return v
			}
		}
	}
	return ""
}

// lookupStringKey returns the first non-empty string value found under any
// of the given keys.
func lookupStringKey(m map[string]interface{}, keys []string) string {
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
// It parses the well-known "cwd" field first, then falls back to scanning
// the document generically for renamed/nested working-directory fields to
// stay compatible with newer Copilot CLI releases.
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

	if meta.CWD == "" {
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err == nil {
			if cwd := extractCWD(raw); cwd != "" {
				meta.CWD = cwd
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
