package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

// GetDataDir returns the OpenCode data directory.
// OpenCode follows XDG conventions: it uses $XDG_DATA_HOME/opencode on Linux
// and ~/Library/Application Support/opencode on macOS.
func GetDataDir() (string, error) {
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("could not determine home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "opencode"), nil
	}

	// Linux/other: respect XDG_DATA_HOME, default to ~/.local/share
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "opencode"), nil
}

// GetProjectID returns the project identifier for OpenCode.
// For git repos, this is the root commit hash. For non-git dirs, it's "global".
func GetProjectID(projectPath string) string {
	cmd := exec.Command("git", "rev-list", "--max-parents=0", "--all")
	cmd.Dir = projectPath
	output, err := cmd.Output()
	if err != nil {
		return "global"
	}

	// Take the first line (first root commit)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) > 0 && lines[0] != "" {
		return strings.TrimSpace(lines[0])
	}
	return "global"
}

// GetSessionDir returns the session storage directory for a project.
func GetSessionDir(projectPath string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}

	projectID := GetProjectID(projectPath)
	return filepath.Join(dataDir, "storage", "session", projectID), nil
}

// GetMessageDir returns the message storage directory for a session.
func GetMessageDir(sessionID string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dataDir, "storage", "message", sessionID), nil
}

// sessionInfo represents an OpenCode session JSON file.
type sessionInfo struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID,omitempty"`
	Directory string `json:"directory,omitempty"`
	Title     string `json:"title,omitempty"`
}

// WriteSessionFile writes a session and its messages to OpenCode's storage.
func WriteSessionFile(projectPath, sessionID string, transcriptData []byte) (string, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return "", fmt.Errorf("could not create session directory: %w", err)
	}

	sessionPath := filepath.Join(sessionDir, sessionID+".json")

	// Write a minimal session file
	session := sessionInfo{
		ID:        sessionID,
		ProjectID: GetProjectID(projectPath),
		Directory: projectPath,
		Title:     "Restored session",
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return "", fmt.Errorf("could not marshal session: %w", err)
	}

	if err := os.WriteFile(sessionPath, data, 0600); err != nil {
		return "", fmt.Errorf("could not write session file: %w", err)
	}

	// Write messages from transcript data
	msgDir, err := GetMessageDir(sessionID)
	if err != nil {
		return sessionPath, nil // Session created, messages optional
	}

	if err := os.MkdirAll(msgDir, 0700); err != nil {
		return sessionPath, nil
	}

	// Write the raw transcript data as a single message file for restore
	msgPath := filepath.Join(msgDir, "transcript.jsonl")
	_ = os.WriteFile(msgPath, transcriptData, 0600)

	return sessionPath, nil
}

// sessionRecord is a permissive view of an OpenCode session file. Newer
// OpenCode releases have moved sessions out of the project-ID-scoped
// directory that GetSessionDir assumes, storing them in a flat namespace
// keyed only by session ID with the owning project recorded inline (as
// "directory" or "cwd") instead of via directory nesting. This type reads
// just enough of that record to re-associate a session with a project path
// regardless of where OpenCode chose to put the file on disk.
type sessionRecord struct {
	ID        string          `json:"id"`
	Directory string          `json:"directory"`
	Cwd       string          `json:"cwd"`
	Time      json.RawMessage `json:"time"`
}

// projectDir returns whichever project-path field OpenCode populated.
func (s sessionRecord) projectDir() string {
	if s.Directory != "" {
		return s.Directory
	}
	return s.Cwd
}

// updatedAt extracts the most recent timestamp from a session record's
// "time" field. OpenCode has represented this both as unix-millisecond
// numbers and as RFC3339 strings across versions, so both are tried.
func (s sessionRecord) updatedAt() (time.Time, bool) {
	if len(s.Time) == 0 {
		return time.Time{}, false
	}

	var asMillis struct {
		Updated json.Number `json:"updated"`
		Created json.Number `json:"created"`
	}
	if err := json.Unmarshal(s.Time, &asMillis); err == nil {
		if ms, err := asMillis.Updated.Int64(); err == nil && ms > 0 {
			return time.UnixMilli(ms), true
		}
		if ms, err := asMillis.Created.Int64(); err == nil && ms > 0 {
			return time.UnixMilli(ms), true
		}
	}

	var asStrings struct {
		Updated string `json:"updated"`
		Created string `json:"created"`
	}
	if err := json.Unmarshal(s.Time, &asStrings); err == nil {
		if t, err := time.Parse(time.RFC3339, asStrings.Updated); err == nil {
			return t, true
		}
		if t, err := time.Parse(time.RFC3339, asStrings.Created); err == nil {
			return t, true
		}
	}

	return time.Time{}, false
}

// findUnscopedSession searches the entire session storage tree under dataDir
// (not just the project-ID-scoped subdirectory GetSessionDir assumes) for the
// most recent session belonging to projectPath. This tolerates OpenCode
// versions that store all sessions in a flat namespace and identify the
// owning project inline rather than via directory nesting.
func findUnscopedSession(dataDir, projectPath string) (sessionID string, foundAt time.Time, ok bool) {
	root := filepath.Join(dataDir, "storage", "session")
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", time.Time{}, false
	}

	now := time.Now()
	var bestID string
	var bestTime time.Time

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		var rec sessionRecord
		if unmarshalErr := json.Unmarshal(data, &rec); unmarshalErr != nil {
			return nil
		}

		dir := rec.projectDir()
		if dir == "" || !agent.PathsEqual(dir, projectPath) {
			return nil
		}

		id := rec.ID
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}

		modTime, hasTime := rec.updatedAt()
		if !hasTime {
			fi, infoErr := d.Info()
			if infoErr != nil {
				return nil
			}
			modTime = fi.ModTime()
		}

		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		if bestID == "" || modTime.After(bestTime) {
			bestID = id
			bestTime = modTime
		}
		return nil
	})

	if bestID == "" {
		return "", time.Time{}, false
	}
	return bestID, bestTime, true
}

// findMessageDir locates the directory containing a session's messages,
// trying known OpenCode storage layouts since the message path has moved
// between versions (e.g. "storage/message/<id>" vs "storage/session/message/<id>").
func findMessageDir(dataDir, sessionID string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return candidates[0]
}
