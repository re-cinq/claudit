package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace file.
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

// sessionMetaFilenames lists the workspace metadata filenames we know Copilot
// CLI has used, tried in order. Different Copilot CLI releases have used
// different filenames/extensions for this file, so we probe for all of them.
var sessionMetaFilenames = []string{
	"workspace.yaml",
	"workspace.yml",
	"workspace.json",
	"session.yaml",
	"session.json",
	"metadata.yaml",
	"metadata.json",
}

// sessionMetaFieldAliases maps our sessionMeta fields to the set of key
// names Copilot CLI has used for them across releases.
var (
	sessionMetaIDKeys      = []string{"id", "session_id", "sessionId"}
	sessionMetaCWDKeys     = []string{"cwd", "workspace", "workspace_path", "workspacePath", "directory", "cwd_path"}
	sessionMetaGitRootKeys = []string{"git_root", "gitRoot"}
)

// parseSessionMeta reads a workspace metadata file from a Copilot session directory.
// It tolerates the metadata file being named/encoded differently across
// Copilot CLI releases (YAML or JSON, various filenames) and the fields
// inside it being renamed, by probing a set of known candidates for each.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var data []byte
	var err error
	for _, name := range sessionMetaFilenames {
		data, err = os.ReadFile(filepath.Join(sessionDir, name))
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}

	// yaml.v3 parses JSON as well (JSON is a valid subset of YAML), so a
	// single decode path covers every filename above.
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	meta := &sessionMeta{
		ID:      firstStringField(raw, sessionMetaIDKeys),
		CWD:     firstStringField(raw, sessionMetaCWDKeys),
		GitRoot: firstStringField(raw, sessionMetaGitRootKeys),
	}

	return meta, nil
}

// firstStringField returns the first non-empty string value found in raw
// under any of the given keys.
func firstStringField(raw map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := raw[k]; ok {
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
