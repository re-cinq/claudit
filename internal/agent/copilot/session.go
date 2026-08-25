```go
package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session's
// workspace file.
type sessionMeta struct {
	ID      string
	CWD     string
	GitRoot string
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

// workspaceFileNames are the known filenames Copilot CLI has used across
// releases to store per-session workspace metadata.
var workspaceFileNames = []string{
	"workspace.yaml",
	"workspace.yml",
	"workspace.json",
	"session.yaml",
	"session.json",
	"metadata.yaml",
	"metadata.json",
	"state.yaml",
	"state.json",
}

// cwdKeys are the known keys Copilot CLI has used for the session's working directory.
var cwdKeys = []string{
	"cwd", "cwd_path", "workingDirectory", "working_directory",
	"workspaceFolder", "workspace_folder", "directory", "workdir", "path",
}

// idKeys are the known keys Copilot CLI has used for the session ID.
var idKeys = []string{"id", "sessionId", "session_id", "sessionID"}

// gitRootKeys are the known keys Copilot CLI has used for the git root path.
var gitRootKeys = []string{"git_root", "gitRoot", "root", "repoRoot", "repo_root"}

// parseSessionMeta reads a session's workspace metadata, trying every known
// filename and field name Copilot CLI has used across releases. Both YAML
// and JSON contents parse fine via yaml.Unmarshal, since JSON is valid YAML.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error
	for _, name := range workspaceFileNames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			lastErr = err
			continue
		}

		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			lastErr = err
			continue
		}

		meta := &sessionMeta{
			ID:      firstStringValue(raw, idKeys),
			CWD:     firstStringValue(raw, cwdKeys),
			GitRoot: firstStringValue(raw, gitRootKeys),
		}

		// Some releases nest workspace info under a "workspace" object.
		if nested, ok := raw["workspace"].(map[string]interface{}); ok {
			if meta.CWD == "" {
				meta.CWD = firstStringValue(nested, cwdKeys)
			}
			if meta.GitRoot == "" {
				meta.GitRoot = firstStringValue(nested, gitRootKeys)
			}
		}

		return meta, nil
	}
	return nil, lastErr
}

// firstStringValue returns the first non-empty string value found in m for
// any of the given candidate keys.
func firstStringValue(m map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
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
