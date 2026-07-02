package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/re-cinq/shift-log/internal/util"
)

// ActiveSession represents the currently active coding agent session
type ActiveSession struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	StartedAt      string `json:"started_at"`
	ProjectPath    string `json:"project_path"`
}

const (
	activeSessionFile   = "active-session.json"
	staleSessionTimeout = 10 * time.Minute
)

// WriteActiveSession writes the active session state to .shiftlog/active-session.json
func WriteActiveSession(session *ActiveSession) error {
	sessionPath, err := getActiveSessionPath()
	if err != nil {
		return err
	}

	// Ensure .shiftlog directory exists
	dir := filepath.Dir(sessionPath)
	if err := util.EnsureDir(dir); err != nil {
		return fmt.Errorf("failed to create .shiftlog directory: %w", err)
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}

	return os.WriteFile(sessionPath, data, 0644)
}

// ReadActiveSession reads the active session state from .shiftlog/active-session.json
// Returns nil if no active session file exists
func ReadActiveSession() (*ActiveSession, error) {
	sessionPath, err := getActiveSessionPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read session file: %w", err)
	}

	var session ActiveSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to parse session file: %w", err)
	}

	return &session, nil
}

// ClearActiveSession removes the active session state file
func ClearActiveSession() error {
	sessionPath, err := getActiveSessionPath()
	if err != nil {
		return err
	}

	err = os.Remove(sessionPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove session file: %w", err)
	}

	return nil
}

// IsSessionActive checks if the session is still active by validating transcript mtime
// A session is considered inactive if the transcript hasn't been modified in 10+ minutes
func IsSessionActive(session *ActiveSession) bool {
	if session == nil || session.TranscriptPath == "" {
		return false
	}

	info, err := os.Stat(session.TranscriptPath)
	if err != nil {
		return false
	}

	// Check if transcript was modified within the stale timeout
	return time.Since(info.ModTime()) < staleSessionTimeout
}

// getActiveSessionPath returns the path to .shiftlog/active-session.json
func getActiveSessionPath() (string, error) {
	root, err := util.GetProjectRoot()
	if err != nil {
		return "", fmt.Errorf("failed to get project root: %w", err)
	}
	return filepath.Join(root, ".shiftlog", activeSessionFile), nil
}
