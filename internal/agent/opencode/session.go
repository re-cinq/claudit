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

// discoverFromDataDirScan scans the whole OpenCode data directory for the most
// recently modified session file belonging to projectPath.
//
// Newer OpenCode releases have changed their on-disk storage layout multiple
// times (e.g. dropping the project-scoped session subdirectory, or nesting
// session metadata under storage/session/info instead of storage/session/<id>).
// Rather than hard-coding one exact shape, this walks storage/ looking at every
// *.json file's mtime and, when present, a directory/path/cwd/worktree field to
// match it to the current project. If nothing declares its project explicitly,
// it falls back to the single most recently modified session-like file overall,
// which is still correct in the common case of one active OpenCode session.
func discoverFromDataDirScan(dataDir, projectPath string) (*agent.SessionInfo, error) {
	storageDir := filepath.Join(dataDir, "storage")
	if _, err := os.Stat(storageDir); err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestPath string
	var bestID string
	var bestModTime time.Time
	var bestData []byte
	var haveProjectMatch bool

	_ = filepath.WalkDir(storageDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
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

		var meta struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
			Path      string `json:"path"`
			Cwd       string `json:"cwd"`
			Worktree  string `json:"worktree"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil
		}

		id := meta.ID
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}

		dir := firstNonEmpty(meta.Directory, meta.Path, meta.Cwd, meta.Worktree)
		matchesProject := dir != "" && agent.PathsEqual(dir, projectPath)

		if matchesProject {
			if !haveProjectMatch || modTime.After(bestModTime) {
				bestPath, bestID, bestModTime, bestData = path, id, modTime, data
				haveProjectMatch = true
			}
			return nil
		}

		if !haveProjectMatch && (bestPath == "" || modTime.After(bestModTime)) {
			bestPath, bestID, bestModTime, bestData = path, id, modTime, data
		}

		return nil
	})

	if bestID == "" {
		return nil, nil
	}

	info := &agent.SessionInfo{
		SessionID:   bestID,
		StartedAt:   bestModTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	if msgDir := findMessageSource(dataDir, bestPath, bestID); msgDir != "" {
		info.TranscriptPath = msgDir
	} else {
		// No standalone message store found for this session; fall back to
		// the session file's own content so store still has something to
		// parse rather than failing outright.
		info.TranscriptData = bestData
	}

	return info, nil
}

// findMessageSource tries several message-storage conventions used across
// OpenCode releases to locate the message data for a given session.
func findMessageSource(dataDir, sessionInfoPath, sessionID string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
		filepath.Join(filepath.Dir(sessionInfoPath), "message", sessionID),
		filepath.Join(filepath.Dir(filepath.Dir(sessionInfoPath)), "message", sessionID),
		filepath.Join(filepath.Dir(sessionInfoPath), sessionID),
	}

	for _, c := range candidates {
		info, err := os.Stat(c)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			return c
		}
		if entries, err := os.ReadDir(c); err == nil && len(entries) > 0 {
			return c
		}
	}
	return ""
}

// firstNonEmpty returns the first non-empty string among values.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
```
