```go
package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
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

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
//
// Copilot CLI has changed the shape of workspace.yaml across releases (e.g.
// nesting id/cwd under a "workspace" or "session" key instead of at the
// document root). We parse the expected top-level shape first, and if either
// field comes back empty, fall back to a generic recursive scan of the YAML
// document for an id/cwd-like key so we stay compatible with schema drift.
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

	if meta.ID == "" || meta.CWD == "" {
		var generic interface{}
		if err := yaml.Unmarshal(data, &generic); err == nil {
			if meta.CWD == "" {
				if v, ok := findYAMLString(generic, "cwd", "directory", "workingdirectory", "workingdir", "root", "path"); ok {
					meta.CWD = v
				}
			}
			if meta.ID == "" {
				if v, ok := findYAMLString(generic, "id", "sessionid", "session_id", "workspaceid"); ok {
					meta.ID = v
				}
			}
		}
	}

	return &meta, nil
}

// findYAMLString recursively searches a decoded YAML document for the first
// string value whose key case-insensitively matches one of the given names.
func findYAMLString(node interface{}, keys ...string) (string, bool) {
	switch v := node.(type) {
	case map[string]interface{}:
		for k, val := range v {
			if s, ok := val.(string); ok && s != "" && matchesAny(k, keys) {
				return s, true
			}
		}
		for _, val := range v {
			if s, ok := findYAMLString(val, keys...); ok {
				return s, true
			}
		}
	case map[interface{}]interface{}:
		for k, val := range v {
			ks, _ := k.(string)
			if s, ok := val.(string); ok && s != "" && matchesAny(ks, keys) {
				return s, true
			}
		}
		for _, val := range v {
			if s, ok := findYAMLString(val, keys...); ok {
				return s, true
			}
		}
	case []interface{}:
		for _, item := range v {
			if s, ok := findYAMLString(item, keys...); ok {
				return s, true
			}
		}
	}
	return "", false
}

// matchesAny reports whether key case-insensitively equals any of names.
func matchesAny(key string, names []string) bool {
	for _, n := range names {
		if strings.EqualFold(key, n) {
			return true
		}
	}
	return false
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
