```go
package copilot

import (
	"fmt"
	"os"
	"path/filepath"

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

// candidateIDKeys, candidateCWDKeys and candidateGitRootKeys list the field
// names Copilot CLI has used (across releases) for a session's identifier,
// working directory, and git root in workspace.yaml. Parsing checks all of
// them so discovery keeps working even if a release renames a field.
// candidateNestedKeys covers releases that wrap the metadata under a key
// instead of storing it at the document root.
var candidateIDKeys = []string{"id", "sessionId", "sessionID", "session_id"}
var candidateCWDKeys = []string{"cwd", "cwdPath", "workingDirectory", "workingDir", "directory", "dir"}
var candidateGitRootKeys = []string{"git_root", "gitRoot", "gitRootPath"}
var candidateNestedKeys = []string{"workspace", "session", "meta"}

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	meta := &sessionMeta{
		ID:      firstStringField(raw, candidateIDKeys),
		CWD:     firstStringField(raw, candidateCWDKeys),
		GitRoot: firstStringField(raw, candidateGitRootKeys),
	}

	for _, key := range candidateNestedKeys {
		nested, ok := asStringMap(raw[key])
		if !ok {
			continue
		}
		if meta.ID == "" {
			meta.ID = firstStringField(nested, candidateIDKeys)
		}
		if meta.CWD == "" {
			meta.CWD = firstStringField(nested, candidateCWDKeys)
		}
		if meta.GitRoot == "" {
			meta.GitRoot = firstStringField(nested, candidateGitRootKeys)
		}
	}

	return meta, nil
}

// firstStringField returns the first non-empty string value found in m for
// any of the given keys.
func firstStringField(m map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// asStringMap converts a YAML-decoded value into map[string]interface{},
// handling both the map[string]interface{} and map[interface{}]interface{}
// shapes a YAML decoder may produce for nested mappings.
func asStringMap(v interface{}) (map[string]interface{}, bool) {
	switch m := v.(type) {
	case map[string]interface{}:
		return m, true
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(m))
		for k, vv := range m {
			if ks, ok := k.(string); ok {
				out[ks] = vv
			}
		}
		return out, true
	default:
		return nil, false
	}
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
