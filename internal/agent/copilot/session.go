```go
package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
// Newer Copilot CLI releases have used different field names for the same
// concepts (e.g. "cwd" vs "directory" vs "workingDirectory"), so several
// aliases are captured and reconciled via effectiveCWD/effectiveID.
type sessionMeta struct {
	ID               string `yaml:"id"`
	SessionID        string `yaml:"sessionId,omitempty"`
	CWD              string `yaml:"cwd"`
	Directory        string `yaml:"directory,omitempty"`
	WorkingDirectory string `yaml:"workingDirectory,omitempty"`
	GitRoot          string `yaml:"git_root,omitempty"`
}

// effectiveCWD returns the first non-empty working-directory field, tolerating
// field-name changes across Copilot CLI versions.
func (m *sessionMeta) effectiveCWD() string {
	for _, v := range []string{m.CWD, m.Directory, m.WorkingDirectory, m.GitRoot} {
		if v != "" {
			return v
		}
	}
	return ""
}

// effectiveID returns the first non-empty session identifier field.
func (m *sessionMeta) effectiveID() string {
	if m.ID != "" {
		return m.ID
	}
	return m.SessionID
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

// sessionMetaFilenames lists the metadata filenames used by different Copilot
// CLI releases, tried in order.
var sessionMetaFilenames = []string{"workspace.yaml", "session.yaml", "meta.yaml", "workspace.json"}

// parseSessionMeta reads session metadata from a Copilot session directory,
// trying several known filenames since the CLI has renamed this file across
// releases.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error
	for _, name := range sessionMetaFilenames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			lastErr = err
			continue
		}

		var meta sessionMeta
		if err := yaml.Unmarshal(data, &meta); err != nil {
			lastErr = err
			continue
		}

		if meta.effectiveID() != "" || meta.effectiveCWD() != "" {
			return &meta, nil
		}
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return nil, lastErr
}

// transcriptFilenames lists the transcript filenames used by different
// Copilot CLI releases, tried in order.
var transcriptFilenames = []string{"events.jsonl", "session.jsonl", "transcript.jsonl", "history.jsonl"}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// resolveTranscriptPath finds the transcript file within an existing session
// directory, tolerating filename changes across Copilot CLI releases. Falls
// back to the default events.jsonl path if none of the known names exist.
func resolveTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFilenames {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
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
```
