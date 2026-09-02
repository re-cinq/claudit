package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace file.
type sessionMeta struct {
	ID  string
	CWD string
}

// sessionMetaFilenames lists the filenames Copilot CLI has used across
// versions to store per-session workspace metadata (YAML and JSON variants).
var sessionMetaFilenames = []string{
	"workspace.yaml", "workspace.yml", "workspace.json",
	"session.yaml", "session.json",
}

// sessionMetaIDKeys and sessionMetaCWDKeys list the known field names used
// across Copilot CLI versions for the session ID and working directory,
// tried in priority order.
var sessionMetaIDKeys = []string{"id", "sessionId", "session_id"}
var sessionMetaCWDKeys = []string{"cwd", "cwdPath", "workingDirectory", "workspaceFolder", "directory"}

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

// parseSessionMeta reads workspace metadata from a Copilot session directory.
// It probes several known filenames and field names since Copilot CLI has
// changed both across releases.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var data []byte
	var readErr error
	for _, name := range sessionMetaFilenames {
		data, readErr = os.ReadFile(filepath.Join(sessionDir, name))
		if readErr == nil {
			break
		}
	}
	if readErr != nil {
		return nil, readErr
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	meta := &sessionMeta{}
	for _, key := range sessionMetaIDKeys {
		if v, ok := raw[key].(string); ok && v != "" {
			meta.ID = v
			break
		}
	}
	for _, key := range sessionMetaCWDKeys {
		if v, ok := raw[key].(string); ok && v != "" {
			meta.CWD = v
			break
		}
	}

	return meta, nil
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
	yamlData, err := yaml.Marshal(map[string]string{"id": sessionID})
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
