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

// metadataFileNames lists the file names Copilot CLI has used across releases
// to record per-session workspace metadata. Earlier releases used
// "workspace.yaml"; newer releases have been observed to use other names
// and/or JSON instead of YAML.
var metadataFileNames = []string{
	"workspace.yaml",
	"workspace.yml",
	"workspace.json",
	"session.yaml",
	"session.json",
	"metadata.yaml",
	"metadata.json",
}

// cwdFieldNames lists the metadata keys Copilot CLI has used across releases
// to record a session's working directory.
var cwdFieldNames = []string{
	"cwd",
	"workingDirectory",
	"working_directory",
	"directory",
	"workspaceFolder",
	"workspace_folder",
	"path",
	"root",
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

// parseSessionMeta reads per-session workspace metadata from a Copilot
// session directory. It tries several known metadata file names and
// encodings (YAML and JSON), since the exact file name has changed across
// Copilot CLI releases.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error = os.ErrNotExist

	for _, name := range metadataFileNames {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}

		if meta := decodeSessionMeta(data, strings.HasSuffix(name, ".json")); meta != nil {
			return meta, nil
		}
	}

	return nil, lastErr
}

// decodeSessionMeta parses workspace metadata bytes (YAML or JSON) into a
// sessionMeta, tolerating field-name drift across Copilot CLI releases.
func decodeSessionMeta(data []byte, isJSON bool) *sessionMeta {
	var raw map[string]interface{}
	var err error
	if isJSON {
		err = json.Unmarshal(data, &raw)
	} else {
		err = yaml.Unmarshal(data, &raw)
	}
	if err != nil || raw == nil {
		return nil
	}

	meta := &sessionMeta{}
	if id, ok := raw["id"].(string); ok {
		meta.ID = id
	}

	meta.CWD = extractStringField(raw, cwdFieldNames)
	if meta.CWD == "" {
		if ws, ok := raw["workspace"].(map[string]interface{}); ok {
			meta.CWD = extractStringField(ws, cwdFieldNames)
		}
	}

	if gr, ok := raw["git_root"].(string); ok {
		meta.GitRoot = gr
	}

	return meta
}

// extractStringField returns the first non-empty string value found in m
// among the given candidate keys.
func extractStringField(m map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// GetTranscriptPath returns the path to the events transcript within a
// session directory. Prefers the standard events.jsonl name, but falls back
// to any other .jsonl file present so that a rename in a newer Copilot CLI
// release doesn't break discovery of an existing transcript.
func GetTranscriptPath(sessionDir string) string {
	primary := filepath.Join(sessionDir, "events.jsonl")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}

	if entries, err := os.ReadDir(sessionDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				return filepath.Join(sessionDir, e.Name())
			}
		}
	}

	return primary
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
