```go
package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session's
// on-disk metadata file.
type sessionMeta struct {
	ID      string `yaml:"id"`
	CWD     string `yaml:"cwd"`
	GitRoot string `yaml:"git_root,omitempty"`
}

// metadataFilenames are the known names Copilot CLI has used for a session's
// metadata file across versions, tried in order.
var metadataFilenames = []string{"workspace.yaml", "workspace.yml", "workspace.json", "session.yaml", "session.json"}

// cwdKeys are the known key names Copilot CLI has used to record a session's
// working directory in its metadata file.
var cwdKeys = []string{"cwd", "cwd_path", "cwdpath", "workingdirectory", "working_dir", "workingdir", "directory", "dir"}

// idKeys are the known key names Copilot CLI has used for a session's ID.
var idKeys = []string{"id", "session_id", "sessionid"}

// transcriptFilenames are the known names Copilot CLI has used for a
// session's event transcript file across versions, tried in order.
var transcriptFilenames = []string{"events.jsonl", "events.json", "transcript.jsonl"}

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

// parseSessionMeta reads a session's metadata file from a Copilot session
// directory. It tries several known filenames and, for each, first decodes
// into the known schema and falls back to a case-insensitive generic lookup
// so that key renames across Copilot CLI versions don't break discovery.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error

	for _, name := range metadataFilenames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			lastErr = err
			continue
		}

		var meta sessionMeta
		if err := yaml.Unmarshal(data, &meta); err == nil && (meta.ID != "" || meta.CWD != "") {
			return &meta, nil
		}

		// The known schema didn't decode cleanly (or produced an empty
		// result) - fall back to a generic decode in case field names moved.
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			lastErr = err
			continue
		}

		return &sessionMeta{
			ID:  lookupStringCI(raw, idKeys),
			CWD: lookupStringCI(raw, cwdKeys),
		}, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no session metadata file found in %s", sessionDir)
	}
	return nil, lastErr
}

// lookupStringCI returns the string value of the first key in candidates
// found in m, matching key names case-insensitively.
func lookupStringCI(m map[string]interface{}, candidates []string) string {
	for k, v := range m {
		lk := strings.ToLower(k)
		for _, c := range candidates {
			if lk == c {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// findTranscriptFile locates a session's transcript file within its
// directory, tolerating filename changes across Copilot CLI versions.
func findTranscriptFile(sessionDir string) string {
	for _, name := range transcriptFilenames {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			return filepath.Join(sessionDir, entry.Name())
		}
	}
	return ""
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
