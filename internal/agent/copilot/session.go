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

// metaFilenames lists the workspace metadata filenames to try, newest-known first.
// Copilot CLI has renamed/reformatted this file across releases (e.g. YAML -> JSON),
// so we probe several candidates instead of assuming one fixed name.
var metaFilenames = []string{
	"workspace.yaml",
	"workspace.json",
	"session.yaml",
	"session.json",
	"metadata.yaml",
	"metadata.json",
}

// cwdKeys are known aliases for the session's working directory field across
// Copilot CLI versions.
var cwdKeys = []string{
	"cwd", "cwdPath", "workingDirectory", "working_directory",
	"directory", "workspaceRoot", "workspace_root", "root", "path",
}

// idKeys are known aliases for the session ID field across Copilot CLI versions.
var idKeys = []string{"id", "sessionId", "session_id", "sessionID"}

// gitRootKeys are known aliases for the git root field across Copilot CLI versions.
var gitRootKeys = []string{"git_root", "gitRoot", "root"}

// wrapperKeys are top-level keys under which metadata may be nested in newer
// Copilot CLI formats (rather than at the document root).
var wrapperKeys = []string{"workspace", "meta", "info", "session"}

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

// firstStringValue returns the first non-empty string value found in m for any
// of the given keys.
func firstStringValue(m map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// extractSessionMeta pulls id/cwd/git_root out of a raw metadata document,
// trying known key aliases at the top level and one level of nesting under
// common wrapper keys (some Copilot CLI versions nest workspace info).
func extractSessionMeta(raw map[string]interface{}, sessionDir string) *sessionMeta {
	meta := &sessionMeta{
		ID:      firstStringValue(raw, idKeys),
		CWD:     firstStringValue(raw, cwdKeys),
		GitRoot: firstStringValue(raw, gitRootKeys),
	}

	for _, wrapper := range wrapperKeys {
		nested, ok := raw[wrapper].(map[string]interface{})
		if !ok {
			continue
		}
		if meta.CWD == "" {
			meta.CWD = firstStringValue(nested, cwdKeys)
		}
		if meta.ID == "" {
			meta.ID = firstStringValue(nested, idKeys)
		}
		if meta.GitRoot == "" {
			meta.GitRoot = firstStringValue(nested, gitRootKeys)
		}
	}

	if meta.ID == "" {
		// Session directories are conventionally named after the session ID;
		// fall back to that if no explicit ID field is present.
		meta.ID = filepath.Base(sessionDir)
	}

	return meta
}

// parseSessionMeta reads workspace metadata from a Copilot session directory.
// It tries several known filenames and, since JSON is valid YAML, parses both
// formats with the same YAML decoder before falling back to raw key lookup
// to tolerate field renames across Copilot CLI versions.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error

	for _, name := range metaFilenames {
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

		return extractSessionMeta(raw, sessionDir), nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no session metadata file found in %s", sessionDir)
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
