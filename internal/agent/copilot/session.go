package copilot

import (
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

// GetSessionStateDir returns the legacy (~/.copilot) session state directory.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-state"), nil
}

// CandidateSessionStateDirs returns the possible locations for Copilot's
// session-state directory, in priority order. Newer Copilot CLI releases
// honor XDG_STATE_HOME for session/state data, while older releases always
// used ~/.copilot regardless of XDG env vars. Checking both keeps session
// discovery working across CLI versions.
func CandidateSessionStateDirs() []string {
	var dirs []string
	seen := make(map[string]bool)

	add := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}

	if xdgState := os.Getenv("XDG_STATE_HOME"); xdgState != "" {
		add(filepath.Join(xdgState, "copilot", "session-state"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".local", "state", "copilot", "session-state"))
	}
	if legacy, err := GetSessionStateDir(); err == nil {
		add(legacy)
	}

	return dirs
}

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
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

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// findTranscriptFile locates the transcript file within a Copilot session
// directory. It prefers the standard events.jsonl name but falls back to
// the most recently modified *.jsonl file, in case the transcript filename
// changes across Copilot CLI versions.
func findTranscriptFile(sessionDir string) string {
	standard := GetTranscriptPath(sessionDir)
	if _, err := os.Stat(standard); err == nil {
		return standard
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return standard
	}

	var bestPath string
	var bestModTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestModTime) {
			bestPath = filepath.Join(sessionDir, entry.Name())
			bestModTime = info.ModTime()
		}
	}

	if bestPath != "" {
		return bestPath
	}
	return standard
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
