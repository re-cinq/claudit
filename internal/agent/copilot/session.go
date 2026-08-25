package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
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

// cachedActiveSession is a shiftlog-managed breadcrumb of the most recently
// observed Copilot CLI session, refreshed on every postToolUse hook call.
//
// Copilot's own session-state directory (scanned by scanForRecentSession) is
// only reliably scannable while the Copilot process is still running. For a
// manual commit (made after the agent has already exited, via the post-commit
// git hook), that directory may no longer exist or no longer be matchable by
// CWD. Caching a snapshot here — including the transcript content itself —
// lets manual-mode discovery succeed regardless of what Copilot does with its
// own session storage after the process exits.
type cachedActiveSession struct {
	SessionID      string `json:"session_id"`
	TranscriptData string `json:"transcript_data,omitempty"`
	ProjectPath    string `json:"project_path"`
	CapturedAt     string `json:"captured_at"`
}

func activeSessionCachePath(projectPath string) string {
	return filepath.Join(projectPath, ".shiftlog", "copilot-active-session.json")
}

// cacheDiscoveredSession snapshots the given session's current transcript
// content into the shiftlog-owned breadcrumb file. Best-effort: errors are
// swallowed since this is a fallback mechanism, not the primary path.
func cacheDiscoveredSession(projectPath, sessionID, transcriptPath string) {
	if projectPath == "" || sessionID == "" || transcriptPath == "" {
		return
	}

	transcriptData, err := os.ReadFile(transcriptPath)
	if err != nil || len(transcriptData) == 0 {
		return
	}

	cached := cachedActiveSession{
		SessionID:      sessionID,
		TranscriptData: string(transcriptData),
		ProjectPath:    projectPath,
		CapturedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(&cached)
	if err != nil {
		return
	}

	cacheDir := filepath.Join(projectPath, ".shiftlog")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return
	}
	_ = os.WriteFile(activeSessionCachePath(projectPath), data, 0600)
}

// readCachedSession reads the shiftlog-owned session breadcrumb for
// projectPath, if any recent one exists.
func readCachedSession(projectPath string) *agent.SessionInfo {
	data, err := os.ReadFile(activeSessionCachePath(projectPath))
	if err != nil {
		return nil
	}

	var cached cachedActiveSession
	if err := json.Unmarshal(data, &cached); err != nil || cached.SessionID == "" {
		return nil
	}

	if !agent.PathsEqual(cached.ProjectPath, projectPath) {
		return nil
	}

	capturedAt, err := time.Parse(time.RFC3339, cached.CapturedAt)
	if err != nil || time.Since(capturedAt) > agent.RecentSessionTimeout {
		return nil
	}

	info := &agent.SessionInfo{
		SessionID:   cached.SessionID,
		StartedAt:   cached.CapturedAt,
		ProjectPath: projectPath,
	}
	if cached.TranscriptData != "" {
		info.TranscriptData = []byte(cached.TranscriptData)
	}
	return info
}
