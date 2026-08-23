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

// sessionMeta represents lightweight metadata from a Copilot session workspace file.
type sessionMeta struct {
	ID      string `yaml:"id" json:"id"`
	CWD     string `yaml:"cwd" json:"cwd"`
	GitRoot string `yaml:"git_root,omitempty" json:"git_root,omitempty"`
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

// sessionMetaFilenames are the known filenames Copilot CLI has used across
// versions for per-session workspace metadata (id/cwd/git_root). Newer
// releases write JSON instead of YAML, so every known name/format is tried.
var sessionMetaFilenames = []string{
	"workspace.yaml",
	"workspace.yml",
	"workspace.json",
	"session.json",
}

// parseSessionMeta reads session workspace metadata from a Copilot session directory.
// It tries each known filename/format so discovery keeps working across
// Copilot CLI versions that changed the metadata file name or encoding.
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
		lastErr = fmt.Errorf("no session metadata file found in %s", sessionDir)
	}
	return nil, lastErr
}

// transcriptFilenames are the known filenames Copilot CLI has used across
// versions for the per-session event transcript.
var transcriptFilenames = []string{
	"events.jsonl",
	"transcript.jsonl",
	"session.jsonl",
}

// GetTranscriptPath returns the path to the event transcript within a session directory.
// It checks each known filename and returns the first that exists, defaulting to
// events.jsonl (the format shiftlog itself writes when restoring a session).
func GetTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFilenames {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(sessionDir, transcriptFilenames[0])
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
