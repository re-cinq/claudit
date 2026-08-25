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
	ID      string `yaml:"id"`
	CWD     string `yaml:"cwd"`
	GitRoot string `yaml:"git_root,omitempty"`
}

// workspaceMetaFilenames lists candidate metadata filenames within a Copilot
// session directory, in priority order. workspace.yaml has been the primary
// one, but Copilot CLI has shifted filenames/formats across versions.
var workspaceMetaFilenames = []string{
	"workspace.yaml",
	"workspace.yml",
	"workspace.json",
	"session.yaml",
	"session.json",
}

// cwdKeys lists candidate key names Copilot CLI has used for a session's
// working directory across versions.
var cwdKeys = []string{"cwd", "workingDirectory", "working_dir", "directory", "workspace"}

// idKeys lists candidate key names for the session identifier.
var idKeys = []string{"id", "sessionId", "session_id", "sessionID"}

// gitRootKeys lists candidate key names for the session's git root.
var gitRootKeys = []string{"git_root", "gitRoot"}

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
// It tries several known metadata filenames and tolerates alternate field
// names for the working directory, since Copilot CLI's on-disk format has
// drifted across versions.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error

	for _, name := range workspaceMetaFilenames {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}

		var raw map[string]interface{}
		if strings.HasSuffix(name, ".json") {
			err = json.Unmarshal(data, &raw)
		} else {
			err = yaml.Unmarshal(data, &raw)
		}
		if err != nil {
			lastErr = err
			continue
		}

		meta := sessionMeta{
			ID:      firstStringField(raw, idKeys),
			CWD:     firstStringField(raw, cwdKeys),
			GitRoot: firstStringField(raw, gitRootKeys),
		}

		return &meta, nil
	}

	return nil, lastErr
}

// firstStringField returns the value of the first key in keys present in m
// as a non-empty string, or "" if none match.
func firstStringField(m map[string]interface{}, keys []string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
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
