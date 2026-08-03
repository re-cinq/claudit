```go
package opencode

import (
	"encoding/json"
	"fmt"
	"io/fs"
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

// dataDirCandidates returns the locations OpenCode may store project data in,
// most-preferred first: the global XDG/Application Support directory, and a
// project-local .opencode directory. Recent OpenCode releases have shifted
// where session data lives, so both are checked during session discovery.
func dataDirCandidates(projectPath string) []string {
	var dirs []string
	if global, err := GetDataDir(); err == nil {
		dirs = append(dirs, global)
	}
	if projectPath != "" {
		dirs = append(dirs, filepath.Join(projectPath, ".opencode"))
	}
	return dirs
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

// sessionFileCandidate is a session JSON file discovered during a recursive
// storage search, along with its modification time.
type sessionFileCandidate struct {
	SessionID string
	Path      string
	ModTime   time.Time
}

// findSessionFiles recursively walks dataDir/storage for *.json files that
// live under a directory named "session", returning candidates modified
// within recentTimeout. OpenCode has changed the nesting depth of session
// storage across releases (e.g. adding a "project/" prefix or an "info"
// subdirectory), so this does not assume the fixed depth that GetSessionDir
// does — it's a fallback for when the fixed-path lookup finds nothing.
func findSessionFiles(dataDir string, recentTimeout time.Duration) []sessionFileCandidate {
	storageDir := filepath.Join(dataDir, "storage")

	var candidates []sessionFileCandidate
	now := time.Now()

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		if !strings.Contains(filepath.ToSlash(filepath.Dir(path)), "session") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			return nil
		}

		candidates = append(candidates, sessionFileCandidate{
			SessionID: strings.TrimSuffix(d.Name(), ".json"),
			Path:      path,
			ModTime:   modTime,
		})
		return nil
	})

	return candidates
}

// findMessageDir recursively searches dataDir/storage for a directory
// belonging to the given session, tolerating layout changes that the fixed
// storage/message/<sessionID> path assumed by GetMessageDir doesn't cover.
func findMessageDir(dataDir, sessionID string) string {
	storageDir := filepath.Join(dataDir, "storage")

	var found string
	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return filepath.SkipAll
		}
		return nil
	})

	return found
}
```
