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

// findRecentSession performs a resilient recursive scan of the OpenCode
// storage directory for a session belonging to projectPath.
//
// Older OpenCode releases group session files under storage/session/<projectID>/,
// which is what GetSessionDir/GetMessageDir assume. Newer releases have been
// observed to drop that project-based directory nesting and instead record
// the owning project via a "directory" (absolute project path) or "projectID"
// field inside the session JSON itself. This scan tolerates either layout by
// walking storage/session recursively (whatever its depth) and matching on
// the fields inside each file rather than on where the file lives.
func findRecentSession(dataDir, projectPath, projectID string) (*agent.SessionInfo, error) {
	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if info, err := os.Stat(sessionRoot); err != nil || !info.IsDir() {
		return nil, nil
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time

	_ = filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Best-effort scan: skip unreadable entries rather than aborting.
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := fi.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		id, directory, candidateProjectID, ok := parseSessionCandidate(data)
		if !ok {
			return nil
		}

		matches := directory != "" && agent.PathsEqual(directory, projectPath)
		if !matches && candidateProjectID != "" {
			matches = candidateProjectID == projectID
		}
		if !matches {
			return nil
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

	msgDir := findMessageLocation(dataDir, bestID)
	if msgDir == "" {
		msgDir, _ = GetMessageDir(bestID)
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// parseSessionCandidate extracts identifying fields from a session JSON file,
// tolerating field-name variations across OpenCode versions.
func parseSessionCandidate(data []byte) (id, directory, projectID string, ok bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", "", false
	}

	readString := func(keys ...string) string {
		for _, k := range keys {
			v, present := raw[k]
			if !present {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err == nil && s != "" {
				return s
			}
		}
		return ""
	}

	id = readString("id", "sessionID", "session_id")
	if id == "" {
		return "", "", "", false
	}
	directory = readString("directory", "cwd", "worktree", "path")
	projectID = readString("projectID", "project_id")
	return id, directory, projectID, true
}

// findMessageLocation searches the OpenCode storage tree for the directory
// holding messages for the given session, tolerating layout differences
// across versions (e.g. storage/message/<id>/ vs storage/session/message/<id>/).
func findMessageLocation(dataDir, sessionID string) string {
	storageRoot := filepath.Join(dataDir, "storage")
	var found string

	_ = filepath.WalkDir(storageRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return filepath.SkipDir
		}
		return nil
	})

	return found
}
```
