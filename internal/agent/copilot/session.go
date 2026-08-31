```go
package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session's
// per-session workspace file.
type sessionMeta struct {
	ID  string
	CWD string
}

// sessionMetaFilenames are the known/candidate filenames Copilot CLI has
// used for per-session workspace metadata across releases.
var sessionMetaFilenames = []string{
	"workspace.yaml",
	"workspace.yml",
	"session.yaml",
	"meta.yaml",
	"metadata.yaml",
}

// sessionMetaIDKeys and sessionMetaCWDKeys are candidate field names for the
// session id and working directory. Copilot CLI has renamed these fields
// across releases, so several known variants are checked.
var sessionMetaIDKeys = []string{"id", "session_id", "sessionId", "sessionID"}
var sessionMetaCWDKeys = []string{
	"cwd", "working_directory", "workingDirectory", "directory", "dir",
	"workspace", "cwd_path", "project_path", "projectPath", "path",
}

// firstStringField looks up the first matching key (case-insensitive) in a
// generic map and returns its string value, or "" if none match.
func firstStringField(raw map[string]interface{}, keys []string) string {
	lowered := make(map[string]interface{}, len(raw))
	for k, v := range raw {
		lowered[strings.ToLower(k)] = v
	}
	for _, key := range keys {
		if v, ok := lowered[strings.ToLower(key)]; ok {
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

// parseSessionMeta reads per-session workspace metadata from a Copilot
// session directory. It tries several known filenames and field names since
// Copilot CLI has changed this schema across releases.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var raw map[string]interface{}
	var found bool

	for _, name := range sessionMetaFilenames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			continue
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			continue
		}
		found = true
		break
	}

	if !found {
		return nil, os.ErrNotExist
	}

	return &sessionMeta{
		ID:  firstStringField(raw, sessionMetaIDKeys),
		CWD: firstStringField(raw, sessionMetaCWDKeys),
	}, nil
}

// transcriptFilenames are the known/candidate names Copilot CLI has used for
// a session's event transcript across releases.
var transcriptFilenames = []string{"events.jsonl", "transcript.jsonl", "session.jsonl", "log.jsonl"}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
// This is the canonical filename shiftlog itself writes when restoring a session.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// findTranscriptPath returns the path to the first existing transcript file
// within a session directory, checking known filename variants used across
// Copilot CLI releases. Returns "" if none exist.
func findTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFilenames {
		p := filepath.Join(sessionDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
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
