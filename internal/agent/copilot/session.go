```go
package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace
// metadata file. Copilot CLI has used different field names for the session
// id and working directory across releases (cwd, cwd_path, workingDirectory,
// directory), so all known variants are captured here and resolved through
// the ID() and CWD() accessors instead of reading the fields directly.
type sessionMeta struct {
	IDField         string `yaml:"id,omitempty" json:"id,omitempty"`
	SessionIDField  string `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	SessionIDField2 string `yaml:"sessionId,omitempty" json:"sessionId,omitempty"`
	CWDField        string `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	CWDPathField    string `yaml:"cwd_path,omitempty" json:"cwd_path,omitempty"`
	WorkingDirField string `yaml:"workingDirectory,omitempty" json:"workingDirectory,omitempty"`
	DirectoryField  string `yaml:"directory,omitempty" json:"directory,omitempty"`
	GitRoot         string `yaml:"git_root,omitempty" json:"git_root,omitempty"`
}

// ID returns the session identifier, checking known field name variants.
func (m *sessionMeta) ID() string {
	for _, v := range []string{m.IDField, m.SessionIDField, m.SessionIDField2} {
		if v != "" {
			return v
		}
	}
	return ""
}

// CWD returns the session's working directory, checking known field name variants.
func (m *sessionMeta) CWD() string {
	for _, v := range []string{m.CWDField, m.CWDPathField, m.WorkingDirField, m.DirectoryField} {
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

// GetSessionStateDir returns the session state directory.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-state"), nil
}

// sessionStateDirNames lists the known names Copilot CLI has used for its
// session state directory across releases. Discovery scans all of them so a
// rename between versions doesn't silently break session discovery.
var sessionStateDirNames = []string{"session-state", "history-session-state"}

// SessionStateDirs returns every candidate session state directory that
// exists under Copilot's config/data directory.
func SessionStateDirs() ([]string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return nil, err
	}

	var dirs []string
	for _, name := range sessionStateDirNames {
		dirs = append(dirs, filepath.Join(copilotDir, name))
	}
	return dirs, nil
}

// parseSessionMeta reads workspace metadata from a Copilot session directory.
// Copilot CLI releases have used both YAML (workspace.yaml) and JSON
// (workspace.json) metadata files, so all known variants are tried.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	candidates := []string{"workspace.yaml", "workspace.yml", "workspace.json"}

	var lastErr error = os.ErrNotExist
	for _, name := range candidates {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}

		var meta sessionMeta
		if filepath.Ext(name) == ".json" {
			if err := json.Unmarshal(data, &meta); err != nil {
				lastErr = err
				continue
			}
		} else {
			if err := yaml.Unmarshal(data, &meta); err != nil {
				lastErr = err
				continue
			}
		}
		return &meta, nil
	}

	return nil, lastErr
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
	meta := sessionMeta{IDField: sessionID}
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
