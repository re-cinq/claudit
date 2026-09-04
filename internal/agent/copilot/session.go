```go
package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session's
// workspace file. Copilot CLI has used both YAML (workspace.yaml) and JSON
// (workspace.json) formats for this file across releases, and the working
// directory field has been seen under a few different names, so all known
// variants are populated and resolved via resolvedCWD/resolvedID.
type sessionMeta struct {
	ID          string `yaml:"id,omitempty" json:"id,omitempty"`
	SessionID   string `yaml:"session_id,omitempty" json:"sessionId,omitempty"`
	CWD         string `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	CWDPath     string `yaml:"cwd_path,omitempty" json:"cwdPath,omitempty"`
	Directory   string `yaml:"directory,omitempty" json:"directory,omitempty"`
	WorkingDir  string `yaml:"working_directory,omitempty" json:"workingDirectory,omitempty"`
	GitRoot     string `yaml:"git_root,omitempty" json:"gitRoot,omitempty"`
}

// resolvedID returns the session identifier, checking known field aliases.
func (m *sessionMeta) resolvedID() string {
	if m.ID != "" {
		return m.ID
	}
	return m.SessionID
}

// resolvedCWD returns the session's working directory, checking known field aliases.
func (m *sessionMeta) resolvedCWD() string {
	switch {
	case m.CWD != "":
		return m.CWD
	case m.CWDPath != "":
		return m.CWDPath
	case m.Directory != "":
		return m.Directory
	case m.WorkingDir != "":
		return m.WorkingDir
	default:
		return ""
	}
}

// GetCopilotDir returns the path to Copilot's config/data directory.
func GetCopilotDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".copilot"), nil
}

// GetSessionStateDir returns the primary session state directory.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-state"), nil
}

// GetSessionStateDirs returns candidate session state directories to search,
// in preference order. Copilot CLI has renamed this directory across
// releases, so multiple known names are tried.
func GetSessionStateDirs() ([]string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return nil, err
	}
	return []string{
		filepath.Join(copilotDir, "session-state"),
		filepath.Join(copilotDir, "history-session-state"),
		filepath.Join(copilotDir, "sessions"),
	}, nil
}

// sessionMetaFilenames are the known filenames for per-session metadata,
// in preference order.
var sessionMetaFilenames = []string{
	"workspace.yaml",
	"workspace.yml",
	"workspace.json",
	"session.yaml",
	"session.json",
	"metadata.json",
}

// parseSessionMeta reads session metadata from a Copilot session directory,
// trying each known filename/format until one is found.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error
	for _, name := range sessionMetaFilenames {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}

		var meta sessionMeta
		if strings.HasSuffix(name, ".json") {
			err = json.Unmarshal(data, &meta)
		} else {
			err = yaml.Unmarshal(data, &meta)
		}
		if err != nil {
			lastErr = err
			continue
		}
		return &meta, nil
	}

	if lastErr == nil {
		lastErr = os.ErrNotExist
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
