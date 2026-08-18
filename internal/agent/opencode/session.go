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
	"sort"
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

// discoverByScanningStorage scans OpenCode's entire data directory for a
// session belonging to projectPath. OpenCode's on-disk storage layout has
// changed across versions (sessions nested under a project-hash directory,
// stored in SQLite, stored flat with an embedded directory field, etc.), so
// rather than assuming one fixed layout this walks every JSON file under the
// data directory and looks for one that identifies itself as belonging to
// this project via a directory/cwd/path/worktree field, picking the most
// recently modified match within the recent-session window.
func discoverByScanningStorage(dataDir, projectPath string) (*agent.SessionInfo, error) {
	storageDir := filepath.Join(dataDir, "storage")
	if _, err := os.Stat(storageDir); err != nil {
		return nil, nil
	}

	absProject := projectPath
	if abs, err := filepath.Abs(projectPath); err == nil {
		absProject = abs
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageDir, func(p string, d fs.DirEntry, err error) error {
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

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		var meta struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
			CWD       string `json:"cwd"`
			Path      string `json:"path"`
			Worktree  string `json:"worktree"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil
		}

		dir := meta.Directory
		for _, alt := range []string{meta.CWD, meta.Path, meta.Worktree} {
			if dir == "" {
				dir = alt
			}
		}
		if dir == "" || !agent.PathsEqual(dir, absProject) {
			return nil
		}

		id := meta.ID
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}

		if bestID == "" || modTime.After(bestModTime) {
			bestID = id
			bestModTime = modTime
		}
		return nil
	})

	if bestID == "" {
		return nil, nil
	}

	transcriptData := collectSessionMessages(storageDir, bestID)
	if len(transcriptData) == 0 {
		transcriptData = []byte("[]")
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// collectSessionMessages walks the storage directory for any JSON files
// associated with sessionID, matched by the session ID appearing as a path
// segment (e.g. ".../message/<sessionID>/...", ".../part/<sessionID>/...",
// or ".../session/message/<sessionID>/<messageID>.json"), and combines them
// into a single JSON array in path order. Returns nil if nothing is found.
func collectSessionMessages(storageDir, sessionID string) []byte {
	var paths []string
	_ = filepath.WalkDir(storageDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		rel, err := filepath.Rel(storageDir, p)
		if err != nil {
			return nil
		}
		for _, seg := range strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/") {
			if seg == sessionID {
				paths = append(paths, p)
				break
			}
		}
		return nil
	})

	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)

	var messages []json.RawMessage
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		messages = append(messages, json.RawMessage(data))
	}
	if len(messages) == 0 {
		return nil
	}

	out, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return out
}
```
