package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
// Copilot CLI has changed the exact field names it writes across versions, so the
// raw parsed document is also kept around to allow lenient fallback matching.
type sessionMeta struct {
	ID      string `yaml:"id"`
	CWD     string `yaml:"cwd"`
	GitRoot string `yaml:"git_root,omitempty"`
	Raw     map[string]interface{} `yaml:"-"`
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

	// Keep the raw document around too: Copilot has renamed metadata fields
	// across releases, and resolveID/directoryCandidates use this to fall
	// back to alternate field names rather than hardcoding just one.
	var raw map[string]interface{}
	_ = yaml.Unmarshal(data, &raw)
	meta.Raw = raw

	return &meta, nil
}

// resolveID returns the session identifier, checking known alternate field
// names in case Copilot renamed the "id" field in a newer release.
func (m *sessionMeta) resolveID() string {
	if m.ID != "" {
		return m.ID
	}
	for _, key := range []string{"sessionId", "session_id", "sessionID", "uuid"} {
		if v, ok := m.Raw[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// directoryCandidates returns every path-like value found in the session
// metadata that could represent the project directory. Copilot CLI has used
// different field names for this across versions ("cwd", "git_root", etc.),
// so every plausible field is checked rather than a single hardcoded name.
func (m *sessionMeta) directoryCandidates() []string {
	var candidates []string
	if m.CWD != "" {
		candidates = append(candidates, m.CWD)
	}
	if m.GitRoot != "" {
		candidates = append(candidates, m.GitRoot)
	}

	keys := []string{
		"cwd", "workingDirectory", "working_directory",
		"directory", "dir", "path", "workspace",
		"workspaceFolder", "workspace_folder",
		"gitRoot", "git_root", "repoRoot", "repo_root", "root",
	}
	for _, key := range keys {
		v, ok := m.Raw[key].(string)
		if !ok || v == "" || !strings.HasPrefix(v, "/") {
			continue
		}
		candidates = append(candidates, v)
	}
	return candidates
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
