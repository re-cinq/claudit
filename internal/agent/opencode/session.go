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

// maxSessionScanDepth bounds how deep scanForRecentSession and
// resolveMessageDir will recurse below their root directory. OpenCode's
// on-disk session layout has changed across releases (sessions partitioned
// into per-project directories vs. flat files that carry the project
// association inside the JSON payload); a shallow bounded walk lets
// discovery tolerate either layout without scanning unbounded state.
const maxSessionScanDepth = 4

// scanForRecentSession walks root (an OpenCode "storage/session" directory)
// for the most recently modified session JSON file, within
// agent.RecentSessionTimeout, that belongs to projectID/projectPath. A
// session file matches either by legacy path partitioning
// (storage/session/<projectID>/...) or by its "projectID"/"directory"
// fields, so this survives OpenCode changing how sessions are laid out on
// disk between versions.
func scanForRecentSession(root, projectID, projectPath string) (sessionInfo, time.Time, bool) {
	var best sessionInfo
	var bestModTime time.Time
	found := false
	now := time.Now()

	rootDepth := strings.Count(filepath.Clean(root), string(filepath.Separator))

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			depth := strings.Count(filepath.Clean(path), string(filepath.Separator)) - rootDepth
			if depth > maxSessionScanDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		// Legacy layout: sessions partitioned under storage/session/<projectID>/...
		rel, relErr := filepath.Rel(root, path)
		pathMatches := relErr == nil && (rel == projectID+".json" ||
			strings.HasPrefix(rel, projectID+string(filepath.Separator)))

		data, readErr := os.ReadFile(path)
		var s sessionInfo
		contentMatches := false
		if readErr == nil && json.Unmarshal(data, &s) == nil && s.ID != "" {
			contentMatches = s.ProjectID == projectID ||
				(s.Directory != "" && agent.PathsEqual(s.Directory, projectPath))
		}

		if !pathMatches && !contentMatches {
			return nil
		}
		if s.ID == "" {
			// Path matched but the file didn't parse into a usable session
			// (or had no "id" field); fall back to the filename.
			s.ID = strings.TrimSuffix(d.Name(), ".json")
		}

		if !found || info.ModTime().After(bestModTime) {
			best = s
			bestModTime = info.ModTime()
			found = true
		}
		return nil
	})

	return best, bestModTime, found
}

// resolveMessageDir locates the message directory for a session. It first
// tries the standard storage/message/<sessionID> path; if that doesn't
// exist (OpenCode has moved message storage around between releases), it
// falls back to a bounded search for a directory named after the session ID
// under storage/.
func resolveMessageDir(dataDir, sessionID string) string {
	direct := filepath.Join(dataDir, "storage", "message", sessionID)
	if info, err := os.Stat(direct); err == nil && info.IsDir() {
		return direct
	}

	storageRoot := filepath.Join(dataDir, "storage")
	rootDepth := strings.Count(filepath.Clean(storageRoot), string(filepath.Separator))
	var found string
	_ = filepath.WalkDir(storageRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		depth := strings.Count(filepath.Clean(path), string(filepath.Separator)) - rootDepth
		if depth > maxSessionScanDepth {
			return filepath.SkipDir
		}
		if d.Name() == sessionID {
			found = path
			return filepath.SkipAll
		}
		return nil
	})

	if found != "" {
		return found
	}
	return direct
}
