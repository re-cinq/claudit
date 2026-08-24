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

// sessionMetaNested handles Copilot CLI versions that nest workspace fields
// under a "workspace" key instead of at the top level.
type sessionMetaNested struct {
	ID        string `yaml:"id"`
	Workspace struct {
		CWD     string `yaml:"cwd"`
		GitRoot string `yaml:"git_root,omitempty"`
	} `yaml:"workspace"`
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
// Newer Copilot CLI releases have nested the workspace fields under a
// "workspace" key rather than keeping them flat; both layouts are supported.
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

	if meta.CWD == "" {
		var nested sessionMetaNested
		if err := yaml.Unmarshal(data, &nested); err == nil && nested.Workspace.CWD != "" {
			meta.CWD = nested.Workspace.CWD
			if meta.ID == "" {
				meta.ID = nested.ID
			}
			if meta.GitRoot == "" {
				meta.GitRoot = nested.Workspace.GitRoot
			}
		}
	}

	return &meta, nil
}

// GetTranscriptPath returns the path to the transcript file within a session directory.
// Copilot CLI has historically named this file events.jsonl. If that file is
// missing, fall back to the most recently modified *.jsonl file in the session
// directory to tolerate filename changes across CLI versions.
func GetTranscriptPath(sessionDir string) string {
	preferred := filepath.Join(sessionDir, "events.jsonl")
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return preferred
	}

	var bestPath string
	var bestModTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestModTime) {
			bestPath = filepath.Join(sessionDir, entry.Name())
			bestModTime = info.ModTime()
		}
	}

	if bestPath != "" {
		return bestPath
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
