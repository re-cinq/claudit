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

// FindSessionByDirectory scans the entire session storage tree (across every
// project subdirectory under storage/session) for the most recently modified
// session belonging to projectPath, within timeout.
//
// OpenCode's internal project-ID generation (the subdirectory name under
// storage/session) is not a stable public contract and has changed across
// versions, so a session is matched primarily by the "directory" field
// recorded inside its own JSON file (compared with agent.PathsEqual), rather
// than by trying to reproduce OpenCode's project-ID algorithm. The predicted
// project directory name (GetProjectID) is still checked as a secondary
// signal for older OpenCode versions whose session files may not include a
// "directory" field.
func FindSessionByDirectory(dataDir, projectPath string, timeout time.Duration) (sessionID string, modTime time.Time, found bool) {
	sessionRoot := filepath.Join(dataDir, "storage", "session")
	now := time.Now()
	expectedProjectDir := GetProjectID(projectPath)

	_ = filepath.Walk(sessionRoot, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if now.Sub(info.ModTime()) > timeout {
			return nil
		}

		matchesPredictedDir := filepath.Base(filepath.Dir(p)) == expectedProjectDir

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var sess sessionInfo
		if err := json.Unmarshal(data, &sess); err != nil {
			return nil
		}

		if !matchesPredictedDir && (sess.Directory == "" || !agent.PathsEqual(sess.Directory, projectPath)) {
			return nil
		}

		if sess.ID == "" {
			sess.ID = strings.TrimSuffix(info.Name(), ".json")
		}

		if found && !info.ModTime().After(modTime) {
			return nil
		}

		sessionID = sess.ID
		modTime = info.ModTime()
		found = true
		return nil
	})

	return sessionID, modTime, found
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
