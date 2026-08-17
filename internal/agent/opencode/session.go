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

// scanSessionInfo is the subset of fields we look for when scanning arbitrary
// JSON files under the data directory for a session record.
type scanSessionInfo struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	Cwd       string `json:"cwd"`
}

// samePath reports whether two filesystem paths refer to the same location,
// tolerating trailing slashes and symlink differences.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// FindRecentSessionFile scans the OpenCode data directory for a session
// record belonging to the given project, without assuming a fixed on-disk
// layout. OpenCode is published without a version constraint and its storage
// format has changed across releases (flat per-project directories, a shared
// SQLite database, etc.), so directory-listing a single hardcoded path is
// not reliable. This walks dataDir/storage (or dataDir, if "storage" isn't
// present) looking for JSON files that look like session records — anything
// with an "id" field and a projectID/directory/cwd field matching this
// project — and returns the most recently modified match within
// agent.RecentSessionTimeout.
func FindRecentSessionFile(dataDir, projectID, projectPath string) (sessionID string, modTime time.Time, found bool) {
	root := filepath.Join(dataDir, "storage")
	if _, err := os.Stat(root); err != nil {
		root = dataDir
	}

	now := time.Now()
	const maxScan = 20000
	scanned := 0

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if scanned >= maxScan {
			return filepath.SkipAll
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		scanned++

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		var rec scanSessionInfo
		if err := json.Unmarshal(data, &rec); err != nil || rec.ID == "" {
			return nil
		}

		matches := (rec.ProjectID != "" && rec.ProjectID == projectID) ||
			(rec.Directory != "" && samePath(rec.Directory, projectPath)) ||
			(rec.Cwd != "" && samePath(rec.Cwd, projectPath))
		if !matches {
			return nil
		}

		if !found || info.ModTime().After(modTime) {
			sessionID = rec.ID
			modTime = info.ModTime()
			found = true
		}
		return nil
	})

	return sessionID, modTime, found
}

// FindMessageSource locates the message storage location for a session
// without assuming a fixed nesting depth. It looks for a directory named
// exactly after the session ID somewhere under dataDir/storage (or dataDir).
// Returns the directory path and true if found.
func FindMessageSource(dataDir, sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}

	root := filepath.Join(dataDir, "storage")
	if _, err := os.Stat(root); err != nil {
		root = dataDir
	}

	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = p
			return filepath.SkipAll
		}
		return nil
	})

	if found == "" {
		return "", false
	}
	return found, true
}
