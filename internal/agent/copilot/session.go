```go
package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
type sessionMeta struct {
	ID      string `yaml:"id"`
	CWD     string `yaml:"cwd"`
	GitRoot string `yaml:"git_root,omitempty"`
}

// pathLikeKeys are workspace.yaml field names (case-insensitive) that have
// been used across Copilot CLI versions to record the session's working
// directory or project root.
var pathLikeKeys = map[string]bool{
	"cwd": true, "pwd": true, "workingdirectory": true,
	"working_directory": true, "directory": true, "dir": true,
	"git_root": true, "gitroot": true, "root": true, "project_root": true,
}

// findPathLikeField searches a decoded YAML document (recursively, since
// some versions nest fields under a sub-object such as "workspace") for a
// string value under a commonly used working-directory field name. This
// tolerates field renames/nesting across Copilot CLI versions.
func findPathLikeField(raw map[string]interface{}) string {
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			if pathLikeKeys[strings.ToLower(k)] && val != "" {
				return val
			}
		case map[string]interface{}:
			if nested := findPathLikeField(val); nested != "" {
				return nested
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

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var meta sessionMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	// Some Copilot CLI versions rename or nest the working-directory field.
	// Fall back to a generic scan so a renamed field doesn't break discovery.
	if meta.CWD == "" && meta.GitRoot == "" {
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err == nil {
			if cwd := findPathLikeField(raw); cwd != "" {
				meta.CWD = cwd
			}
		}
	}

	// The session ID is normally the directory name; use it as a fallback
	// if the "id" field is missing or renamed.
	if meta.ID == "" {
		meta.ID = filepath.Base(sessionDir)
	}

	return &meta, nil
}

// GetTranscriptPath returns the path to the transcript file within a session
// directory. Copilot CLI has used different filenames for this across
// versions, so pick the most recently modified .jsonl file in the
// directory, falling back to the historical "events.jsonl" name if none is
// found (e.g. the directory doesn't exist yet, as when writing a new one).
func GetTranscriptPath(sessionDir string) string {
	fallback := filepath.Join(sessionDir, "events.jsonl")

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return fallback
	}

	var best string
	var bestModTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestModTime) {
			best = entry.Name()
			bestModTime = info.ModTime()
		}
	}

	if best == "" {
		return fallback
	}
	return filepath.Join(sessionDir, best)
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
	eventsPath := filepath.Join(sessionDir, "events.jsonl")
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}
```
