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

// sessionMetaFilenames are the filenames Copilot CLI has used across versions
// to store session workspace metadata.
var sessionMetaFilenames = []string{
	"workspace.yaml", "workspace.json",
	"session.yaml", "session.json",
	"state.yaml", "state.json",
	"metadata.yaml", "metadata.json",
}

// sessionMetaCWDKeys are the field names Copilot CLI has used across versions
// to store a session's working directory.
var sessionMetaCWDKeys = []string{
	"cwd", "workingDirectory", "working_directory", "directory", "projectPath", "project_path", "path",
}

// sessionMetaIDKeys are the field names Copilot CLI has used across versions
// to store a session's ID.
var sessionMetaIDKeys = []string{"id", "sessionId", "session_id", "sessionID"}

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

// parseSessionMeta reads session metadata from a Copilot session directory,
// trying multiple known filenames and field names since both have changed
// across Copilot CLI versions.
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
		if err := yaml.Unmarshal(data, &raw); err != nil {
			lastErr = err
			continue
		}

		meta := &sessionMeta{}
		for _, key := range sessionMetaCWDKeys {
			if v, ok := raw[key].(string); ok && v != "" {
				meta.CWD = v
				break
			}
		}
		for _, key := range sessionMetaIDKeys {
			if v, ok := raw[key].(string); ok && v != "" {
				meta.ID = v
				break
			}
		}
		if gr, ok := raw["git_root"].(string); ok {
			meta.GitRoot = gr
		}

		return meta, nil
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
