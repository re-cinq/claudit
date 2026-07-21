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
// This matches OpenCode's legacy (pre-v1.2-era) layout, where sessions are
// nested under a per-project directory keyed by project ID.
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

// sessionIdentity captures the fields different OpenCode releases have used
// to record which project a session file belongs to. Newer releases store
// all sessions in a single flat directory and rely on one of these fields
// (rather than directory nesting) to scope a session to a project.
type sessionIdentity struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	Cwd       string `json:"cwd"`
	Path      string `json:"path"`
}

// matchesProject reports whether a session's identity fields indicate it
// belongs to the given project. If none of the known identity fields are
// present, the session can't be ruled out, so it's treated as a match.
func (s sessionIdentity) matchesProject(projectPath, projectID string) bool {
	switch {
	case s.ProjectID != "":
		return s.ProjectID == projectID
	case s.Directory != "":
		return s.Directory == projectPath
	case s.Cwd != "":
		return s.Cwd == projectPath
	case s.Path != "":
		return s.Path == projectPath
	default:
		return true
	}
}

// FindRecentSessionFile searches OpenCode's flat-file session storage for
// the most recently modified session belonging to projectPath. It supports
// two on-disk layouts that different OpenCode releases have used:
//
//   - Nested: storage/session/<projectID>/<sessionID>.json
//   - Flat: storage/session/<sessionID>.json, with the owning project
//     recorded inside the file (projectID/directory/cwd/path field).
//
// Returns found=false if no session directory exists or no session matches.
func FindRecentSessionFile(dataDir, projectPath string) (sessionID string, modTime time.Time, found bool) {
	sessionRoot := filepath.Join(dataDir, "storage", "session")
	projectID := GetProjectID(projectPath)

	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return "", time.Time{}, false
	}

	var bestID string
	var bestModTime time.Time

	consider := func(id string, mt time.Time) {
		if bestID == "" || mt.After(bestModTime) {
			bestID = id
			bestModTime = mt
		}
	}

	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() {
			// Nested layout: only descend into this project's directory.
			if name != projectID {
				continue
			}
			projEntries, err := os.ReadDir(filepath.Join(sessionRoot, name))
			if err != nil {
				continue
			}
			for _, pe := range projEntries {
				if pe.IsDir() || !strings.HasSuffix(pe.Name(), ".json") {
					continue
				}
				info, err := pe.Info()
				if err != nil {
					continue
				}
				consider(strings.TrimSuffix(pe.Name(), ".json"), info.ModTime())
			}
			continue
		}

		if !strings.HasSuffix(name, ".json") {
			continue
		}

		// Flat layout: filter by the project identity embedded in the file.
		data, err := os.ReadFile(filepath.Join(sessionRoot, name))
		if err != nil {
			continue
		}
		var ident sessionIdentity
		if err := json.Unmarshal(data, &ident); err != nil {
			continue
		}
		if !ident.matchesProject(projectPath, projectID) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		id := ident.ID
		if id == "" {
			id = strings.TrimSuffix(name, ".json")
		}
		consider(id, info.ModTime())
	}

	if bestID == "" {
		return "", time.Time{}, false
	}
	return bestID, bestModTime, true
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
