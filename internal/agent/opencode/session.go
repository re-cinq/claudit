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

// foundSession is a session located by walking OpenCode's storage tree.
type foundSession struct {
	SessionID string
	ModTime   time.Time
}

// findRecentProjectSession locates the most recently modified session
// belonging to projectID/projectPath within dataDir/storage.
//
// OpenCode has changed how it nests session files on disk across releases
// (flat per-project directories, then SQLite, and layouts in between), so
// rather than assuming one fixed directory depth we walk the whole storage
// tree and match on the session file's own identifying fields (or, for the
// legacy layout, on the projectID appearing as a path segment). This keeps
// discovery working across those on-disk layout changes.
func findRecentProjectSession(dataDir, projectID, projectPath string) *foundSession {
	storageRoot := filepath.Join(dataDir, "storage")
	now := time.Now()

	var best *foundSession

	_ = filepath.WalkDir(storageRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		rel, err := filepath.Rel(storageRoot, path)
		if err != nil {
			return nil
		}
		if hasPathSegment(rel, "message") || hasPathSegment(rel, "messages") {
			return nil // session messages live here, not session info
		}

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var fields struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectID"`
			Directory string `json:"directory"`
			CWD       string `json:"cwd"`
		}
		if err := json.Unmarshal(data, &fields); err != nil || fields.ID == "" {
			return nil
		}

		matches := fields.ProjectID == projectID ||
			fields.Directory == projectPath ||
			fields.CWD == projectPath ||
			hasPathSegment(rel, projectID)
		if !matches {
			return nil
		}

		if best == nil || info.ModTime().After(best.ModTime) {
			best = &foundSession{SessionID: fields.ID, ModTime: info.ModTime()}
		}
		return nil
	})

	return best
}

// findMessageDir locates the message directory for sessionID under
// dataDir/storage, preferring the known storage/message/<sessionID> path but
// falling back to a tree search since newer OpenCode versions nest it
// differently.
func findMessageDir(dataDir, sessionID string) string {
	direct := filepath.Join(dataDir, "storage", "message", sessionID)
	if info, err := os.Stat(direct); err == nil && info.IsDir() {
		return direct
	}

	storageRoot := filepath.Join(dataDir, "storage")
	var found string
	_ = filepath.WalkDir(storageRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
		}
		return nil
	})
	return found
}

// hasPathSegment reports whether rel contains seg as a whole path segment.
func hasPathSegment(rel, seg string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == seg {
			return true
		}
	}
	return false
}
