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
	ID      string
	CWD     string
	GitRoot string
}

// Field name variants observed across Copilot CLI releases. Newer versions
// have renamed these fields, so we probe several known aliases instead of
// binding to a single key.
var (
	cwdKeys     = []string{"cwd", "cwd_path", "workspaceFolder", "workspace_folder", "directory", "workingDirectory", "working_directory", "projectPath", "project_path", "root", "path"}
	idKeys      = []string{"id", "session_id", "sessionId", "sessionID"}
	gitRootKeys = []string{"git_root", "gitRoot", "git_root_path"}
)

// metaFileNames are the known filenames Copilot CLI has used to store session
// metadata across versions.
var metaFileNames = []string{"workspace.yaml", "workspace.yml", "session.yaml", "session.yml", "metadata.yaml", "metadata.yml", "metadata.json", "session.json"}

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
// tolerating file-format and field-name drift across CLI versions.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	for _, name := range metaFileNames {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		raw := map[string]interface{}{}
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
			ID:      firstStringValue(raw, idKeys),
			CWD:     firstStringValue(raw, cwdKeys),
			GitRoot: firstStringValue(raw, gitRootKeys),
		}, nil
	}

	return nil, fmt.Errorf("no session metadata file found in %s", sessionDir)
}

// firstStringValue returns the first non-empty string value found in raw for
// any of the given candidate keys.
func firstStringValue(raw map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// transcriptFileNames are the known filenames Copilot CLI has used for the
// per-session event transcript across versions.
var transcriptFileNames = []string{"events.jsonl", "transcript.jsonl", "session.jsonl", "history.jsonl", "messages.jsonl"}

// GetTranscriptPath returns the path to the event transcript within a session
// directory. It probes known filenames and falls back to the first .jsonl
// file present, defaulting to events.jsonl if none is found (e.g. when called
// before writing a new session).
func GetTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFileNames {
		p := filepath.Join(sessionDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	if entries, err := os.ReadDir(sessionDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				return filepath.Join(sessionDir, e.Name())
			}
		}
	}

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
	eventsPath := filepath.Join(sessionDir, "events.jsonl")
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}
```
