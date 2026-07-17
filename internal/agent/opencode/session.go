```go
package opencode

import (
	"encoding/json"
	"errors"
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

// sessionProjectIDFields lists the JSON keys OpenCode has used across
// versions to embed a session's project-ID (root commit hash) identity.
var sessionProjectIDFields = []string{"projectID", "projectId", "project_id"}

// sessionDirectoryFields lists the JSON keys OpenCode has used across
// versions to embed a session's working-directory identity, for versions
// that key projects by directory rather than by git root commit hash.
var sessionDirectoryFields = []string{"directory", "cwd", "path", "worktree", "root"}

// errStopWalk aborts a filepath.WalkDir traversal once a match is found.
var errStopWalk = errors.New("stop walk")

// ScanAllSessions performs a broad, content-based scan of the entire session
// storage tree for the most recently modified session belonging to
// projectPath/projectID. Unlike GetSessionDir, it does not assume sessions
// live under storage/session/<projectID>/ — some OpenCode versions store all
// sessions flat (or nested differently) and embed the project identity
// inside each session's JSON instead of using it as a directory name. This
// is used as a fallback when the project-partitioned directory lookup finds
// nothing, so session discovery keeps working across that kind of layout
// drift.
func ScanAllSessions(dataDir, projectPath, projectID string) (*agent.SessionInfo, error) {
	root := filepath.Join(dataDir, "storage", "session")

	now := time.Now()
	var bestPath, bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
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

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return nil
		}
		if !sessionBelongsToProject(fields, projectPath, projectID) {
			return nil
		}

		sessionID := strings.TrimSuffix(d.Name(), ".json")
		if idRaw, ok := fields["id"]; ok {
			var id string
			if err := json.Unmarshal(idRaw, &id); err == nil && id != "" {
				sessionID = id
			}
		}

		if bestPath == "" || modTime.After(bestModTime) {
			bestPath = p
			bestSessionID = sessionID
			bestModTime = modTime
		}
		return nil
	})

	if bestPath == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: ResolveMessageDir(dataDir, bestSessionID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// sessionBelongsToProject checks whether a session's raw JSON fields
// identify it as belonging to projectPath/projectID. It tries known
// project-ID fields (root commit hash) first, then falls back to
// directory/path fields for versions that key projects by working
// directory instead.
func sessionBelongsToProject(fields map[string]json.RawMessage, projectPath, projectID string) bool {
	for _, key := range sessionProjectIDFields {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var v string
		if err := json.Unmarshal(raw, &v); err == nil && v != "" {
			return v == projectID
		}
	}
	for _, key := range sessionDirectoryFields {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var v string
		if err := json.Unmarshal(raw, &v); err == nil && v != "" {
			return agent.PathsEqual(v, projectPath)
		}
	}
	return false
}

// ResolveMessageDir returns the directory containing a session's message
// files. It tries the canonical storage/message/<sessionID> path first (used
// by GetMessageDir), then falls back to searching the whole storage tree for
// a directory literally named after the session ID. This keeps message
// lookup working for OpenCode versions that nest messages differently (e.g.
// under storage/session/message/<id> instead of storage/message/<id>).
func ResolveMessageDir(dataDir, sessionID string) string {
	canonical := filepath.Join(dataDir, "storage", "message", sessionID)
	if info, err := os.Stat(canonical); err == nil && info.IsDir() {
		return canonical
	}

	if found := findDirNamed(filepath.Join(dataDir, "storage"), sessionID); found != "" {
		return found
	}

	return canonical
}

// findDirNamed returns the path of the first directory named exactly `name`
// found under root, or "" if none is found.
func findDirNamed(root, name string) string {
	var found string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() && p != root && d.Name() == name {
			found = p
			return errStopWalk
		}
		return nil
	})
	return found
}
```
