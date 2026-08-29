```go
package copilot

import (
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

// sessionStateDirNames lists the directory names Copilot CLI has used for
// session state across versions, newest known format first. Newer Copilot CLI
// releases renamed "session-state" to "history-session-state"; we check both
// so a CLI version bump doesn't silently break session discovery.
var sessionStateDirNames = []string{"history-session-state", "session-state"}

// GetSessionStateDirs returns all known candidate session state directories,
// newest known format first, regardless of whether they currently exist.
func GetSessionStateDirs() ([]string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return nil, err
	}
	dirs := make([]string, len(sessionStateDirNames))
	for i, name := range sessionStateDirNames {
		dirs[i] = filepath.Join(copilotDir, name)
	}
	return dirs, nil
}

// GetSessionStateDir returns a single canonical session state directory,
// preferring whichever known candidate already exists on disk. Used when a
// single write location is needed (e.g. RestoreSession).
func GetSessionStateDir() (string, error) {
	dirs, err := GetSessionStateDirs()
	if err != nil {
		return "", err
	}
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return dirs[0], nil
}

// sessionMetaFilenames lists the filenames Copilot CLI has used for
// per-session metadata across versions.
var sessionMetaFilenames = []string{"workspace.yaml", "session.yaml", "workspace.json", "session.json"}

// sessionIDKeys and sessionCWDKeys list the metadata field names Copilot CLI
// has used to record a session's ID and working directory across versions.
var (
	sessionIDKeys  = []string{"id", "sessionId", "session_id", "sessionID"}
	sessionCWDKeys = []string{"cwd", "workingDirectory", "working_directory", "directory", "path"}
)

// parseSessionMeta reads per-session metadata from a Copilot session
// directory, tolerating file name and field name drift across Copilot CLI
// versions.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error = os.ErrNotExist

	for _, name := range sessionMetaFilenames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			lastErr = err
			continue
		}

		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			lastErr = err
			continue
		}

		id := firstStringField(raw, sessionIDKeys...)
		cwd := firstStringField(raw, sessionCWDKeys...)
		if id == "" && cwd == "" {
			continue
		}
		if id == "" {
			// Session directories are conventionally named after the session ID.
			id = filepath.Base(sessionDir)
		}

		return &sessionMeta{
			ID:      id,
			CWD:     cwd,
			GitRoot: firstStringField(raw, "git_root", "gitRoot", "gitRootPath"),
		}, nil
	}

	return nil, lastErr
}

// firstStringField returns the first non-empty string found under any of the
// given keys, checking top-level fields and then one level deep under a
// nested "workspace" object.
func firstStringField(raw map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := raw[k].(string); ok && v != "" {
			return v
		}
	}
	if nested, ok := raw["workspace"].(map[string]interface{}); ok {
		for _, k := range keys {
			if v, ok := nested[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// transcriptFilenames lists the filenames Copilot CLI has used for the
// per-session event transcript across versions.
var transcriptFilenames = []string{"events.jsonl", "transcript.jsonl", "session.jsonl"}

// GetTranscriptPath returns the path to the event transcript within a session
// directory, preferring whichever known filename actually exists.
func GetTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFilenames {
		p := filepath.Join(sessionDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(sessionDir, transcriptFilenames[0])
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
	eventsPath := filepath.Join(sessionDir, transcriptFilenames[0])
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}
```
