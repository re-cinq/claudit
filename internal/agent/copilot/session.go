package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace file.
type sessionMeta struct {
	ID      string
	CWD     string
	GitRoot string
}

// sessionMetaFilenames are the known (and plausible) filenames Copilot CLI
// has used across versions to store per-session workspace metadata. We try
// each in turn since the CLI has renamed this file more than once.
var sessionMetaFilenames = []string{
	"workspace.yaml",
	"workspace.yml",
	"workspace.json",
	"session.yaml",
	"session.json",
	"state.json",
	"metadata.json",
}

// Candidate field names for each piece of metadata we need, since Copilot
// CLI has also renamed the fields within the metadata file across versions.
var (
	sessionMetaIDKeys      = []string{"id", "sessionId", "session_id", "sessionID"}
	sessionMetaCWDKeys     = []string{"cwd", "directory", "workingDirectory", "working_directory", "workspaceDir", "workspace_dir", "path"}
	sessionMetaGitRootKeys = []string{"git_root", "gitRoot", "gitRootDir", "repoRoot", "repo_root"}
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

// firstStringField returns the first non-empty string value in raw matching
// one of the candidate keys, trying an exact match first and falling back to
// a case-insensitive match (in case the real field uses different casing).
func firstStringField(raw map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	for _, k := range keys {
		for rawKey, v := range raw {
			if strings.EqualFold(rawKey, k) {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// parseSessionMeta reads workspace metadata from a Copilot session directory.
// Copilot CLI has changed both the metadata filename and its field names
// across versions, so this tries several known variants before giving up.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error
	for _, name := range sessionMetaFilenames {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err != nil {
			lastErr = err
			continue
		}

		var raw map[string]interface{}
		// yaml.Unmarshal also parses JSON (JSON is valid YAML), so this
		// handles both workspace.yaml and *.json style metadata files.
		if err := yaml.Unmarshal(data, &raw); err != nil {
			lastErr = err
			continue
		}

		meta := &sessionMeta{
			ID:      firstStringField(raw, sessionMetaIDKeys),
			CWD:     firstStringField(raw, sessionMetaCWDKeys),
			GitRoot: firstStringField(raw, sessionMetaGitRootKeys),
		}
		if meta.ID == "" && meta.CWD == "" {
			// Didn't recognize this file's shape; keep looking.
			lastErr = fmt.Errorf("unrecognized metadata shape in %s", name)
			continue
		}
		return meta, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no session metadata file found in %s", sessionDir)
	}
	return nil, lastErr
}

// GetTranscriptPath returns the path to the transcript file within a session
// directory. Copilot CLI has used different transcript filenames across
// versions; this returns the first known name that exists, defaulting to
// events.jsonl (used when writing a fresh session for restore).
func GetTranscriptPath(sessionDir string) string {
	candidates := []string{"events.jsonl", "transcript.jsonl", "history.jsonl", "messages.jsonl", "session.jsonl"}
	for _, name := range candidates {
		p := filepath.Join(sessionDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
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
	eventsPath := filepath.Join(sessionDir, "events.jsonl")
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}
