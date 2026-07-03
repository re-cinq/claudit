```go
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

// FindMessageDir locates the message directory for sessionID. It first tries
// the conventional storage/message/<sessionID> path, then falls back to a
// recursive search under the data directory in case a newer OpenCode
// release nests message storage differently (e.g. under a project or
// worktree subdirectory).
func FindMessageDir(sessionID string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}

	direct := filepath.Join(dataDir, "storage", "message", sessionID)
	if info, err := os.Stat(direct); err == nil && info.IsDir() {
		return direct, nil
	}

	storageRoot := filepath.Join(dataDir, "storage")
	var found string
	_ = filepath.WalkDir(storageRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return filepath.SkipAll
		}
		return nil
	})

	if found != "" {
		return found, nil
	}

	return direct, nil
}

// ScanAllSessionDirs scans every project directory under the session storage
// root for the most recent session belonging to projectPath. It is used as a
// fallback when GetProjectID's git-root-commit hash no longer matches how
// OpenCode groups sessions on disk (e.g. after a storage layout change in a
// newer release). It prefers a session whose recorded directory/path/cwd
// field matches projectPath, and falls back to the single most recently
// modified session across all projects when no field match is found.
func ScanAllSessionDirs(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestSessionID string
	var bestModTime time.Time
	var bestMatched bool
	found := false

	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		dir := filepath.Join(sessionRoot, pd.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			modTime := info.ModTime()
			if now.Sub(modTime) > agent.RecentSessionTimeout {
				continue
			}

			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}

			var header struct {
				ID        string `json:"id"`
				Directory string `json:"directory"`
				Path      string `json:"path"`
				Cwd       string `json:"cwd"`
				Worktree  string `json:"worktree"`
			}
			_ = json.Unmarshal(data, &header)

			matched := header.Directory == projectPath || header.Path == projectPath ||
				header.Cwd == projectPath || header.Worktree == projectPath

			// Prefer field-matched sessions over unmatched ones; among
			// sessions with the same match status, prefer the most recent.
			if found {
				if bestMatched && !matched {
					continue
				}
				if bestMatched == matched && !modTime.After(bestModTime) {
					continue
				}
			}

			sessionID := header.ID
			if sessionID == "" {
				sessionID = strings.TrimSuffix(e.Name(), ".json")
			}

			bestSessionID = sessionID
			bestModTime = modTime
			bestMatched = matched
			found = true
		}
	}

	if !found {
		return nil, nil
	}

	msgDir, _ := FindMessageDir(bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
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
```
