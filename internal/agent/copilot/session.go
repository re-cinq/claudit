package copilot

import (
	"encoding/json"
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

// metadataFilenames lists known/possible Copilot session metadata filenames,
// tried in order. Newer CLI releases have renamed this file across versions,
// so we don't assume "workspace.yaml" is the only possibility.
var metadataFilenames = []string{
	"workspace.yaml",
	"workspace.yml",
	"session.yaml",
	"metadata.yaml",
	"metadata.json",
	"state.json",
}

// cwdKeyPaths lists known/possible (possibly nested) key paths Copilot may
// use to record a session's working directory, tried in order to tolerate
// field renames across CLI versions.
var cwdKeyPaths = [][]string{
	{"cwd"},
	{"cwdPath"},
	{"workingDirectory"},
	{"working_directory"},
	{"directory"},
	{"path"},
	{"workspace", "cwd"},
	{"workspace", "path"},
	{"session", "cwd"},
}

// idKeyPaths lists known/possible (possibly nested) key paths for a session's
// identifier, tried in order to tolerate field renames across CLI versions.
var idKeyPaths = [][]string{
	{"id"},
	{"sessionId"},
	{"session_id"},
	{"sessionID"},
	{"workspace", "id"},
	{"session", "id"},
}

// gitRootKeyPaths lists known/possible key paths for a session's git root.
var gitRootKeyPaths = [][]string{
	{"git_root"},
	{"gitRoot"},
	{"workspace", "git_root"},
	{"workspace", "gitRoot"},
}

// lookupNestedString looks up a (possibly nested) string value in a generic
// map decoded from YAML or JSON.
func lookupNestedString(m map[string]interface{}, keys []string) (string, bool) {
	cur := m
	for i, k := range keys {
		v, ok := cur[k]
		if !ok {
			return "", false
		}
		if i == len(keys)-1 {
			s, ok := v.(string)
			return s, ok
		}
		next, ok := v.(map[string]interface{})
		if !ok {
			return "", false
		}
		cur = next
	}
	return "", false
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

// parseSessionMeta reads a Copilot session directory's metadata file,
// tolerating renamed files and renamed/nested fields across CLI versions.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error = os.ErrNotExist

	for _, name := range metadataFilenames {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}

		var raw map[string]interface{}
		if strings.HasSuffix(name, ".json") {
			if err := json.Unmarshal(data, &raw); err != nil {
				lastErr = err
				continue
			}
		} else {
			if err := yaml.Unmarshal(data, &raw); err != nil {
				lastErr = err
				continue
			}
		}

		meta := &sessionMeta{}
		for _, keys := range idKeyPaths {
			if v, ok := lookupNestedString(raw, keys); ok && v != "" {
				meta.ID = v
				break
			}
		}
		for _, keys := range cwdKeyPaths {
			if v, ok := lookupNestedString(raw, keys); ok && v != "" {
				meta.CWD = v
				break
			}
		}
		for _, keys := range gitRootKeyPaths {
			if v, ok := lookupNestedString(raw, keys); ok && v != "" {
				meta.GitRoot = v
				break
			}
		}

		return meta, nil
	}

	return nil, lastErr
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
