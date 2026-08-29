package copilot

import (
	"fmt"
	"io/fs"
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

// candidateMetaFilenames are the filenames Copilot CLI has been observed or
// suspected to use for per-session workspace metadata. The exact format was
// never validated against a real Copilot CLI session (see workspace-ptf), so
// we check multiple plausible candidates rather than assuming one.
var candidateMetaFilenames = []string{"workspace.yaml", "workspace.yml", "session.yaml", "metadata.yaml"}

// candidateTranscriptFilenames are the filenames Copilot CLI has been
// observed or suspected to use for the session transcript.
var candidateTranscriptFilenames = []string{"events.jsonl", "history.jsonl", "transcript.jsonl", "session.jsonl"}

// cwdKeys are the possible key names for the session's working directory
// across Copilot CLI versions/formats.
var cwdKeys = []string{"cwd", "cwd_path", "working_dir", "workingdirectory", "directory", "root", "workspace_root", "workspaceroot"}

// idKeys are the possible key names for the session ID across Copilot CLI
// versions/formats.
var idKeys = []string{"id", "session_id", "sessionid"}

// parseSessionMeta reads a workspace.yaml from a Copilot session directory
// using the strict, known-good field names.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var meta sessionMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

// parseFlexibleSessionMeta reads a session metadata file, first trying the
// strict known-good format, then falling back to scanning for alternate key
// names (including one level of nesting) if that yields nothing useful.
func parseFlexibleSessionMeta(path string) *sessionMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var meta sessionMeta
	_ = yaml.Unmarshal(data, &meta)
	if meta.CWD != "" {
		return &meta
	}

	var generic map[string]interface{}
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return nil
	}

	cwd := lookupFlexibleString(generic, cwdKeys)
	if cwd == "" {
		return nil
	}
	id := lookupFlexibleString(generic, idKeys)
	if id == "" {
		id = meta.ID
	}

	return &sessionMeta{ID: id, CWD: cwd}
}

// lookupFlexibleString looks for any of the given keys (case-insensitive) at
// the top level of m, or one level of nesting inside any nested map value.
func lookupFlexibleString(m map[string]interface{}, keys []string) string {
	if v := lookupStringByKeys(m, keys); v != "" {
		return v
	}
	for _, v := range m {
		if nested, ok := v.(map[string]interface{}); ok {
			if s := lookupStringByKeys(nested, keys); s != "" {
				return s
			}
		}
	}
	return ""
}

func lookupStringByKeys(m map[string]interface{}, keys []string) string {
	for k, v := range m {
		lk := strings.ToLower(k)
		for _, candidate := range keys {
			if lk == candidate {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// findSessionMetaFiles walks the whole session-state tree looking for any
// known metadata filename, at any depth. Older code assumed a fixed
// <sessionDir>/<sessionID>/workspace.yaml layout one level deep; this
// tolerates Copilot CLI adding or removing nesting levels across versions.
func findSessionMetaFiles(root string) []string {
	var found []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		for _, candidate := range candidateMetaFilenames {
			if name == candidate {
				found = append(found, path)
				return nil
			}
		}
		return nil
	})
	return found
}

// findTranscriptFile locates the transcript file within a session directory,
// trying known candidate filenames before falling back to the historical
// default (events.jsonl).
func findTranscriptFile(sessionDir string) string {
	for _, candidate := range candidateTranscriptFilenames {
		path := filepath.Join(sessionDir, candidate)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return GetTranscriptPath(sessionDir)
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
