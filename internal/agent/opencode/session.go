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

// maxSessionInfoFileSize bounds how large a candidate file can be before it's
// read during storage tree scanning. Session metadata blobs are small; this
// avoids wasting time parsing large message/part content files.
const maxSessionInfoFileSize = 1 << 20 // 1MB

// openCodeSessionCandidate is a session discovered by scanning OpenCode's
// storage directory for JSON blobs that carry both a session id and the
// project directory the session belongs to.
type openCodeSessionCandidate struct {
	id        string
	directory string
	updated   time.Time
	path      string
}

// discoverFromStorageTree scans OpenCode's storage directory for session
// metadata blobs, regardless of how they're nested on disk. OpenCode has
// changed its on-disk layout across releases (per-project directories,
// SQLite, flat blob stores), but session metadata has consistently carried
// both an id and the originating project directory somewhere in the blob.
// A file is treated as session metadata if it decodes as a JSON object with
// an id-like field and a directory-like field. Returns nil if no session in
// the tree matches projectPath.
func discoverFromStorageTree(dataDir, projectPath string) *agent.SessionInfo {
	candidates := discoverSessionCandidates(dataDir)
	if len(candidates) == 0 {
		return nil
	}

	now := time.Now()
	var best *openCodeSessionCandidate
	for i := range candidates {
		c := &candidates[i]
		if !agent.PathsEqual(c.directory, projectPath) {
			continue
		}
		if now.Sub(c.updated) > agent.RecentSessionTimeout {
			continue
		}
		if best == nil || c.updated.After(best.updated) {
			best = c
		}
	}
	if best == nil {
		return nil
	}

	session := &agent.SessionInfo{
		SessionID:   best.id,
		StartedAt:   best.updated.Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	if msgDir := findSessionMessageDir(dataDir, best.id); msgDir != "" {
		session.TranscriptPath = msgDir
		return session
	}

	// No separate message directory could be located; fall back to the
	// session metadata blob itself so a note can still be written.
	if data, err := os.ReadFile(best.path); err == nil {
		session.TranscriptData = []byte("[" + string(data) + "]")
	}
	return session
}

// discoverSessionCandidates walks dataDir/storage looking for JSON files
// that look like OpenCode session metadata.
func discoverSessionCandidates(dataDir string) []openCodeSessionCandidate {
	var candidates []openCodeSessionCandidate

	root := filepath.Join(dataDir, "storage")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil || info.Size() > maxSessionInfoFileSize {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil
		}

		id, ok := stringFieldFrom(raw, "id", "sessionID", "session_id")
		if !ok {
			return nil
		}
		directory, ok := stringFieldFrom(raw, "directory", "cwd", "worktree", "path", "projectPath", "project_path", "workingDirectory")
		if !ok {
			return nil
		}

		candidates = append(candidates, openCodeSessionCandidate{
			id:        id,
			directory: directory,
			updated:   sessionUpdatedTime(raw, info.ModTime()),
			path:      path,
		})
		return nil
	})

	return candidates
}

// stringFieldFrom returns the first non-empty string value found under any
// of the given keys.
func stringFieldFrom(raw map[string]json.RawMessage, keys ...string) (string, bool) {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			return s, true
		}
	}
	return "", false
}

// sessionUpdatedTime extracts a session's last-updated time from common
// field shapes OpenCode has used ("time":{"updated":...} as epoch millis, or
// RFC3339 strings), falling back to the file's modification time.
func sessionUpdatedTime(raw map[string]json.RawMessage, fallback time.Time) time.Time {
	if timeRaw, ok := raw["time"]; ok {
		var millis struct {
			Updated float64 `json:"updated"`
			Created float64 `json:"created"`
		}
		if err := json.Unmarshal(timeRaw, &millis); err == nil {
			if millis.Updated > 0 {
				return time.UnixMilli(int64(millis.Updated))
			}
			if millis.Created > 0 {
				return time.UnixMilli(int64(millis.Created))
			}
		}

		var strs struct {
			Updated string `json:"updated"`
			Created string `json:"created"`
		}
		if err := json.Unmarshal(timeRaw, &strs); err == nil {
			if t, err := time.Parse(time.RFC3339, strs.Updated); err == nil {
				return t
			}
			if t, err := time.Parse(time.RFC3339, strs.Created); err == nil {
				return t
			}
		}
	}

	if v, ok := stringFieldFrom(raw, "updated", "updatedAt", "created", "createdAt"); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}

	return fallback
}

// findSessionMessageDir looks for a directory under dataDir/storage named
// after sessionID, which is where OpenCode stores that session's messages
// regardless of how deeply it's nested (e.g. storage/message/<id> or
// storage/session/message/<id>).
func findSessionMessageDir(dataDir, sessionID string) string {
	root := filepath.Join(dataDir, "storage")

	var found string
	var fallback string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || d.Name() != sessionID {
			return nil
		}

		entries, err := os.ReadDir(path)
		if err != nil || len(entries) == 0 {
			return nil
		}

		if strings.Contains(filepath.ToSlash(path), "/message/") {
			if found == "" {
				found = path
			}
		} else if fallback == "" {
			fallback = path
		}
		return nil
	})

	if found != "" {
		return found
	}
	return fallback
}
```
