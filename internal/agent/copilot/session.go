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
	ID      string `yaml:"id"`
	CWD     string `yaml:"cwd"`
	GitRoot string `yaml:"git_root,omitempty"`
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

// firstStringField returns the first non-empty string value found in raw for
// the given candidate keys. Newer Copilot CLI releases have been observed to
// rename metadata fields (e.g. cwd -> workingDirectory); checking multiple
// candidates keeps session discovery working across those renames.
func firstStringField(raw map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := raw[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// parseSessionMeta reads session metadata from a Copilot session directory.
// Tries workspace.yaml first (the historical format), then workspace.json
// (in case a newer Copilot CLI release switched formats), and tolerates
// several possible field name variants for cwd/id.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	if meta, err := parseSessionMetaFile(filepath.Join(sessionDir, "workspace.yaml"), yaml.Unmarshal); err == nil {
		return meta, nil
	}
	return parseSessionMetaFile(filepath.Join(sessionDir, "workspace.json"), json.Unmarshal)
}

// parseSessionMetaFile reads and decodes a session metadata file using the
// given unmarshal function (yaml.Unmarshal or json.Unmarshal).
func parseSessionMetaFile(path string, unmarshal func([]byte, interface{}) error) (*sessionMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var raw map[string]interface{}
	if err := unmarshal(data, &raw); err != nil {
		return nil, err
	}

	return &sessionMeta{
		ID:      firstStringField(raw, "id", "sessionId", "session_id"),
		CWD:     firstStringField(raw, "cwd", "workingDirectory", "working_directory", "directory", "workdir"),
		GitRoot: firstStringField(raw, "git_root", "gitRoot"),
	}, nil
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a
// session directory. Falls back to any other .jsonl file present if
// events.jsonl is missing, in case a newer Copilot CLI release renamed it.
func GetTranscriptPath(sessionDir string) string {
	preferred := filepath.Join(sessionDir, "events.jsonl")
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return preferred
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			return filepath.Join(sessionDir, entry.Name())
		}
	}

	return preferred
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
