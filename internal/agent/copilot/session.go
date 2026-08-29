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

// sessionMetaCandidates are the workspace metadata filenames tried, in
// order, when discovering a Copilot session. Copilot CLI has used different
// filenames and formats (YAML/JSON) across releases.
var sessionMetaCandidates = []string{"workspace.yaml", "workspace.yml", "workspace.json", "session.yaml", "session.json"}

// parseSessionMeta reads a session's workspace metadata file from a Copilot
// session directory. It tolerates the filename/format and field-name
// variations that have appeared across Copilot CLI releases by decoding
// into a generic map and checking several candidate keys per field.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var data []byte
	var name string
	for _, candidate := range sessionMetaCandidates {
		d, err := os.ReadFile(filepath.Join(sessionDir, candidate))
		if err != nil {
			continue
		}
		data = d
		name = candidate
		break
	}
	if data == nil {
		return nil, os.ErrNotExist
	}

	raw := make(map[string]interface{})
	if strings.HasSuffix(name, ".json") {
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
	} else {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
	}

	return &sessionMeta{
		ID:      firstStringValue(raw, "id", "sessionId", "session_id", "sessionID"),
		CWD:     firstStringValue(raw, "cwd", "cwdPath", "workingDirectory", "workspaceFolder", "directory"),
		GitRoot: firstStringValue(raw, "git_root", "gitRoot", "repoRoot", "repo_root"),
	}, nil
}

// firstStringValue returns the first non-empty string value found in m for
// the given candidate keys, or "" if none match.
func firstStringValue(m map[string]interface{}, keys ...string) string {
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
	meta := map[string]string{"id": sessionID}
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
