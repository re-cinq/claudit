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

// scanAllSessionDirs performs a broad, best-effort scan of OpenCode's session
// storage directory (storage/session) for the most recently modified session
// belonging to projectPath.
//
// GetSessionDir/GetProjectID assume sessions are nested as
// storage/session/<projectID>/<sessionID>.json, where <projectID> is derived
// from the git root commit hash. OpenCode has changed its project scoping
// scheme across releases, so a projectID-based lookup can silently return
// nothing even though a matching session exists on disk. This scan tolerates
// both a flat layout (storage/session/<sessionID>.json) and a nested one, and
// prefers a session whose embedded "directory" field matches projectPath. If
// no session carries that field (or none matches), it falls back to the most
// recently modified session overall — safe for the common case of a single
// active project, and strictly better than discovering nothing.
func scanAllSessionDirs(dataDir, projectPath string) (*agent.SessionInfo, error) {
	sessionRoot := filepath.Join(dataDir, "storage", "session")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestSessionID string
	var bestModTime time.Time
	var bestMatched bool

	consider := func(path string, name string, modTime time.Time) {
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return
		}

		sessionID := strings.TrimSuffix(name, ".json")
		matched := false

		if data, err := os.ReadFile(path); err == nil {
			var header sessionInfo
			if json.Unmarshal(data, &header) == nil {
				if header.ID != "" {
					sessionID = header.ID
				}
				if header.Directory != "" {
					matched = agent.PathsEqual(header.Directory, projectPath)
				}
			}
		}

		if matched && (!bestMatched || modTime.After(bestModTime)) {
			bestSessionID, bestModTime, bestMatched = sessionID, modTime, true
			return
		}
		if !bestMatched && (bestSessionID == "" || modTime.After(bestModTime)) {
			bestSessionID, bestModTime = sessionID, modTime
		}
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			consider(filepath.Join(sessionRoot, entry.Name()), entry.Name(), info.ModTime())
			continue
		}

		nestedDir := filepath.Join(sessionRoot, entry.Name())
		nested, err := os.ReadDir(nestedDir)
		if err != nil {
			continue
		}
		for _, file := range nested {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			info, err := file.Info()
			if err != nil {
				continue
			}
			consider(filepath.Join(nestedDir, file.Name()), file.Name(), info.ModTime())
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
