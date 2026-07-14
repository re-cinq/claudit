```go
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
//
// This is shiftlog's own best-effort project identifier, used when writing
// restored sessions. OpenCode's internal project identifier is undocumented
// and has changed across releases, so session *discovery* must not assume
// storage is partitioned by this value - see findRecentSession, which
// matches sessions by the "directory" field recorded inside each session
// file instead.
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

// findRecentSession searches the OpenCode storage tree for the most recent
// session belonging to projectPath, returning its session ID.
//
// OpenCode's on-disk session layout - whether sessions are partitioned into
// per-project directories, and how the project identifier is computed - has
// changed between releases. Rather than re-deriving OpenCode's internal
// project identifier (undocumented and version-dependent), this walks the
// whole session storage tree and matches each session file by the
// "directory" field it records for itself, falling back to matching our own
// best-effort GetProjectID value for older layouts that only recorded a
// "projectID".
func findRecentSession(dataDir, projectPath string) (sessionID string, startedAt time.Time, found bool) {
	root := filepath.Join(dataDir, "storage", "session")
	if _, err := os.Stat(root); err != nil {
		return "", time.Time{}, false
	}

	now := time.Now()
	legacyProjectID := GetProjectID(projectPath)
	legacyProjectDir := filepath.Join(root, legacyProjectID)

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Message/part content lives in its own subtree in newer
			// layouts - skip it, it's irrelevant here (and can be large).
			if d.Name() == "message" || d.Name() == "part" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var sess sessionInfo
		if err := json.Unmarshal(data, &sess); err != nil {
			return nil
		}

		var matches bool
		switch {
		case sess.Directory != "":
			matches = agent.PathsEqual(sess.Directory, projectPath)
		case sess.ProjectID != "":
			matches = sess.ProjectID == legacyProjectID
		default:
			// No identifying field in the file itself - fall back to the
			// legacy per-project directory partitioning.
			matches = filepath.Dir(path) == legacyProjectDir
		}
		if !matches {
			return nil
		}

		if sessionID == "" || modTime.After(startedAt) {
			id := sess.ID
			if id == "" {
				id = strings.TrimSuffix(d.Name(), ".json")
			}
			sessionID = id
			startedAt = modTime
			found = true
		}
		return nil
	})

	return sessionID, startedAt, found
}

// findMessageDir locates the message/transcript directory for a session,
// tolerating the different storage layouts OpenCode has used across
// releases (a top-level "message" directory vs. messages nested under
// "session").
func findMessageDir(dataDir, sessionID string) (string, bool) {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
	}
	for _, dir := range candidates {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return dir, true
		}
	}

	// Last resort: search for a directory literally named after the
	// session ID anywhere under the storage tree.
	var foundDir string
	_ = filepath.WalkDir(filepath.Join(dataDir, "storage"), func(path string, d os.DirEntry, err error) error {
		if err != nil || foundDir != "" {
			return nil
		}
		if foundDir != "" {
			return filepath.SkipAll
		}
		if d.IsDir() && d.Name() == sessionID {
			foundDir = path
			return filepath.SkipAll
		}
		return nil
	})
	if foundDir != "" {
		return foundDir, true
	}

	return filepath.Join(dataDir, "storage", "message", sessionID), false
}
```
