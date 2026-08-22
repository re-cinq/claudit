package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session's
// workspace metadata file. Field names identifying the working directory
// have shifted across Copilot CLI releases (e.g. "cwd" vs "directory" /
// "workingDirectory"), so several aliases are accepted and reconciled via
// resolvedCWD.
type sessionMeta struct {
	ID  string `yaml:"id"`
	CWD string `yaml:"cwd"`

	// Alternate spellings seen across Copilot CLI versions.
	Directory        string `yaml:"directory,omitempty"`
	WorkingDirectory string `yaml:"workingDirectory,omitempty"`
	Path             string `yaml:"path,omitempty"`

	GitRoot string `yaml:"git_root,omitempty"`
}

// resolvedCWD returns the working directory from whichever field is populated.
func (m *sessionMeta) resolvedCWD() string {
	for _, v := range []string{m.CWD, m.Directory, m.WorkingDirectory, m.Path} {
		if v != "" {
			return v
		}
	}
	return ""
}

// metaFileCandidates are the workspace metadata filenames tried, in order,
// when reading a Copilot session directory. Copilot CLI has renamed and
// reformatted this sidecar file at least once across releases.
var metaFileCandidates = []string{"workspace.yaml", "workspace.json", "session.yaml", "metadata.yaml"}

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

// parseSessionMeta reads workspace metadata from a Copilot session directory.
// Copilot CLI has renamed/reformatted this sidecar file across releases, so
// several candidate filenames are tried in order; yaml.Unmarshal also
// accepts well-formed JSON, so a single parse path covers both encodings.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	var lastErr error
	for _, name := range metaFileCandidates {
		path := filepath.Join(sessionDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}

		var meta sessionMeta
		if err := yaml.Unmarshal(data, &meta); err != nil {
			lastErr = err
			continue
		}

		return &meta, nil
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
