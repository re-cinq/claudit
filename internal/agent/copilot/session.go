package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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

// sessionMetaFileNames are the metadata file names used by Copilot CLI to
// describe a session directory. Different CLI releases have used different
// names, so all known variants are tried in order.
var sessionMetaFileNames = []string{
	"workspace.yaml",
	"workspace.yml",
	"session.yaml",
	"session.yml",
	"metadata.yaml",
	"metadata.json",
	"session.json",
	"workspace.json",
}

// cwdFieldNames are the known field names Copilot CLI has used to record a
// session's working directory.
var cwdFieldNames = []string{"cwd", "cwdPath", "workingDirectory", "working_dir", "dir", "directory"}

// idFieldNames are the known field names Copilot CLI has used to record a
// session's identifier.
var idFieldNames = []string{"id", "sessionId", "session_id", "sessionID"}

// gitRootFieldNames are the known field names Copilot CLI has used to record
// a session's git repository root.
var gitRootFieldNames = []string{"git_root", "gitRoot", "root", "repo_root"}

// parseSessionMeta reads session metadata from a Copilot session directory.
// It tries several known file names and field names since both have changed
// across Copilot CLI releases.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	for _, name := range sessionMetaFileNames {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		raw := map[string]interface{}{}
		if filepath.Ext(name) == ".json" {
			if err := json.Unmarshal(data, &raw); err != nil {
				continue
			}
		} else if err := yaml.Unmarshal(data, &raw); err != nil {
			continue
		}

		return &sessionMeta{
			ID:      firstStringField(raw, idFieldNames),
			CWD:     firstStringField(raw, cwdFieldNames),
			GitRoot: firstStringField(raw, gitRootFieldNames),
		}, nil
	}

	return nil, os.ErrNotExist
}

// firstStringField returns the first string value found in raw for any of
// the given candidate keys, or "" if none match.
func firstStringField(raw map[string]interface{}, keys []string) string {
	for _, key := range keys {
		if v, ok := raw[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// transcriptFileNames are the known transcript file names used within a
// Copilot session directory.
var transcriptFileNames = []string{"events.jsonl", "session.jsonl", "transcript.jsonl", "messages.jsonl"}

// GetTranscriptPath returns the default path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// ResolveTranscriptPath finds the transcript file actually present within a
// session directory, trying known file names in order. Falls back to the
// default events.jsonl path if none are found on disk.
func ResolveTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFileNames {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return GetTranscriptPath(sessionDir)
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
	meta := struct {
		ID string `yaml:"id"`
	}{ID: sessionID}
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
