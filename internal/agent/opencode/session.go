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

// sessionTreeRecord captures the fields we care about from an OpenCode
// session info JSON file, regardless of where under storage/session it lives.
type sessionTreeRecord struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	Time      struct {
		Created float64 `json:"created"`
		Updated float64 `json:"updated"`
	} `json:"time"`
}

// recordModTime returns the best-effort timestamp for a session record,
// preferring the record's own "updated"/"created" fields (epoch millis)
// over the file's mtime.
func recordModTime(rec sessionTreeRecord) time.Time {
	ms := rec.Time.Updated
	if ms == 0 {
		ms = rec.Time.Created
	}
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(ms))
}

// findMessageDir locates the directory containing message files for a
// session, trying known OpenCode storage layouts in order. Falls back to
// the legacy path if none of the candidates exist yet.
func findMessageDir(dataDir, sessionID string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", sessionID, "message"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return candidates[0]
}

// discoverFromSessionTree walks the OpenCode storage/session directory
// looking for a session info file belonging to projectPath. Newer OpenCode
// releases have moved session info files around (e.g. under
// storage/session/info/<id>.json rather than storage/session/<projectID>/<id>.json
// used by pre-v1.2 releases), so this scans recursively and matches sessions
// by their recorded "directory"/"projectID" fields instead of assuming a
// fixed directory layout.
func discoverFromSessionTree(dataDir, projectPath string) (*agent.SessionInfo, error) {
	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if info, err := os.Stat(sessionRoot); err != nil || !info.IsDir() {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()

	var bestID string
	var bestModTime time.Time

	_ = filepath.WalkDir(sessionRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		var rec sessionTreeRecord
		if jsonErr := json.Unmarshal(data, &rec); jsonErr != nil || rec.ID == "" {
			return nil
		}

		// Scope to this project using whatever identifying field is present.
		if rec.Directory != "" {
			if !agent.PathsEqual(rec.Directory, projectPath) {
				return nil
			}
		} else if rec.ProjectID != "" {
			if rec.ProjectID != projectID {
				return nil
			}
		}

		modTime := recordModTime(rec)
		if modTime.IsZero() {
			if fi, statErr := d.Info(); statErr == nil {
				modTime = fi.ModTime()
			}
		}

		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		if bestID == "" || modTime.After(bestModTime) {
			bestID = rec.ID
			bestModTime = modTime
		}

		return nil
	})

	if bestID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		TranscriptPath: findMessageDir(dataDir, bestID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}
