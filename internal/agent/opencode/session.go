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

// findRecentSessionID locates the most recently modified session belonging
// to projectPath, within the recent-session timeout window.
//
// OpenCode has changed its on-disk storage layout across releases (project-
// keyed session directories, flatter global layouts with the project
// reference embedded inside each session file, etc). To stay compatible
// across those changes, this first tries the conventional project-keyed
// directory (storage/session/<projectID>/*.json) and, if that yields
// nothing, falls back to scanning the broader session storage tree and
// matching sessions by their own projectID/directory fields instead of by
// directory structure.
func findRecentSessionID(dataDir, projectID, projectPath string) (sessionID string, modTime time.Time, found bool) {
	now := time.Now()

	sessionDir := filepath.Join(dataDir, "storage", "session", projectID)
	if id, mt, ok := mostRecentJSONFile(sessionDir, now); ok {
		return id, mt, true
	}

	root := filepath.Join(dataDir, "storage", "session")
	var bestID string
	var bestModTime time.Time
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		mt := info.ModTime()
		if now.Sub(mt) > agent.RecentSessionTimeout {
			return nil
		}
		if bestID != "" && !mt.After(bestModTime) {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var sf sessionInfo
		if err := json.Unmarshal(data, &sf); err != nil || sf.ID == "" {
			return nil
		}
		if sf.ProjectID != projectID && sf.Directory != projectPath {
			return nil
		}

		bestID = sf.ID
		bestModTime = mt
		return nil
	})

	if bestID == "" {
		return "", time.Time{}, false
	}
	return bestID, bestModTime, true
}

// mostRecentJSONFile returns the session ID (filename without extension) of
// the most recently modified .json file directly inside dir, if any exist
// within the recent-session timeout window.
func mostRecentJSONFile(dir string, now time.Time) (sessionID string, modTime time.Time, found bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}

	var bestSessionID string
	var bestModTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		mt := info.ModTime()
		if now.Sub(mt) > agent.RecentSessionTimeout {
			continue
		}

		if bestSessionID == "" || mt.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = mt
		}
	}

	if bestSessionID == "" {
		return "", time.Time{}, false
	}
	return bestSessionID, bestModTime, true
}

// findMessageDir locates the directory containing message JSON files for a
// session, tolerating storage layout changes across OpenCode versions
// (message trees keyed directly by session ID, nested under the project ID,
// or nested under the session's own directory).
func findMessageDir(dataDir, projectID, sessionID string) (string, bool) {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", sessionID, "message"),
		filepath.Join(dataDir, "storage", "session", projectID, sessionID, "message"),
		filepath.Join(dataDir, "storage", "session", projectID, sessionID),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c, true
		}
	}

	root := filepath.Join(dataDir, "storage")
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || found != "" || !d.IsDir() {
			return nil
		}
		if d.Name() == sessionID {
			found = p
			return fs.SkipAll
		}
		if d.Name() == "message" && filepath.Base(filepath.Dir(p)) == sessionID {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	if found != "" {
		return found, true
	}
	return "", false
}

// collectMessageData recursively gathers message JSON/JSONL files under dir
// into a single JSON array, mirroring the shape ParseTranscript expects.
// Recursing (rather than assuming a flat directory) keeps this resilient to
// OpenCode nesting messages one or more levels deeper than expected.
func collectMessageData(dir string) []byte {
	var messages []json.RawMessage
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}

		name := d.Name()
		isJSON := strings.HasSuffix(name, ".json")
		isJSONL := strings.HasSuffix(name, ".jsonl")
		if !isJSON && !isJSONL {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		if isJSONL {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				messages = append(messages, json.RawMessage(line))
			}
		} else {
			messages = append(messages, json.RawMessage(data))
		}
		return nil
	})

	if len(messages) == 0 {
		return nil
	}

	data, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return data
}
```
