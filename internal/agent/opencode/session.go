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

// maxStorageScanFiles bounds how many candidate files discoverSessionByScan
// will inspect, so a large/long-lived OpenCode data directory can't make
// session discovery unreasonably slow.
const maxStorageScanFiles = 20000

// staticStorageDirNames lists well-known OpenCode storage folder names that
// are never themselves a session identifier.
var staticStorageDirNames = map[string]bool{
	"storage":  true,
	"session":  true,
	"sessions": true,
	"message":  true,
	"messages": true,
	"info":     true,
	"part":     true,
	"parts":    true,
	"project":  true,
	"projects": true,
	"opencode": true,
	"data":     true,
}

// skipStorageDirNames lists directories that never contain session/message
// data and are safe to skip while scanning, purely as a performance
// optimization.
var skipStorageDirNames = map[string]bool{
	"node_modules": true,
	"cache":        true,
	"log":          true,
	"logs":         true,
	"bin":          true,
	"tmp":          true,
}

// discoverSessionByScan searches the OpenCode data directory for the most
// recently written data that looks like conversation messages, without
// assuming a specific on-disk layout.
//
// OpenCode's on-disk storage format has changed across releases (flat
// per-project JSON files, nested session/message directories, SQLite), so
// hardcoding a single layout is fragile. Instead, this scans for the most
// recently modified *.json/*.jsonl file(s) whose content looks like a
// message (has a "role" or "parts" field, or is an array of such objects)
// within the recent-session window, and treats its containing directory as
// the session's message directory.
func discoverSessionByScan(dataDir string) *agent.SessionInfo {
	cutoff := time.Now().Add(-agent.RecentSessionTimeout)

	var bestPath string
	var bestModTime time.Time
	scanned := 0

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dataDir && skipStorageDirNames[strings.ToLower(d.Name())] {
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") && path != dataDir {
				return filepath.SkipDir
			}
			return nil
		}

		if scanned > maxStorageScanFiles {
			return filepath.SkipAll
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".json" && ext != ".jsonl" {
			return nil
		}
		scanned++

		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		if !info.ModTime().After(bestModTime) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil || !looksLikeMessageData(data) {
			return nil
		}

		bestPath = path
		bestModTime = info.ModTime()
		return nil
	})

	if bestPath == "" {
		return nil
	}

	sessionDir := filepath.Dir(bestPath)

	return &agent.SessionInfo{
		SessionID:      sessionIDFromPath(sessionDir, dataDir),
		TranscriptPath: sessionDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
	}
}

// looksLikeMessageData reports whether raw JSON content looks like an
// OpenCode message (an object with a "role" or "parts" field) or an array of
// such objects.
func looksLikeMessageData(raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return false
	}

	if strings.HasPrefix(trimmed, "[") {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
			return false
		}
		for _, item := range arr {
			if isMessageObject(item) {
				return true
			}
		}
		return false
	}

	return isMessageObject(json.RawMessage(trimmed))
}

// isMessageObject reports whether a single JSON value looks like a message
// object (has a "role" or "parts" field).
func isMessageObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	if _, ok := obj["role"]; ok {
		return true
	}
	if _, ok := obj["parts"]; ok {
		return true
	}
	return false
}

// sessionIDFromPath walks up from a message directory to find the nearest
// ancestor (bounded by dataDir) whose name isn't a well-known OpenCode
// storage folder, which is typically the session identifier.
func sessionIDFromPath(dir, dataDir string) string {
	dataDir = filepath.Clean(dataDir)
	for {
		name := filepath.Base(dir)
		if !staticStorageDirNames[strings.ToLower(name)] {
			return name
		}
		if dir == dataDir {
			return name
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return name
		}
		dir = parent
	}
}
```
