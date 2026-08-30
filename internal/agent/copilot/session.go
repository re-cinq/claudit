```go
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

// metadataFilenames lists candidate filenames for Copilot's session metadata
// file, in order of preference. Copilot CLI versions have used different
// names and formats for this file over time.
var metadataFilenames = []string{
	"workspace.yaml",
	"workspace.yml",
	"session.yaml",
	"session.yml",
	"metadata.yaml",
	"metadata.json",
	"session.json",
}

// transcriptFilenames lists candidate filenames for Copilot's transcript
// file, in order of preference.
var transcriptFilenames = []string{
	"events.jsonl",
	"transcript.jsonl",
	"session.jsonl",
	"history.jsonl",
	"log.jsonl",
}

// metaFieldCandidates maps sessionMeta fields to the set of keys that have
// been observed (or are plausible) across Copilot CLI versions for that
// piece of metadata.
var (
	idFieldCandidates      = []string{"id", "sessionId", "sessionID", "session_id"}
	cwdFieldCandidates     = []string{"cwd", "workingDirectory", "working_directory", "working_dir", "directory", "dir", "path", "projectPath", "project_path", "root"}
	gitRootFieldCandidates = []string{"git_root", "gitRoot", "gitroot"}
)

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

// parseSessionMeta reads a session metadata file from a Copilot session
// directory. It tries several known filenames and tolerates field name
// variations that have appeared across Copilot CLI versions.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var raw map[string]interface{}
	found := false

	for _, name := range metadataFilenames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			continue
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			continue
		}
		found = true
		break
	}

	if !found {
		return nil, fmt.Errorf("no session metadata file found in %s", sessionDir)
	}

	return &sessionMeta{
		ID:      stringFromKeys(raw, idFieldCandidates...),
		CWD:     stringFromKeys(raw, cwdFieldCandidates...),
		GitRoot: stringFromKeys(raw, gitRootFieldCandidates...),
	}, nil
}

// stringFromKeys returns the first non-empty string value found among the
// given keys in m.
func stringFromKeys(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// GetTranscriptPath returns the canonical path to the events.jsonl transcript
// within a session directory. This is the name shiftlog itself writes when
// restoring a session; use resolveTranscriptPath to locate a transcript file
// that may have been written by the Copilot CLI under a different name.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// resolveTranscriptPath finds the transcript file within a session directory.
// It tries known candidate filenames first, then falls back to any .jsonl
// file present, and finally to the canonical events.jsonl path (even if it
// doesn't exist) so callers get a sensible default.
func resolveTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFilenames {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	if entries, err := os.ReadDir(sessionDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
				return filepath.Join(sessionDir, entry.Name())
			}
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
```
