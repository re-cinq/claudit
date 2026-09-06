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

// cwdFieldCandidates are alternate YAML keys newer Copilot CLI versions may
// use in place of "cwd" to record the working directory a session started from.
var cwdFieldCandidates = []string{"cwd", "cwd_path", "workingDirectory", "working_directory", "workspace", "directory", "path", "root"}

// gitRootFieldCandidates are alternate YAML keys newer Copilot CLI versions
// may use in place of "git_root".
var gitRootFieldCandidates = []string{"git_root", "gitRoot", "repo_root", "repoRoot", "gitDir", "git_dir"}

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
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

	// Newer Copilot CLI releases have renamed workspace.yaml fields before.
	// If the known fields are empty, fall back to scanning the raw document
	// for common alternates rather than failing to match entirely.
	if meta.CWD == "" || meta.GitRoot == "" {
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err == nil {
			if meta.CWD == "" {
				meta.CWD = firstStringField(raw, cwdFieldCandidates)
			}
			if meta.GitRoot == "" {
				meta.GitRoot = firstStringField(raw, gitRootFieldCandidates)
			}
		}
	}

	return &meta, nil
}

// firstStringField returns the first non-empty string value found in raw
// under any of the given keys.
func firstStringField(raw map[string]interface{}, keys []string) string {
	for _, key := range keys {
		if v, ok := raw[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	primary := filepath.Join(sessionDir, "events.jsonl")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}

	// Newer Copilot CLI versions may use a different transcript filename.
	// Fall back to the first .jsonl file found in the session directory.
	if entries, err := os.ReadDir(sessionDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
				return filepath.Join(sessionDir, entry.Name())
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
