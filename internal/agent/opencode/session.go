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

// discoveredSession is a session found while scanning OpenCode's on-disk
// storage for a recently active session.
type discoveredSession struct {
	sessionID string
	modTime   time.Time
}

// findRecentSessionInDir returns the most recently modified *.json session
// file directly inside dir (non-recursive), within RecentSessionTimeout.
func findRecentSessionInDir(dir string) *discoveredSession {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	now := time.Now()
	var best *discoveredSession

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

		if best == nil || modTime.After(best.modTime) {
			best = &discoveredSession{
				sessionID: strings.TrimSuffix(entry.Name(), ".json"),
				modTime:   modTime,
			}
		}
	}

	return best
}

// sessionFileMeta captures the fields OpenCode has used, across releases, to
// record which working directory a session belongs to. Newer releases have
// changed how sessions are keyed on disk (dropping or renaming the
// project-ID subdirectory), so matching on these fields lets discovery keep
// working even when the on-disk layout shifts between versions.
type sessionFileMeta struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Path      string `json:"path"`
	Cwd       string `json:"cwd"`
	Worktree  string `json:"worktree"`
}

func (m sessionFileMeta) matchesProject(projectPath string) bool {
	for _, candidate := range []string{m.Directory, m.Path, m.Cwd, m.Worktree} {
		if candidate != "" && agent.PathsEqual(candidate, projectPath) {
			return true
		}
	}
	return false
}

// findRecentSessionByDirectory walks an OpenCode session storage tree —
// which may nest sessions under a project-ID subdirectory, store them flat,
// or store them as <sessionID>/info.json depending on the OpenCode version —
// and returns the most recently modified session whose recorded working
// directory matches projectPath. This is used as a fallback when the
// project-ID-keyed directory OpenCode is currently expected to use doesn't
// yield a session, so discovery survives on-disk layout changes across
// OpenCode releases.
func findRecentSessionByDirectory(root, projectPath string) *discoveredSession {
	now := time.Now()
	var best *discoveredSession

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		// Message payloads live under directories named "message"; those
		// aren't session metadata, skip them.
		if strings.Contains(filepath.ToSlash(path), "/message/") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var meta sessionFileMeta
		if err := json.Unmarshal(data, &meta); err != nil || !meta.matchesProject(projectPath) {
			return nil
		}

		sessionID := meta.ID
		if sessionID == "" {
			sessionID = strings.TrimSuffix(d.Name(), ".json")
		}

		if best == nil || modTime.After(best.modTime) {
			best = &discoveredSession{sessionID: sessionID, modTime: modTime}
		}
		return nil
	})

	return best
}

// resolveMessagePath finds where a session's messages are stored on disk.
// It tries the historical flat "storage/message/<sessionID>" layout first,
// then falls back to "storage/session/<sessionID>/message" used by newer
// OpenCode releases that nest messages alongside their session metadata.
func resolveMessagePath(dataDir, sessionID string) string {
	flat := filepath.Join(dataDir, "storage", "message", sessionID)
	nested := filepath.Join(dataDir, "storage", "session", sessionID, "message")

	for _, candidate := range []string{flat, nested} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	return flat
}
