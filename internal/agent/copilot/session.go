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

// rawSessionMeta mirrors the possible shapes Copilot CLI's workspace.yaml has
// used across releases: flat top-level keys under a few different names for
// the working directory, or those same keys nested under a "workspace"
// mapping. We parse permissively and pick the first populated alias so that
// a Copilot CLI upgrade that renames or nests these fields doesn't silently
// break session discovery.
type rawSessionMeta struct {
	ID         string `yaml:"id"`
	SessionID  string `yaml:"session_id"`
	CWD        string `yaml:"cwd"`
	CWDPath    string `yaml:"cwd_path"`
	Directory  string `yaml:"directory"`
	WorkingDir string `yaml:"working_directory"`
	GitRoot    string `yaml:"git_root"`
	Workspace  *struct {
		ID        string `yaml:"id"`
		CWD       string `yaml:"cwd"`
		Directory string `yaml:"directory"`
		GitRoot   string `yaml:"git_root"`
	} `yaml:"workspace"`
}

// firstNonEmpty returns the first non-empty string among vals.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// toSessionMeta collapses the permissive raw shape into the canonical sessionMeta.
func (r rawSessionMeta) toSessionMeta() *sessionMeta {
	m := &sessionMeta{
		ID:      firstNonEmpty(r.ID, r.SessionID),
		CWD:     firstNonEmpty(r.CWD, r.CWDPath, r.Directory, r.WorkingDir),
		GitRoot: r.GitRoot,
	}
	if r.Workspace != nil {
		m.ID = firstNonEmpty(m.ID, r.Workspace.ID)
		m.CWD = firstNonEmpty(m.CWD, r.Workspace.CWD, r.Workspace.Directory)
		m.GitRoot = firstNonEmpty(m.GitRoot, r.Workspace.GitRoot)
	}
	return m
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

	var raw rawSessionMeta
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	return raw.toSessionMeta(), nil
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
