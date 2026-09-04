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

// sessionMeta represents lightweight metadata from a Copilot session directory.
type sessionMeta struct {
	ID  string `yaml:"id"`
	CWD string `yaml:"cwd"`
}

// sessionMetaFilenames are the known Copilot CLI session metadata filenames,
// checked in priority order. Copilot CLI has changed the metadata filename
// across versions, so we probe several candidates rather than assuming one.
var sessionMetaFilenames = []string{
	"workspace.yaml", "workspace.yml",
	"session.yaml", "session.yml",
	"metadata.yaml", "metadata.yml",
	"state.yaml", "state.yml",
	"workspace.json", "session.json", "metadata.json", "state.json",
}

// idFieldAliases and cwdFieldAliases list the known field name variants
// Copilot CLI has used for the session ID and working directory across versions.
var idFieldAliases = []string{"id", "sessionId", "session_id", "sessionID"}
var cwdFieldAliases = []string{"cwd", "cwdPath", "cwd_path", "workingDirectory", "working_directory", "directory", "dir", "path"}

// transcriptFilenames are the known Copilot CLI transcript filenames, checked
// in priority order when discovering an existing session's transcript.
var transcriptFilenames = []string{"events.jsonl", "transcript.jsonl", "session.jsonl", "history.jsonl"}

// stringFromAliases returns the first non-empty string value found in m
// under any of the given keys.
func stringFromAliases(m map[string]interface{}, aliases []string) string {
	for _, key := range aliases {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
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

// parseSessionMeta reads Copilot session metadata from a session directory,
// tolerating the filename and field-name variations Copilot CLI has used
// across versions.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	for _, name := range sessionMetaFilenames {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		raw := make(map[string]interface{})
		if strings.HasSuffix(name, ".json") {
			if err := json.Unmarshal(data, &raw); err != nil {
				continue
			}
		} else {
			if err := yaml.Unmarshal(data, &raw); err != nil {
				continue
			}
		}

		return &sessionMeta{
			ID:  stringFromAliases(raw, idFieldAliases),
			CWD: stringFromAliases(raw, cwdFieldAliases),
		}, nil
	}

	return nil, os.ErrNotExist
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// findTranscriptFile locates the transcript file within a Copilot session
// directory, tolerating the filename differences Copilot CLI has used across
// versions. Falls back to any *.jsonl file in the directory, and finally to
// the legacy default name if nothing else matches.
func findTranscriptFile(sessionDir string) string {
	for _, name := range transcriptFilenames {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	entries, err := os.ReadDir(sessionDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				return filepath.Join(sessionDir, e.Name())
			}
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
