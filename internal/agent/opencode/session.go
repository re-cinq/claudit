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

// sessionCandidate captures the fields shiftlog looks for when scanning
// OpenCode's data directory for session metadata. OpenCode has used
// different field names and directory layouts across versions (e.g. moving
// from per-project session folders to a flat store keyed by session ID with
// an internal "directory"/"projectID" field), so several possible field
// names are probed rather than assuming one fixed shape.
type sessionCandidate struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Directory string `json:"directory"`
	Worktree  string `json:"worktree"`
	CWD       string `json:"cwd"`
	Path      string `json:"path"`
}

// DiscoverSessionByScanning walks the OpenCode data directory looking for a
// session file whose recorded project directory matches projectPath, most
// recently modified within agent.RecentSessionTimeout. It does not assume a
// fixed directory depth or project-ID scheme, since OpenCode's on-disk
// storage layout has changed across versions and a hard-coded path (e.g.
// "storage/session/<projectID>") can silently stop matching real sessions.
func DiscoverSessionByScanning(dataDir, projectPath string) (*agent.SessionInfo, error) {
	if _, err := os.Stat(dataDir); err != nil {
		return nil, nil
	}

	now := time.Now()
	found := false
	var bestID string
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var candidate sessionCandidate
		if err := json.Unmarshal(data, &candidate); err != nil {
			return nil
		}

		id := candidate.ID
		if id == "" {
			id = candidate.SessionID
		}

		dir := candidate.Directory
		if dir == "" {
			dir = candidate.Worktree
		}
		if dir == "" {
			dir = candidate.CWD
		}
		if dir == "" {
			dir = candidate.Path
		}

		if id == "" || dir == "" || !agent.PathsEqual(dir, projectPath) {
			return nil
		}

		if !found || info.ModTime().After(bestModTime) {
			bestID = id
			bestModTime = info.ModTime()
			found = true
		}
		return nil
	})

	if !found {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		TranscriptPath: findSessionMessageDir(dataDir, bestID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findSessionMessageDir searches dataDir for a directory named after
// sessionID, which OpenCode uses (at whatever nesting depth) to group a
// session's message files. Matching by name rather than a fixed relative
// path keeps discovery working across OpenCode's storage layout changes.
func findSessionMessageDir(dataDir, sessionID string) string {
	if sessionID == "" {
		return ""
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
```
