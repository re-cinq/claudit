package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/re-cinq/shift-log/internal/agent"
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

// cachedSession is a point-in-time snapshot of a live Copilot session,
// written while the session-state directory (and its events.jsonl) still
// exist. Copilot CLI removes its session-state directory once the process
// exits, so a later `shiftlog store --manual` (post-commit hook) call has
// nothing left to discover unless we snapshot it ahead of time.
type cachedSession struct {
	SessionID  string `json:"session_id"`
	Transcript []byte `json:"transcript"`
}

// activeSessionCachePath returns the path to the per-project transcript cache.
func activeSessionCachePath(projectPath string) string {
	return filepath.Join(projectPath, ".shiftlog", "copilot-active-session.json")
}

// cacheActiveSession snapshots a live session's transcript to disk so it
// survives Copilot CLI cleaning up its session-state directory on exit.
// Best-effort: failures are silently ignored since this is a fallback path.
func cacheActiveSession(projectPath, sessionID, transcriptPath string) {
	if projectPath == "" || sessionID == "" || transcriptPath == "" {
		return
	}

	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return
	}

	cachePath := activeSessionCachePath(projectPath)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		return
	}

	out, err := json.Marshal(&cachedSession{SessionID: sessionID, Transcript: data})
	if err != nil {
		return
	}

	_ = os.WriteFile(cachePath, out, 0600)
}

// loadCachedSession reads a previously cached session snapshot, if present and recent.
func loadCachedSession(projectPath string) (*agent.SessionInfo, error) {
	cachePath := activeSessionCachePath(projectPath)

	info, err := os.Stat(cachePath)
	if err != nil {
		return nil, nil
	}
	if time.Since(info.ModTime()) > agent.RecentSessionTimeout {
		return nil, nil
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, nil
	}

	var cache cachedSession
	if err := json.Unmarshal(data, &cache); err != nil || cache.SessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      cache.SessionID,
		StartedAt:      info.ModTime().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: cache.Transcript,
	}, nil
}
