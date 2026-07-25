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

// candidateSession is a loosely-typed view of an OpenCode session JSON file,
// tolerant of field names/casing that may differ across OpenCode versions.
type candidateSession struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectID"`
	ProjectID2  string `json:"project_id"`
	Directory   string `json:"directory"`
	Directory2  string `json:"path"`
	Directory3  string `json:"cwd"`
	Directory4  string `json:"worktree"`
	ProjectPath string `json:"projectPath"`
}

func (c candidateSession) matchesProject(projectID, projectPath string) bool {
	for _, id := range []string{c.ProjectID, c.ProjectID2} {
		if id != "" && id == projectID {
			return true
		}
	}
	for _, dir := range []string{c.Directory, c.Directory2, c.Directory3, c.Directory4, c.ProjectPath} {
		if dir != "" && agent.PathsEqual(dir, projectPath) {
			return true
		}
	}
	return false
}

// ScanAllProjectDirs scans every directory under storage/session/ for a session
// belonging to projectPath, matching by the session file's own recorded
// project/directory fields rather than trusting that OpenCode named the
// directory after the project ID computed by GetProjectID.
//
// This is a fallback used when the primary project-ID-named directory lookup
// (GetSessionDir) finds nothing — e.g. when the on-disk project scoping
// scheme changes between OpenCode versions and no longer matches the git
// root-commit-hash assumption baked into GetProjectID.
func ScanAllProjectDirs(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionsRoot := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()
	var bestSessionID string
	var bestModTime time.Time

	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}

		dir := filepath.Join(sessionsRoot, pd.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		// Fast path: this directory is already named after the project ID.
		// (GetSessionDir would have found it, but scan it too in case the
		// primary lookup bailed out early for an unrelated reason.)
		matchesByName := pd.Name() == projectID

		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				continue
			}

			modTime := info.ModTime()
			if now.Sub(modTime) > agent.RecentSessionTimeout {
				continue
			}
			if bestSessionID != "" && !modTime.After(bestModTime) {
				continue
			}

			filePath := filepath.Join(dir, entry.Name())

			if !matchesByName {
				data, err := os.ReadFile(filePath)
				if err != nil {
					continue
				}
				var candidate candidateSession
				if err := json.Unmarshal(data, &candidate); err != nil {
					continue
				}
				if !candidate.matchesProject(projectID, projectPath) {
					continue
				}
			}

			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
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
