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
type sessionMeta struct {
	ID  string
	CWD string
}

// sessionMetaFilenames are the metadata filenames Copilot CLI has used across
// versions to describe a session's workspace. Newer releases have moved from
// workspace.yaml towards JSON variants, so each candidate is tried in turn.
var sessionMetaFilenames = []string{"workspace.yaml", "workspace.yml", "session.yaml", "workspace.json", "session.json"}

// transcriptFilenames are the transcript filenames Copilot CLI has used
// across versions, tried in order of preference.
var transcriptFilenames = []string{"events.jsonl", "transcript.jsonl", "session.jsonl"}

// cwdKeyPaths are the field paths (including nested objects) Copilot has used
// to record a session's working directory across versions.
var cwdKeyPaths = [][]string{
	{"cwd"},
	{"cwd_path"},
	{"workingDirectory"},
	{"working_directory"},
	{"directory"},
	{"path"},
	{"workspace", "cwd"},
	{"workspace", "path"},
	{"workspace", "directory"},
}

// idKeyPaths are the field paths Copilot has used to record a session's ID.
var idKeyPaths = [][]string{
	{"id"},
	{"sessionId"},
	{"session_id"},
	{"workspace", "id"},
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

// lookupPath walks a nested map[string]interface{} following the given key
// path and returns the value found, or nil if any segment is missing.
func lookupPath(raw map[string]interface{}, path []string) interface{} {
	var cur interface{} = raw
	for _, key := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		val, exists := m[key]
		if !exists {
			return nil
		}
		cur = val
	}
	return cur
}

// lookupString tries each candidate field path in turn and returns the first
// non-empty string value found.
func lookupString(raw map[string]interface{}, paths [][]string) string {
	for _, path := range paths {
		if s, ok := lookupPath(raw, path).(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// parseSessionMeta reads workspace metadata from a Copilot session directory.
// It tolerates the metadata living under different filenames/formats and the
// cwd/id fields living under different (possibly nested) keys, since these
// have changed across Copilot CLI releases.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error = os.ErrNotExist

	for _, name := range sessionMetaFilenames {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}

		var raw map[string]interface{}
		if filepath.Ext(name) == ".json" {
			err = json.Unmarshal(data, &raw)
		} else {
			err = yaml.Unmarshal(data, &raw)
		}
		if err != nil {
			lastErr = err
			continue
		}

		meta := &sessionMeta{
			ID:  lookupString(raw, idKeyPaths),
			CWD: lookupString(raw, cwdKeyPaths),
		}
		if meta.ID != "" || meta.CWD != "" {
			return meta, nil
		}
	}

	return nil, lastErr
}

// findTranscriptFile locates the transcript file within a session directory,
// trying known filenames in order of preference. Falls back to the primary
// "events.jsonl" name (used for shiftlog-restored sessions) if none exist yet.
func findTranscriptFile(sessionDir string) string {
	for _, name := range transcriptFilenames {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(sessionDir, transcriptFilenames[0])
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
	meta := struct {
		ID string `yaml:"id"`
	}{ID: sessionID}
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
