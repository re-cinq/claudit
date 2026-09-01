package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace file.
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

// GetSessionStateDir returns the primary session state directory.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-state"), nil
}

// sessionStateDirNames lists the directory names, in order of preference,
// that Copilot CLI has been observed using (or renaming to) for session
// state across releases. Newer releases may not use "session-state" at all,
// so we scan every candidate that exists rather than hard-coding one.
var sessionStateDirNames = []string{"session-state", "sessions", "history", "history-session-state"}

// sessionStateDirCandidates returns every existing session state directory
// candidate under Copilot's config directory.
func sessionStateDirCandidates() []string {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return nil
	}

	var dirs []string
	for _, name := range sessionStateDirNames {
		path := filepath.Join(copilotDir, name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			dirs = append(dirs, path)
		}
	}
	return dirs
}

// sessionMetaFileNames lists the metadata filenames tried, in order, within
// a Copilot session directory. Older CLI versions used workspace.yaml; the
// file/field names have shifted across releases.
var sessionMetaFileNames = []string{"workspace.yaml", "session.yaml", "metadata.yaml", "workspace.yml"}

// sessionIDKeys / sessionCWDKeys list known aliases for the fields we need
// from session metadata, since Copilot has renamed these keys across
// releases.
var sessionIDKeys = []string{"id", "session_id", "sessionId", "sessionID"}
var sessionCWDKeys = []string{"cwd", "cwd_path", "working_directory", "workingDirectory", "directory", "path"}

// parseSessionMeta reads session metadata from a Copilot session directory,
// trying known metadata filenames and field name aliases.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var raw map[string]interface{}
	var lastErr error

	for _, name := range sessionMetaFileNames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			lastErr = err
			continue
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			lastErr = err
			raw = nil
			continue
		}
		lastErr = nil
		break
	}

	if raw == nil {
		if lastErr == nil {
			lastErr = fmt.Errorf("no session metadata file found in %s", sessionDir)
		}
		return nil, lastErr
	}

	meta := &sessionMeta{}
	for _, key := range sessionIDKeys {
		if v, ok := raw[key].(string); ok && v != "" {
			meta.ID = v
			break
		}
	}
	for _, key := range sessionCWDKeys {
		if v, ok := raw[key].(string); ok && v != "" {
			meta.CWD = v
			break
		}
	}

	return meta, nil
}

// sessionTranscriptFileNames lists the transcript filenames tried, in
// order, within a Copilot session directory.
var sessionTranscriptFileNames = []string{"events.jsonl", "transcript.jsonl", "session.jsonl", "history.jsonl"}

// GetTranscriptPath returns the path to the transcript file within a
// session directory, checking known filenames and falling back to
// events.jsonl if none are found on disk (e.g. when writing a new session).
func GetTranscriptPath(sessionDir string) string {
	for _, name := range sessionTranscriptFileNames {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(sessionDir, sessionTranscriptFileNames[0])
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
