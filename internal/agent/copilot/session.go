package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
// Some Copilot CLI versions nest fields under a "workspace" key, or use a
// "folder" field instead of "cwd", so both flat and nested locations are
// checked when resolving the effective values.
type sessionMeta struct {
	ID        string `yaml:"id"`
	CWD       string `yaml:"cwd"`
	GitRoot   string `yaml:"git_root,omitempty"`
	Workspace *struct {
		CWD     string `yaml:"cwd"`
		GitRoot string `yaml:"git_root,omitempty"`
		Folder  string `yaml:"folder,omitempty"`
	} `yaml:"workspace,omitempty"`
	Folder string `yaml:"folder,omitempty"`
}

// effectiveCWD returns the best-guess working directory recorded for a
// session, checking flat and nested field locations.
func (m *sessionMeta) effectiveCWD() string {
	if m.CWD != "" {
		return m.CWD
	}
	if m.Folder != "" {
		return m.Folder
	}
	if m.Workspace != nil {
		if m.Workspace.CWD != "" {
			return m.Workspace.CWD
		}
		return m.Workspace.Folder
	}
	return ""
}

// effectiveGitRoot returns the best-guess git root recorded for a session.
func (m *sessionMeta) effectiveGitRoot() string {
	if m.GitRoot != "" {
		return m.GitRoot
	}
	if m.Workspace != nil {
		return m.Workspace.GitRoot
	}
	return ""
}

// sessionStateDirNames are directory names, in preference order, that
// different Copilot CLI versions have used to store session state under
// the Copilot config directory.
var sessionStateDirNames = []string{"session-state", "sessions", "session-history"}

// sessionMetaFileNames are metadata filenames, in preference order, that
// different Copilot CLI versions have used within a session directory.
var sessionMetaFileNames = []string{"workspace.yaml", "session.yaml", "metadata.yaml", "workspace.json", "session.json"}

// transcriptFileNames are transcript filenames, in preference order, that
// different Copilot CLI versions have used within a session directory.
var transcriptFileNames = []string{"events.jsonl", "transcript.jsonl", "session.jsonl"}

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
	return filepath.Join(copilotDir, sessionStateDirNames[0]), nil
}

// GetSessionStateDirs returns every candidate session state directory that
// currently exists on disk. Different Copilot CLI versions have used
// different directory names, so callers scanning for sessions should check
// them all. If none exist yet, the primary candidate is returned so callers
// still have a path to work with.
func GetSessionStateDirs() ([]string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return nil, err
	}

	var dirs []string
	for _, name := range sessionStateDirNames {
		p := filepath.Join(copilotDir, name)
		if info, statErr := os.Stat(p); statErr == nil && info.IsDir() {
			dirs = append(dirs, p)
		}
	}
	if len(dirs) == 0 {
		dirs = append(dirs, filepath.Join(copilotDir, sessionStateDirNames[0]))
	}
	return dirs, nil
}

// findSessionMetaFile locates the session metadata file within a session
// directory, trying known filename variants used across Copilot CLI versions.
func findSessionMetaFile(sessionDir string) string {
	for _, name := range sessionMetaFileNames {
		p := filepath.Join(sessionDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// parseSessionMeta reads a session metadata file (YAML or JSON) from a
// Copilot session directory, trying known filename variants.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := findSessionMetaFile(sessionDir)
	if path == "" {
		return nil, fmt.Errorf("no session metadata file found in %s", sessionDir)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var meta sessionMeta
	// yaml.Unmarshal also handles JSON, since JSON is a subset of YAML.
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

// GetTranscriptPath returns the path to the transcript file within a session
// directory, trying known filename variants and falling back to the default
// (events.jsonl) if none are found yet.
func GetTranscriptPath(sessionDir string) string {
	for _, name := range transcriptFileNames {
		p := filepath.Join(sessionDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(sessionDir, transcriptFileNames[0])
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
