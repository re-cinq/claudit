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

// ScanAllProjectDirs scans every entry under storage/session for a session
// belonging to projectPath, without assuming OpenCode groups sessions under
// a directory keyed by our locally computed project ID (see GetProjectID).
// OpenCode's on-disk project ID scheme has changed across releases, so the
// directory GetSessionDir predicts may no longer be where a given version
// actually writes sessions. Each candidate entry — whether a nested
// per-project directory (older layout) or a session file directly under
// storage/session (newer flat layout) — is opened and matched against
// projectPath via its own recorded "directory" or "projectID" field, and the
// most recently modified match within the recent-session window wins.
func ScanAllProjectDirs(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	cleanProjectPath := filepath.Clean(projectPath)

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

		var sess sessionInfo
		if err := json.Unmarshal(data, &sess); err != nil {
			return
		}

		matches := sess.ProjectID != "" && sess.ProjectID == projectID
		if !matches && sess.Directory != "" {
			matches = filepath.Clean(sess.Directory) == cleanProjectPath
		}
		if !matches {
			return
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			id := sess.ID
			if id == "" {
				id = strings.TrimSuffix(filepath.Base(path), ".json")
			}
			bestSessionID = id
			bestModTime = modTime
		}
	}

	for _, entry := range entries {
		name := entry.Name()
		fullPath := filepath.Join(sessionRoot, name)

		if entry.IsDir() {
			children, err := os.ReadDir(fullPath)
			if err != nil {
				continue
			}
			for _, child := range children {
				if child.IsDir() || !strings.HasSuffix(child.Name(), ".json") {
					continue
				}
				info, err := child.Info()
				if err != nil {
					continue
				}
				consider(filepath.Join(fullPath, child.Name()), info.ModTime())
			}
			continue
		}

		if !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		consider(fullPath, info.ModTime())
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
