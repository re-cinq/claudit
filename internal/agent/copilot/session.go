package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session directory.
type sessionMeta struct {
	ID      string
	CWD     string
	GitRoot string
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

// sessionMetaFileNames are the candidate filenames Copilot CLI has used across
// versions to store per-session metadata within a session-state directory.
// Newer CLI releases have moved away from the original "workspace.yaml".
var sessionMetaFileNames = []string{
	"workspace.yaml",
	"workspace.yml",
	"workspace.json",
	"session.yaml",
	"session.json",
	"state.json",
	"metadata.json",
}

// Candidate key names Copilot CLI has used for session metadata fields across versions.
var (
	metaIDKeys      = []string{"id", "sessionId", "session_id", "sessionID"}
	metaCWDKeys     = []string{"cwd", "cwdPath", "cwd_path", "workingDirectory", "working_directory", "dir", "directory"}
	metaGitRootKeys = []string{"git_root", "gitRoot", "repoRoot", "repo_root"}
)

// parseSessionMeta reads per-session metadata from a Copilot session directory.
// Copilot CLI has used different filenames/schemas for this file across
// versions, so several candidates are tried before giving up.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error = os.ErrNotExist

	for _, name := range sessionMetaFileNames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			if !os.IsNotExist(err) {
				lastErr = err
			}
			continue
		}

		raw, err := decodeMetaFile(name, data)
		if err != nil {
			lastErr = err
			continue
		}

		return &sessionMeta{
			ID:      firstStringValue(raw, metaIDKeys),
			CWD:     firstStringValue(raw, metaCWDKeys),
			GitRoot: firstStringValue(raw, metaGitRootKeys),
		}, nil
	}

	return nil, lastErr
}

// decodeMetaFile decodes a metadata file's contents into a generic map,
// trying JSON (by extension or content sniffing) before falling back to YAML.
func decodeMetaFile(name string, data []byte) (map[string]interface{}, error) {
	raw := map[string]interface{}{}

	looksLikeJSON := strings.HasSuffix(name, ".json") || strings.HasPrefix(strings.TrimSpace(string(data)), "{")
	if looksLikeJSON {
		if err := json.Unmarshal(data, &raw); err == nil {
			return raw, nil
		}
	}

	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// firstStringValue returns the first non-empty string value found in raw for
// any of the given candidate keys.
func firstStringValue(raw map[string]interface{}, keys []string) string {
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
