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

// workingDirKeys are field names (case-insensitive) that plausibly hold a
// session's working directory across Copilot CLI versions. Copilot CLI is an
// unpinned, actively evolving dependency, so the workspace.yaml schema can
// shift field names/nesting between releases.
var workingDirKeys = []string{"cwd", "workingdirectory", "working_directory", "dir", "directory", "workdir", "workspace"}

// findWorkingDirValue recursively searches a generically-decoded YAML
// document for a string value under a plausible working-directory key.
func findWorkingDirValue(node interface{}) string {
	m, ok := node.(map[string]interface{})
	if !ok {
		return ""
	}

	for _, key := range workingDirKeys {
		for k, val := range m {
			if !strings.EqualFold(k, key) {
				continue
			}
			if s, ok := val.(string); ok && s != "" {
				return s
			}
		}
	}

	for _, val := range m {
		if s := findWorkingDirValue(val); s != "" {
			return s
		}
	}

	return ""
}

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

	if meta.CWD == "" {
		// Newer/older Copilot CLI releases may rename or nest the
		// working-directory field. Fall back to a generic scan instead of
		// requiring an exact top-level "cwd" key.
		var generic interface{}
		if err := yaml.Unmarshal(data, &generic); err == nil {
			meta.CWD = findWorkingDirValue(generic)
		}
	}

	return &meta, nil
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
