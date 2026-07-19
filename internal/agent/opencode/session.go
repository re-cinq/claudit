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

// sessionMatch is a candidate session found while scanning OpenCode's
// session storage tree.
type sessionMatch struct {
	sessionID string
	modTime   time.Time
	rawData   []byte
}

// findRecentSession walks an OpenCode session storage tree looking for the
// most recent session belonging to projectPath.
//
// OpenCode's on-disk partitioning of sessions by project has changed across
// versions (e.g. the algorithm used to derive a project ID from a worktree),
// so this does not assume any particular directory nesting. It instead reads
// each session file's own "directory" field — which OpenCode records inside
// the session JSON regardless of how the file happens to be nested on disk —
// and compares it against the actual project path. If no session in the tree
// carries a matching (or any) "directory" field, it falls back to the most
// recently modified session file anywhere in the tree within the timeout,
// since that's still very likely to be the right one in practice (each test
// run / real usage session works out of a single project at a time).
func findRecentSession(sessionRoot, projectPath string, timeout time.Duration) *sessionMatch {
	now := time.Now()

	var bestMatched *sessionMatch
	var bestAny *sessionMatch

	_ = filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > timeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var si sessionInfo
		if err := json.Unmarshal(data, &si); err != nil {
			return nil
		}

		id := si.ID
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}

		candidate := &sessionMatch{sessionID: id, modTime: modTime, rawData: data}

		if bestAny == nil || modTime.After(bestAny.modTime) {
			bestAny = candidate
		}

		if si.Directory != "" && agent.PathsEqual(si.Directory, projectPath) {
			if bestMatched == nil || modTime.After(bestMatched.modTime) {
				bestMatched = candidate
			}
		}

		return nil
	})

	if bestMatched != nil {
		return bestMatched
	}
	return bestAny
}

// findMessageDirFor locates the message storage directory for a session.
// It tries OpenCode's conventional layout (storage/message/<sessionID>)
// first, then falls back to a recursive search under the data directory in
// case the storage layout has changed between OpenCode versions.
func findMessageDirFor(dataDir, sessionID string) string {
	if conventional, err := GetMessageDir(sessionID); err == nil {
		if info, err := os.Stat(conventional); err == nil && info.IsDir() {
			return conventional
		}
	}

	storageRoot := filepath.Join(dataDir, "storage")
	var found string
	_ = filepath.WalkDir(storageRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
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
