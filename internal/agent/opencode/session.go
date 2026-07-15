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

// scanSessionCandidate probes the fields OpenCode has used across versions to
// record which project a session belongs to (nested directory, embedded
// projectID, embedded cwd/directory path), since the on-disk layout has
// changed release to release.
type scanSessionCandidate struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	CWD       string `json:"cwd"`
	Path      string `json:"path"`
}

// ScanAllSessions recursively scans OpenCode's session storage tree for the
// most recently modified session that can be matched to projectPath, falling
// back to the single most recently modified session file overall if no
// content match is found. This is deliberately layout-agnostic: OpenCode has
// nested sessions under a per-project directory in some releases and stored
// them flat (with the project recorded inside the JSON) in others, so this
// does not assume one fixed directory structure the way GetSessionDir does.
func ScanAllSessions(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if _, err := os.Stat(sessionRoot); err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()

	var bestSessionID string
	var bestModTime time.Time
	var bestMatchedID string
	var bestMatchedModTime time.Time

	_ = filepath.Walk(sessionRoot, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		// Track the most recently modified session file overall, regardless
		// of whether it can be matched to this project. Used as a last
		// resort below if no content match is found.
		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(info.Name(), ".json")
			bestModTime = modTime
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		var candidate scanSessionCandidate
		if err := json.Unmarshal(data, &candidate); err != nil {
			return nil
		}

		matches := candidate.ProjectID != "" && candidate.ProjectID == projectID
		if !matches {
			for _, dir := range []string{candidate.Directory, candidate.CWD, candidate.Path} {
				if dir != "" && agent.PathsEqual(dir, projectPath) {
					matches = true
					break
				}
			}
		}
		if !matches {
			return nil
		}

		if bestMatchedID == "" || modTime.After(bestMatchedModTime) {
			id := candidate.ID
			if id == "" {
				id = strings.TrimSuffix(info.Name(), ".json")
			}
			bestMatchedID = id
			bestMatchedModTime = modTime
		}

		return nil
	})

	sessionID := bestMatchedID
	modTime := bestMatchedModTime
	if sessionID == "" {
		sessionID = bestSessionID
		modTime = bestModTime
	}

	if sessionID == "" {
		return nil, nil
	}

	msgDir, _ := GetMessageDir(sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: msgDir,
		StartedAt:      modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}
