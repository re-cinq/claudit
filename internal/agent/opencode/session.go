```go
package opencode

import (
	"bytes"
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

// findRecentSessionFile recursively searches dataDir/storage for the most
// recently modified session record belonging to projectID, tolerating
// directory-layout differences across OpenCode versions (e.g. sessions
// nested under "session/info/" rather than directly under "session/<id>/").
// It matches either by parent directory name or by a projectID/directory
// field inside the JSON content. Returns "" if nothing recent is found.
func findRecentSessionFile(dataDir, projectID, projectPath string) (string, time.Time) {
	storageDir := filepath.Join(dataDir, "storage")
	now := time.Now()
	sep := string(filepath.Separator)

	var bestID string
	var bestMod time.Time

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		if !strings.Contains(path, sep+"session"+sep) {
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

		sessionID := strings.TrimSuffix(d.Name(), ".json")
		matches := filepath.Base(filepath.Dir(path)) == projectID

		if !matches {
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			var probe struct {
				ID        string `json:"id"`
				ProjectID string `json:"projectID"`
				Project   string `json:"project_id"`
				Directory string `json:"directory"`
				Worktree  string `json:"worktree"`
			}
			if json.Unmarshal(data, &probe) != nil {
				return nil
			}
			if probe.ProjectID != projectID && probe.Project != projectID &&
				probe.Directory != projectPath && probe.Worktree != projectPath {
				return nil
			}
			if probe.ID != "" {
				sessionID = probe.ID
			}
		}

		if bestID == "" || modTime.After(bestMod) {
			bestID = sessionID
			bestMod = modTime
		}
		return nil
	})

	return bestID, bestMod
}

// findSessionMessages recursively searches dataDir/storage for message
// files belonging to sessionID, tolerating nesting differences across
// OpenCode versions (e.g. messages nested under "session/message/<id>/"
// rather than directly under "message/<id>/"). Matching files are combined
// into a single JSON array. Returns nil if no message files are found.
func findSessionMessages(dataDir, sessionID string) []byte {
	if sessionID == "" {
		return nil
	}

	storageDir := filepath.Join(dataDir, "storage")
	sep := string(filepath.Separator)

	var paths []string
	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if strings.Contains(path, sep+sessionID+sep) || strings.HasPrefix(name, sessionID) {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)

	var messages []json.RawMessage
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 {
			continue
		}
		if trimmed[0] == '[' {
			var arr []json.RawMessage
			if json.Unmarshal(trimmed, &arr) == nil {
				messages = append(messages, arr...)
				continue
			}
		}
		if strings.HasSuffix(p, ".jsonl") {
			for _, line := range strings.Split(string(trimmed), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					messages = append(messages, json.RawMessage(line))
				}
			}
			continue
		}
		messages = append(messages, json.RawMessage(trimmed))
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
