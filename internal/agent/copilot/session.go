package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
type sessionMeta struct {
	ID  string
	CWD string
}

// sessionMetaIDKeys and sessionMetaCWDKeys list the known field names Copilot
// CLI has used across releases for the session ID and working directory in
// workspace.yaml. Parsing generically against this list (instead of a fixed
// struct) keeps discovery working even if a release renames these fields.
var sessionMetaIDKeys = []string{"id", "sessionId", "sessionID", "session_id"}
var sessionMetaCWDKeys = []string{"cwd", "cwdPath", "workingDirectory", "directory", "workspacePath", "path"}
var sessionMetaNestedKeys = []string{"workspace", "session"}

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
// It parses generically rather than into a fixed struct so that discovery
// keeps working if a Copilot CLI release renames or restructures fields.
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

	meta := &sessionMeta{}
	meta.ID = lookupStringField(raw, sessionMetaIDKeys)
	meta.CWD = lookupStringField(raw, sessionMetaCWDKeys)

	if meta.CWD == "" || meta.ID == "" {
		for _, nestedKey := range sessionMetaNestedKeys {
			nested, ok := raw[nestedKey].(map[string]interface{})
			if !ok {
				continue
			}
			if meta.ID == "" {
				meta.ID = lookupStringField(nested, sessionMetaIDKeys)
			}
			if meta.CWD == "" {
				meta.CWD = lookupStringField(nested, sessionMetaCWDKeys)
			}
		}
	}

	return meta, nil
}

// lookupStringField returns the first non-empty string value found in raw
// under any of the given candidate keys.
func lookupStringField(raw map[string]interface{}, keys []string) string {
	for _, key := range keys {
		if v, ok := raw[key].(string); ok && v != "" {
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
