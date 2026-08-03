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
// OpenCode has changed where it nests per-session message files across
// releases: older versions store them as a sibling of "session"
// (storage/message/<sessionID>), while newer versions nest them under the
// "session" namespace (storage/session/message/<sessionID>). Prefer whichever
// layout actually exists on disk, falling back to the legacy sibling path
// (which is also what WriteSessionFile/RestoreSession write to) when neither
// can be confirmed.
func GetMessageDir(sessionID string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}

	nested := filepath.Join(dataDir, "storage", "session", "message", sessionID)
	if info, err := os.Stat(nested); err == nil && info.IsDir() {
		return nested, nil
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

// ScanAllSessionsForProject scans every session file under storage/session/
// for one belonging to projectPath, without assuming a specific directory
// nesting scheme. OpenCode has changed how it organizes per-project session
// storage across releases (e.g. nesting sessions under a per-project
// directory named by hash, vs. storing them flat under an "info" directory
// and filtering by an embedded field), so directory-name-based lookups like
// discoverFromFlatFiles can silently find nothing even though a matching
// session exists on disk. This walks one level into every entry under
// storage/session/ (whether it's a per-project directory or a flat file) and
// matches each session file by its own recorded "projectID" or "directory"
// field instead of trusting where it happens to live.
func ScanAllSessionsForProject(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	sessionRoot := filepath.Join(dataDir, "storage", "session")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestSessionID string
	var bestModTime time.Time

	consider := func(path string, modTime time.Time) {
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var session sessionInfo
		if err := json.Unmarshal(data, &session); err != nil {
			return
		}

		matches := (projectID != "" && session.ProjectID == projectID) ||
			(session.Directory != "" && agent.PathsEqual(session.Directory, projectPath))
		if !matches {
			return
		}

		if bestSessionID != "" && !modTime.After(bestModTime) {
			return
		}

		sessionID := session.ID
		if sessionID == "" {
			sessionID = strings.TrimSuffix(filepath.Base(path), ".json")
		}

		bestSessionID = sessionID
		bestModTime = modTime
	}

	for _, entry := range entries {
		full := filepath.Join(sessionRoot, entry.Name())

		if entry.IsDir() {
			subEntries, err := os.ReadDir(full)
			if err != nil {
				continue
			}
			for _, sub := range subEntries {
				if sub.IsDir() || !strings.HasSuffix(sub.Name(), ".json") {
					continue
				}
				info, err := sub.Info()
				if err != nil {
					continue
				}
				consider(filepath.Join(full, sub.Name()), info.ModTime())
			}
			continue
		}

		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		consider(full, info.ModTime())
	}

	if bestSessionID == "" {
		return nil, nil
	}

	msgDir, _ := GetMessageDir(bestSessionID)
	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}
