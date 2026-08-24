```go
package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace file.
// Copilot CLI has used different field names for the working directory across
// versions, so we accept several aliases and resolve them via resolvedCWD().
type sessionMeta struct {
	ID         string `yaml:"id" json:"id"`
	CWD        string `yaml:"cwd" json:"cwd"`
	Workspace  string `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	Directory  string `yaml:"directory,omitempty" json:"directory,omitempty"`
	WorkingDir string `yaml:"workingDirectory,omitempty" json:"workingDirectory,omitempty"`
	GitRoot    string `yaml:"git_root,omitempty" json:"git_root,omitempty"`
}

// resolvedCWD returns the first non-empty working directory field, checking
// every known alias used by different Copilot CLI versions.
func (m *sessionMeta) resolvedCWD() string {
	for _, v := range []string{m.CWD, m.Workspace, m.Directory, m.WorkingDir, m.GitRoot} {
		if v != "" {
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

// sessionStateDirNames are candidate directory names Copilot CLI has used
// across versions for storing session state.
var sessionStateDirNames = []string{"session-state", "sessions", "history-session-state"}

// GetSessionStateDir returns the session state directory. It probes the
// known candidate names and returns the first one that exists on disk,
// falling back to the primary (most common) name if none are present yet.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}

	for _, name := range sessionStateDirNames {
		dir := filepath.Join(copilotDir, name)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}

	return filepath.Join(copilotDir, sessionStateDirNames[0]), nil
}

// parseSessionMeta reads session metadata from a Copilot session directory.
// Supports both workspace.yaml (older CLI versions) and workspace.json
// (newer CLI versions). If the session ID isn't present in the metadata
// file, it falls back to the session directory's own name, since Copilot
// consistently names session directories after their session ID.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	meta := &sessionMeta{}
	found := false

	if data, err := os.ReadFile(filepath.Join(sessionDir, "workspace.yaml")); err == nil {
		if err := yaml.Unmarshal(data, meta); err == nil {
			found = true
		}
	}

	if !found {
		if data, err := os.ReadFile(filepath.Join(sessionDir, "workspace.json")); err == nil {
			if err := json.Unmarshal(data, meta); err == nil {
				found = true
			}
		}
	}

	if !found {
		return nil, os.ErrNotExist
	}

	if meta.ID == "" {
		meta.ID = filepath.Base(sessionDir)
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
```
