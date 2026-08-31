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
	ID      string `yaml:"id" json:"id"`
	CWD     string `yaml:"cwd" json:"cwd"`
	GitRoot string `yaml:"git_root,omitempty" json:"git_root,omitempty"`
}

// GetCopilotDir returns the path to Copilot's config/data directory.
func GetCopilotDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".copilot"), nil
}

// GetSessionStateDir returns the default session state directory.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-state"), nil
}

// sessionStateDirNames are directory names Copilot CLI has used (across versions)
// to store per-session state under its config directory. "session-state" is the
// documented default, but newer releases have been observed renaming it.
var sessionStateDirNames = []string{"session-state", "sessions", "history-state", "state"}

// CandidateSessionStateDirs returns every existing directory under Copilot's config
// directory that could hold per-session state. This tolerates the session state
// directory being renamed across CLI versions instead of assuming a single fixed path.
func CandidateSessionStateDirs() []string {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return nil
	}

	var dirs []string
	seen := make(map[string]bool)
	for _, name := range sessionStateDirNames {
		p := filepath.Join(copilotDir, name)
		if seen[p] {
			continue
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			dirs = append(dirs, p)
			seen[p] = true
		}
	}
	return dirs
}

// sessionMetaFileNames are the metadata filenames Copilot CLI has used to describe
// a session directory; both the filename and the encoding have changed across versions.
var sessionMetaFileNames = []string{"workspace.yaml", "workspace.json"}

// parseSessionMetaFile reads session metadata from a workspace.yaml or workspace.json file.
func parseSessionMetaFile(path string) (*sessionMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var meta sessionMeta
	if strings.HasSuffix(path, ".json") {
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil, err
		}
		return &meta, nil
	}

	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
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
