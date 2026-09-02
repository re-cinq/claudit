```go
package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// metadataFileCandidates lists possible session-metadata filenames, tried in
// order. Copilot CLI has renamed/restructured this file across releases, so
// we don't hard-fail if "workspace.yaml" specifically isn't present.
var metadataFileCandidates = []string{
	"workspace.yaml", "workspace.yml", "workspace.json",
	"session.yaml", "session.json",
	"state.yaml", "state.json",
	"meta.yaml", "meta.json",
}

// transcriptFileCandidates lists possible transcript filenames, tried in order.
var transcriptFileCandidates = []string{
	"events.jsonl", "history.jsonl", "transcript.jsonl", "session.jsonl",
}

// parseSessionMetaFlexible reads session metadata from a session directory,
// tolerating differently-named metadata files and differently-named fields
// across Copilot CLI versions. ok is false if no metadata file could be
// found or parsed, in which case callers should fall back to recency-only
// matching rather than treating the session as unusable.
func parseSessionMetaFlexible(sessionDir string) (meta *sessionMeta, ok bool) {
	for _, name := range metadataFileCandidates {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			continue
		}

		var raw map[string]interface{}
		if strings.HasSuffix(name, ".json") {
			err = json.Unmarshal(data, &raw)
		} else {
			err = yaml.Unmarshal(data, &raw)
		}
		if err != nil || raw == nil {
			continue
		}

		return &sessionMeta{
			ID:  lookupString(raw, "id", "sessionId", "sessionID", "session_id"),
			CWD: lookupString(raw, "cwd", "workingDirectory", "workingDir", "directory", "path", "projectPath", "project_path"),
		}, true
	}
	return nil, false
}

// lookupString searches a decoded metadata map for the first matching key
// (case-insensitive), including inside a nested "workspace"/"session"/"meta"
// object, and returns its string value.
func lookupString(m map[string]interface{}, keys ...string) string {
	if v := lookupStringFlat(m, keys...); v != "" {
		return v
	}
	for _, nestKey := range []string{"workspace", "session", "meta"} {
		if nested, ok := m[nestKey].(map[string]interface{}); ok {
			if v := lookupStringFlat(nested, keys...); v != "" {
				return v
			}
		}
	}
	return ""
}

func lookupStringFlat(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		for k, v := range m {
			if !strings.EqualFold(k, key) {
				continue
			}
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// findTranscriptFile locates the transcript file within a session directory,
// tolerating filename changes across Copilot CLI versions. Falls back to the
// most recently modified *.jsonl file if none of the known names are present.
func findTranscriptFile(sessionDir string) string {
	for _, name := range transcriptFileCandidates {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return ""
	}

	var best string
	var bestModTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestModTime) {
			best = filepath.Join(sessionDir, entry.Name())
			bestModTime = info.ModTime()
		}
	}
	return best
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
```
