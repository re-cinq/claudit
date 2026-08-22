package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session's
// workspace/session metadata file.
type sessionMeta struct {
	ID      string `yaml:"id"`
	CWD     string `yaml:"cwd"`
	GitRoot string `yaml:"git_root,omitempty"`
}

// sessionMetaFilenames lists the metadata filenames Copilot CLI has used
// across releases, tried in order. Newer CLI versions have been observed
// renaming workspace.yaml to session.yaml/meta.json, so several candidates
// are tried instead of assuming a single fixed name.
var sessionMetaFilenames = []string{
	"workspace.yaml",
	"workspace.yml",
	"session.yaml",
	"session.yml",
	"meta.yaml",
	"workspace.json",
	"session.json",
	"meta.json",
}

// cwdKeys, idKeys, and gitRootKeys list the field names Copilot CLI has used
// for the corresponding metadata values across releases.
var cwdKeys = []string{"cwd", "workingDirectory", "working_dir", "workingDir", "directory", "dir", "path", "workspace", "workspaceFolder"}
var idKeys = []string{"id", "sessionId", "sessionID", "session_id"}
var gitRootKeys = []string{"git_root", "gitRoot", "root"}

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

// parseSessionMeta reads session metadata from a Copilot session directory.
// Copilot CLI has changed the metadata filename and field names across
// releases, so several filenames and key aliases are tried before giving up.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	for _, name := range sessionMetaFilenames {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		raw := map[string]interface{}{}
		var unmarshalErr error
		if filepath.Ext(name) == ".json" {
			unmarshalErr = json.Unmarshal(data, &raw)
		} else {
			unmarshalErr = yaml.Unmarshal(data, &raw)
		}
		if unmarshalErr != nil {
			continue
		}

		meta := &sessionMeta{
			ID:      firstStringValue(raw, idKeys),
			CWD:     firstStringValue(raw, cwdKeys),
			GitRoot: firstStringValue(raw, gitRootKeys),
		}
		if meta.ID == "" {
			// Fall back to the directory name, which is the session ID
			// in every observed Copilot CLI layout.
			meta.ID = filepath.Base(sessionDir)
		}
		return meta, nil
	}

	return nil, os.ErrNotExist
}

// firstStringValue returns the first non-empty string value found in m for
// any of the given keys.
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
