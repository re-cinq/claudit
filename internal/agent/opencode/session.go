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

// discoverByContentScan walks the OpenCode session storage tree looking for
// a session file whose embedded project directory matches projectPath.
// Unlike the project-ID-keyed lookup in GetSessionDir, this doesn't assume
// sessions are grouped into a subdirectory keyed by any particular
// project-ID scheme — OpenCode has changed that scheme more than once, and
// this scan tolerates future layout changes as long as sessions are still
// stored as JSON somewhere under storage/session and still record which
// directory they belong to.
func discoverByContentScan(dataDir, projectPath string) (*agent.SessionInfo, error) {
	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if _, err := os.Stat(sessionRoot); err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestSessionID, bestSessionPath string
	var bestModTime time.Time

	_ = filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var raw map[string]interface{}
		if json.Unmarshal(data, &raw) != nil || !sessionMatchesProject(raw, projectPath) {
			return nil
		}

		if bestSessionID == "" || info.ModTime().After(bestModTime) {
			id, _ := raw["id"].(string)
			if id == "" {
				id = strings.TrimSuffix(d.Name(), ".json")
			}
			bestSessionID = id
			bestSessionPath = path
			bestModTime = info.ModTime()
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: resolveMessageDir(dataDir, bestSessionID, bestSessionPath),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// sessionMatchesProject checks whether a session record identifies
// projectPath as its working directory, trying the field names OpenCode
// has used for this across versions.
func sessionMatchesProject(raw map[string]interface{}, projectPath string) bool {
	for _, key := range []string{"directory", "path", "cwd", "worktree"} {
		v, ok := raw[key].(string)
		if !ok || v == "" {
			continue
		}
		if agent.PathsEqual(v, projectPath) {
			return true
		}
	}
	return false
}

// resolveMessageDir locates the directory holding a session's message files.
// It tries the classic storage/message/<id> location, then a message
// subdirectory next to the matched session file, then falls back to the
// session file itself in case messages are embedded inline.
func resolveMessageDir(dataDir, sessionID, sessionFilePath string) string {
	if msgDir, err := GetMessageDir(sessionID); err == nil {
		if entries, err := os.ReadDir(msgDir); err == nil && len(entries) > 0 {
			return msgDir
		}
	}

	sessionDir := filepath.Dir(sessionFilePath)
	for _, name := range []string{"message", "messages"} {
		candidate := filepath.Join(sessionDir, name)
		if entries, err := os.ReadDir(candidate); err == nil && len(entries) > 0 {
			return candidate
		}
	}

	return sessionFilePath
}
