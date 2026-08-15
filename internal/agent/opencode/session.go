```go
package opencode

import (
	"encoding/json"
	"errors"
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
//
// NOTE: this is our own best-effort guess at OpenCode's internal project-scoping
// scheme, which is undocumented and has changed across releases. Prefer
// FindRecentSessionByDirectory (which matches sessions by their own recorded
// "directory" field) over path-joining with this ID wherever possible.
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

// GetSessionDir returns the legacy session storage directory for a project,
// assuming sessions are grouped in a subdirectory named after GetProjectID.
// This is a fallback used only when content-based discovery
// (FindRecentSessionByDirectory) finds nothing.
func GetSessionDir(projectPath string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}

	projectID := GetProjectID(projectPath)
	return filepath.Join(dataDir, "storage", "session", projectID), nil
}

// GetMessageDir returns the legacy message storage directory for a session.
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

// errFoundMessageDir is a sentinel used to stop filepath.Walk early once a
// matching message directory has been found.
var errFoundMessageDir = errors.New("found message dir")

// FindRecentSessionByDirectory walks the OpenCode data directory looking for
// session files, matching by the session's own recorded working-directory
// field rather than a locally-computed project ID.
//
// OpenCode's internal project-scoping/directory-nesting scheme is undocumented
// and has changed across releases (this is what previously broke the
// GetSessionDir-based lookup). Content-based matching mirrors the approach
// already used for Claude, Codex, and Copilot session discovery, so it stays
// correct regardless of how OpenCode nests session files on disk.
func FindRecentSessionByDirectory(dataDir, projectPath string, timeout time.Duration) (sessionID string, modTime time.Time, found bool) {
	now := time.Now()
	sessionRoot := filepath.Join(dataDir, "storage", "session")

	_ = filepath.Walk(sessionRoot, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if now.Sub(info.ModTime()) > timeout {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil
		}

		id, _ := raw["id"].(string)
		if id == "" {
			return nil
		}

		dir := firstStringField(raw, "directory", "cwd", "path", "worktree", "root")
		if dir == "" || !agent.PathsEqual(dir, projectPath) {
			return nil
		}

		if !found || info.ModTime().After(modTime) {
			sessionID = id
			modTime = info.ModTime()
			found = true
		}
		return nil
	})

	return sessionID, modTime, found
}

// FindMessageDirForSession searches the OpenCode data directory for the
// message directory associated with sessionID, regardless of how deeply
// OpenCode nests it (avoids assuming the legacy storage/message/<id> layout).
func FindMessageDirForSession(dataDir, sessionID string) (string, bool) {
	var result string

	err := filepath.Walk(dataDir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || !info.IsDir() || info.Name() != sessionID {
			return nil
		}

		entries, err := os.ReadDir(p)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".jsonl") {
				result = p
				return errFoundMessageDir
			}
		}
		return nil
	})

	if err != nil && !errors.Is(err, errFoundMessageDir) {
		return "", false
	}
	if result == "" {
		return "", false
	}
	return result, true
}

// firstStringField returns the first non-empty string value found in m for
// the given candidate keys.
func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
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
