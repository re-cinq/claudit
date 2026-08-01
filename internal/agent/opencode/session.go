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

// discoverByContentScan performs a resilient, layout-agnostic scan of OpenCode's
// data directory for the most recent session belonging to this project. OpenCode
// has changed its on-disk storage layout across versions (e.g. moving between
// storage/session/<projectID>/<id>.json and other project-scoped arrangements),
// so instead of assuming one fixed directory structure, this walks the whole
// data directory and matches session files by their recorded project path
// rather than by directory location.
func discoverByContentScan(dataDir, projectPath string) (*agent.SessionInfo, error) {
	if _, err := os.Stat(dataDir); err != nil {
		return nil, nil
	}

	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		absProjectPath = projectPath
	}

	now := time.Now()
	var bestSessionPath string
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		// Only consider files that live under a "session" directory component,
		// regardless of how deeply it's nested (project.json, storage.json,
		// message files, etc. are not session records).
		if !strings.Contains(filepath.ToSlash(path), "/session") {
			return nil
		}

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}
		if bestSessionID != "" && !info.ModTime().After(bestModTime) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), absProjectPath) {
			return nil
		}

		id := strings.TrimSuffix(d.Name(), ".json")
		var parsed struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &parsed) == nil && parsed.ID != "" {
			id = parsed.ID
		}

		bestSessionPath = path
		bestSessionID = id
		bestModTime = info.ModTime()
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: findMessageDir(dataDir, bestSessionID, filepath.Dir(bestSessionPath)),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findMessageDir locates the directory containing a session's messages,
// trying several plausible layouts relative to where the session file itself
// was found so that discoverByContentScan works across OpenCode storage
// layout changes without hardcoding a single directory structure.
func findMessageDir(dataDir, sessionID, sessionFileDir string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
	}

	// If the session file lives directly under a "session" directory, its
	// sibling "message" directory is one level up.
	if filepath.Base(sessionFileDir) == "session" {
		candidates = append(candidates, filepath.Join(filepath.Dir(sessionFileDir), "message", sessionID))
	} else {
		// Otherwise assume it's nested one level deeper, e.g.
		// storage/session/<projectID>/<id>.json -> storage/message/<id>/
		candidates = append(candidates, filepath.Join(filepath.Dir(filepath.Dir(sessionFileDir)), "message", sessionID))
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}

	// Fall back to the first candidate even if it doesn't exist yet; the
	// caller will surface a read error rather than silently finding nothing.
	return candidates[0]
}
```
