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

// sessionMetaFilenames lists the metadata filenames Copilot CLI has used
// across versions to describe a session directory. Newer versions may
// rename this file; older names are still tried as a fallback so shiftlog
// keeps working across upgrades.
var sessionMetaFilenames = []string{"workspace.yaml", "session.yaml", "metadata.yaml", "state.yaml"}

// cwdFieldAliases lists the YAML keys Copilot CLI has used across versions
// to record the working directory a session was started in.
var cwdFieldAliases = []string{"cwd", "cwd_path", "workingDirectory", "working_directory", "directory", "path", "workspace", "workspacePath"}

// idFieldAliases lists the YAML keys used for the session identifier.
var idFieldAliases = []string{"id", "sessionId", "session_id", "sessionID"}

// transcriptFilenames lists the transcript filenames Copilot CLI has used
// across versions.
var transcriptFilenames = []string{"events.jsonl", "transcript.jsonl", "messages.jsonl", "session.jsonl"}

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
// It tries several known metadata filenames, and tolerates field renames
// across Copilot CLI versions by checking a list of known aliases for the
// session ID and working directory fields.
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
	for _, key := range idFieldAliases {
		if v, ok := raw[key].(string); ok && v != "" {
			meta.ID = v
			break
		}
	}
	for _, key := range cwdFieldAliases {
		if v, ok := raw[key].(string); ok && v != "" {
			meta.CWD = v
			break
		}
	}
	if v, ok := raw["git_root"].(string); ok && v != "" {
		meta.GitRoot = v
	} else if v, ok := raw["gitRoot"].(string); ok && v != "" {
		meta.GitRoot = v
	}

	return meta, nil
}

// GetTranscriptPath returns the path to the transcript file within a session
// directory. It tries known filenames across Copilot CLI versions and falls
// back to the canonical "events.jsonl" name if none of them exist yet
// (e.g. when computing a path to write to).
func GetTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFilenames {
		path := filepath.Join(sessionDir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
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
	eventsPath := filepath.Join(sessionDir, "events.jsonl")
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}
