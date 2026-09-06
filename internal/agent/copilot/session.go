```go
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
	ID  string `yaml:"id"`
	CWD string `yaml:"cwd"`
}

// metaFileNames are the candidate filenames for a Copilot session's metadata
// file, tried in order. Copilot CLI has used different names/formats for this
// file across versions.
var metaFileNames = []string{
	"workspace.yaml", "workspace.yml", "workspace.json",
	"session.yaml", "session.json", "state.json",
}

// cwdKeyCandidates are the known field names Copilot has used for a session's
// working directory across versions.
var cwdKeyCandidates = []string{
	"cwd", "cwd_path", "cwdPath", "workingDirectory",
	"working_directory", "directory", "project_dir", "projectDir", "root",
}

// idKeyCandidates are the known field names Copilot has used for the session ID.
var idKeyCandidates = []string{"id", "session_id", "sessionId", "sessionID"}

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

// parseSessionMeta reads a Copilot session's metadata file from a session
// directory, trying several known filenames and formats since Copilot CLI has
// changed both across versions.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error = os.ErrNotExist

	for _, name := range metaFileNames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			lastErr = err
			continue
		}

		raw := make(map[string]interface{})
		if strings.HasSuffix(name, ".json") {
			err = json.Unmarshal(data, &raw)
		} else {
			err = yaml.Unmarshal(data, &raw)
		}
		if err != nil {
			lastErr = err
			continue
		}

		meta := &sessionMeta{
			ID:  lookupStringField(raw, idKeyCandidates),
			CWD: lookupStringField(raw, cwdKeyCandidates),
		}
		if meta.ID == "" && meta.CWD == "" {
			lastErr = fmt.Errorf("no recognizable session fields in %s", name)
			continue
		}
		return meta, nil
	}

	return nil, lastErr
}

// lookupStringField looks for a string value under any of the candidate keys,
// checking one level of nested maps too (e.g. {"workspace": {"cwd": "..."}}).
func lookupStringField(raw map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := raw[k].(string); ok && v != "" {
			return v
		}
	}
	for _, v := range raw {
		nested, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		for _, k := range keys {
			if s, ok := nested[k].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// transcriptFileNames are the candidate transcript filenames within a Copilot
// session directory, tried in order.
var transcriptFileNames = []string{
	"events.jsonl", "transcript.jsonl", "session.jsonl", "log.jsonl", "history.jsonl",
}

// GetTranscriptPath returns the path to the transcript file within a session
// directory. It checks known candidate filenames (Copilot CLI has used
// different names across versions) and falls back to events.jsonl, the
// default location used when writing a new session.
func GetTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFileNames {
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
	eventsPath := GetTranscriptPath(sessionDir)
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}
```
