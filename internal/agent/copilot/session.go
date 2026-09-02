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

// GetCopilotDir returns the path to Copilot's config/data directory.
func GetCopilotDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".copilot"), nil
}

// sessionStateDirNames are the known directory names Copilot CLI has used
// across versions to store per-session state.
var sessionStateDirNames = []string{"session-state", "history-session-state", "sessions", "history"}

// GetSessionStateDir returns the primary session state directory, used when
// writing a restored session to disk.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, sessionStateDirNames[0]), nil
}

// GetSessionStateDirs returns every known candidate session state directory
// that currently exists on disk. Copilot CLI has renamed this directory
// across versions, so callers scanning for existing sessions should check
// all of them rather than assuming a single fixed location.
func GetSessionStateDirs() ([]string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return nil, err
	}

	var dirs []string
	for _, name := range sessionStateDirNames {
		dir := filepath.Join(copilotDir, name)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// metaFileNames are the known metadata filenames Copilot CLI has used across
// versions to describe a session (working directory, id, etc.).
var metaFileNames = []string{"workspace.yaml", "workspace.json", "session.yaml", "session.json", "state.yaml", "state.json"}

// cwdFieldNames are the known field names Copilot CLI has used for a
// session's working directory across versions.
var cwdFieldNames = []string{"cwd", "cwdPath", "workingDirectory", "directory", "projectPath", "project_path", "workspace", "workspacePath"}

// idFieldNames are the known field names Copilot CLI has used for the
// session identifier across versions.
var idFieldNames = []string{"id", "sessionId", "session_id", "sessionID"}

// gitRootFieldNames are the known field names for the session's git root.
var gitRootFieldNames = []string{"git_root", "gitRoot"}

// parseSessionMeta reads session metadata from a Copilot session directory.
// It tries several known metadata filenames and field name variants since
// Copilot CLI has changed both across versions.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	for _, name := range metaFileNames {
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

		meta := &sessionMeta{}
		for _, f := range idFieldNames {
			if v, ok := raw[f].(string); ok && v != "" {
				meta.ID = v
				break
			}
		}
		for _, f := range cwdFieldNames {
			if v, ok := raw[f].(string); ok && v != "" {
				meta.CWD = v
				break
			}
		}
		for _, f := range gitRootFieldNames {
			if v, ok := raw[f].(string); ok && v != "" {
				meta.GitRoot = v
				break
			}
		}

		if meta.ID == "" {
			meta.ID = filepath.Base(sessionDir)
		}

		return meta, nil
	}

	return nil, fmt.Errorf("no session metadata found in %s", sessionDir)
}

// transcriptFileNames are the known transcript filenames Copilot CLI has
// used across versions within a session directory.
var transcriptFileNames = []string{"events.jsonl", "transcript.jsonl", "messages.jsonl", "session.jsonl", "history.jsonl"}

// GetTranscriptPath returns the path to the transcript file within a session
// directory. It checks known filenames used across Copilot CLI versions and
// falls back to the default write location if none are found on disk.
func GetTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFileNames {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(sessionDir, transcriptFileNames[0])
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
	eventsPath := filepath.Join(sessionDir, transcriptFileNames[0])
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}
```
