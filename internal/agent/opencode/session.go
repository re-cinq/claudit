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

// discoveredSession represents a session file found while scanning OpenCode's
// data directory, along with the metadata needed to decide whether it
// belongs to the current project and how recent it is.
type discoveredSession struct {
	Path      string
	SessionID string
	ModTime   time.Time
}

// sessionCandidate is the subset of session JSON fields used to match a
// session file to a project, regardless of which on-disk layout the
// installed OpenCode version uses.
type sessionCandidate struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
}

// maxSessionScanDepth bounds how deep findRecentSession recurses into
// OpenCode's data directory, to avoid pathological slowdowns on unusually
// large or deep directory trees.
const maxSessionScanDepth = 8

// findRecentSession scans dataDir for the most recently modified session
// file belonging to projectPath. OpenCode's on-disk storage layout has
// changed across releases (e.g. a `storage/session/<projectID>/<id>.json`
// layout vs. a flat `storage/session/info/<id>.json` layout with the project
// reference embedded in the file), so sessions are matched by the
// "directory"/"projectID" fields inside each JSON file instead of assuming a
// fixed directory shape.
func findRecentSession(dataDir, projectPath, projectID string) *discoveredSession {
	root := filepath.Join(dataDir, "storage")
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		root = dataDir
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil
	}

	rootDepth := strings.Count(filepath.Clean(root), string(filepath.Separator))

	var best *discoveredSession
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			depth := strings.Count(filepath.Clean(path), string(filepath.Separator)) - rootDepth
			if depth > maxSessionScanDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var candidate sessionCandidate
		if err := json.Unmarshal(data, &candidate); err != nil || candidate.ID == "" {
			return nil
		}

		matches := false
		if candidate.Directory != "" && agent.PathsEqual(candidate.Directory, projectPath) {
			matches = true
		} else if candidate.ProjectID != "" && candidate.ProjectID == projectID {
			matches = true
		}
		if !matches {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		if best == nil || info.ModTime().After(best.ModTime) {
			best = &discoveredSession{Path: path, SessionID: candidate.ID, ModTime: info.ModTime()}
		}
		return nil
	})

	return best
}

// findMessageDir locates the directory holding message files for a
// discovered session. OpenCode has used different layouts for message
// storage relative to session storage across releases, so several
// candidates derived from both the legacy layout and the discovered
// session's own location are tried.
func findMessageDir(dataDir, sessionFilePath, sessionID string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
	}

	if sessionFilePath != "" {
		sessionFileDir := filepath.Dir(sessionFilePath)
		candidates = append(candidates,
			filepath.Join(sessionFileDir, "message", sessionID),
			filepath.Join(filepath.Dir(sessionFileDir), "message", sessionID),
		)
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}

	return ""
}
