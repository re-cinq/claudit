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

// resolveMessageDir returns the directory containing sessionID's messages.
// It tries the conventional storage/message/<sessionID> path first (fast
// path for the common case), and falls back to scanning the data directory
// if that path doesn't exist or is empty. OpenCode has changed its on-disk
// storage layout across releases, so the conventional path is not guaranteed
// to hold the messages even when the session itself was found.
func resolveMessageDir(dataDir, sessionID string) string {
	msgDir := filepath.Join(dataDir, "storage", "message", sessionID)
	if entries, err := os.ReadDir(msgDir); err == nil && len(entries) > 0 {
		return msgDir
	}

	if found := findMessageDirByScanning(dataDir, sessionID); found != "" {
		return found
	}

	return msgDir
}

// findMessageDirByScanning searches the OpenCode data directory for a
// directory holding sessionID's messages. Used as a fallback when the
// conventional storage/message/<sessionID> path doesn't exist, since the
// nesting of "message" storage relative to "session" storage has changed
// across OpenCode releases.
func findMessageDirByScanning(dataDir, sessionID string) string {
	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			entries, rerr := os.ReadDir(path)
			if rerr == nil && len(entries) > 0 {
				found = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

// findSessionByScanning performs a bounded, recency-pruned search under
// dataDir for the most recently modified session file belonging to
// projectPath. Used as a fallback when the conventional
// storage/session/<projectID> path (keyed on the git root commit hash)
// doesn't exist or has no recent sessions — OpenCode has changed how (and
// where) it lays out per-project session directories across releases, so the
// conventional path can no longer be assumed to exist.
func findSessionByScanning(dataDir, projectPath, projectID string) *agent.SessionInfo {
	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path == dataDir {
				return nil
			}
			// Prune directories that haven't been touched recently — no file
			// inside can be newer than its containing directory's mtime was
			// last bumped by a write.
			if info, ierr := d.Info(); ierr == nil && now.Sub(info.ModTime()) > recentTimeout {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			return nil
		}

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		var s sessionInfo
		if jerr := json.Unmarshal(data, &s); jerr != nil || s.ID == "" {
			return nil
		}

		// Match on whichever identifying field is present. Prefer the
		// directory (actual project path) since project ID derivation is
		// what's most likely to have changed across OpenCode releases.
		if s.Directory != "" {
			if s.Directory != projectPath {
				return nil
			}
		} else if s.ProjectID != "" && s.ProjectID != projectID {
			return nil
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = s.ID
			bestModTime = modTime
		}
		return nil
	})

	if bestSessionID == "" {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:   bestSessionID,
		StartedAt:   bestModTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}
}
```
