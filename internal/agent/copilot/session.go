```go
package copilot

import (
	"fmt"
	"os"
	"path/filepath"

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

// metadataFilenames are the filenames Copilot CLI has used across versions
// for the per-session workspace metadata file.
var metadataFilenames = []string{
	"workspace.yaml", "workspace.yml", "workspace.json",
	"session.yaml", "session.json",
	"metadata.yaml", "metadata.json",
}

// cwdKeys are the keys Copilot CLI has used across versions to record a
// session's working directory in its metadata file.
var cwdKeys = []string{
	"cwd", "cwdPath", "workingDirectory", "working_dir",
	"directory", "workspacePath", "workspace_path", "path", "root",
}

// idKeys are the keys Copilot CLI has used across versions to record the
// session ID inside its metadata file.
var idKeys = []string{"id", "sessionId", "session_id", "sessionID"}

// parseSessionMeta reads the workspace metadata file from a Copilot session
// directory. Copilot CLI has changed both the filename and the field names
// used for this across versions, so several candidates of each are tried.
// Keys are also looked up one level deep under a "workspace" object in case
// metadata is nested rather than flat.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	for _, name := range metadataFilenames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			continue
		}

		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			continue
		}

		meta := &sessionMeta{
			ID:  lookupString(raw, idKeys),
			CWD: lookupString(raw, cwdKeys),
		}
		if meta.ID != "" || meta.CWD != "" {
			return meta, nil
		}
	}

	return nil, os.ErrNotExist
}

// lookupString finds the first non-empty string value for any of the given
// keys, checking the top level of raw and one level of nesting under a
// "workspace" object.
func lookupString(raw map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := raw[k].(string); ok && v != "" {
			return v
		}
	}
	if nested, ok := raw["workspace"].(map[string]interface{}); ok {
		for _, k := range keys {
			if v, ok := nested[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// transcriptFilenames are the filenames Copilot CLI has used across versions
// for the per-session transcript/events log.
var transcriptFilenames = []string{"events.jsonl", "transcript.jsonl", "session.jsonl", "history.jsonl"}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// findTranscriptPath locates whichever transcript file is present in a
// session directory, trying known filenames in order. Returns "" if none
// of the known filenames exist.
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
