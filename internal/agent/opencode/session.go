```go
package opencode

import (
	"encoding/json"
	"fmt"
	"io/fs"
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
//
// This is shift-log's own convention, used for round-tripping sessions it
// restores (see WriteSessionFile). OpenCode's own project ID scheme has
// changed across releases, so session *discovery* does not depend on this
// matching OpenCode's internal ID — see findSessionByDirectory.
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

// findSessionByDirectory scans the OpenCode data directory for a session
// belonging to projectPath and returns the most recently modified match
// within agent.RecentSessionTimeout, or nil if none is found.
//
// OpenCode's on-disk layout (project ID derivation, directory nesting under
// the data dir) has changed across releases, so rather than reconstructing
// its internal paths this walks every "session" directory found anywhere
// under dataDir and matches sessions by the "directory" field recorded in
// each session's own JSON file. This keeps discovery working both for
// layouts that store sessions flat under storage/session/<id>.json and for
// layouts nested under project/<id>/storage/session/<id>.json.
func findSessionByDirectory(dataDir, projectPath string) *agent.SessionInfo {
	if dataDir == "" {
		return nil
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		return nil
	}

	now := time.Now()
	var best *agent.SessionInfo
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting the walk
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		if !hasPathComponent(path, "session") {
			return nil
		}

		fi, err := d.Info()
		if err != nil || now.Sub(fi.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var s sessionInfo
		if err := json.Unmarshal(data, &s); err != nil || s.Directory == "" {
			return nil
		}

		if !agent.PathsEqual(s.Directory, projectPath) {
			return nil
		}

		if best != nil && !fi.ModTime().After(bestModTime) {
			return nil
		}

		sessionID := s.ID
		if sessionID == "" {
			sessionID = strings.TrimSuffix(d.Name(), ".json")
		}

		best = &agent.SessionInfo{
			SessionID:      sessionID,
			TranscriptPath: messageDirFromSessionFile(path, sessionID),
			StartedAt:      fi.ModTime().Format(time.RFC3339),
			ProjectPath:    projectPath,
		}
		bestModTime = fi.ModTime()
		return nil
	})

	return best
}

// hasPathComponent reports whether path contains component as one of its
// path segments.
func hasPathComponent(path, component string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == component {
			return true
		}
	}
	return false
}

// messageDirFromSessionFile derives the message storage directory for a
// session, given the path to its session JSON file. OpenCode stores
// messages in a "message" directory that is a sibling of the "session"
// directory the session file lives in (e.g. .../storage/session/<id>.json
// -> .../storage/message/<id>).
func messageDirFromSessionFile(sessionFilePath, sessionID string) string {
	dir := filepath.Dir(sessionFilePath)
	parts := strings.Split(filepath.ToSlash(dir), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "session" {
			base := append(append([]string{}, parts[:i]...), "message", sessionID)
			return filepath.Join(base...)
		}
	}
	return filepath.Join(dir, sessionID)
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
```
