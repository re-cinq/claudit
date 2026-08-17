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

// SessionCandidate is a session file discovered on disk, with best-effort
// metadata extracted from its JSON content.
type SessionCandidate struct {
	ID        string
	Directory string // best-effort project directory hint, if present in the file
	ModTime   time.Time
	Path      string
}

// FindSessionCandidates walks OpenCode's session storage tree looking for
// session JSON files modified more recently than cutoff.
//
// OpenCode's on-disk layout has changed across releases (a flat
// storage/session/<projectID>/<id>.json layout in older versions, a shared
// storage/session/info/<id>.json layout in newer ones, etc), so this walks
// the whole storage/session tree rather than assuming one fixed shape.
func FindSessionCandidates(dataDir string, cutoff time.Time) []SessionCandidate {
	sessionRoot := filepath.Join(dataDir, "storage", "session")

	var candidates []SessionCandidate
	_ = filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}

		id := strings.TrimSuffix(d.Name(), ".json")

		var meta struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectID"`
			Directory string `json:"directory"`
			Path      string `json:"path"`
			Cwd       string `json:"cwd"`
			Worktree  string `json:"worktree"`
		}
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &meta)
		}
		if meta.ID != "" {
			id = meta.ID
		}

		dir := meta.Directory
		if dir == "" {
			dir = meta.Path
		}
		if dir == "" {
			dir = meta.Cwd
		}
		if dir == "" {
			dir = meta.Worktree
		}

		candidates = append(candidates, SessionCandidate{
			ID:        id,
			Directory: dir,
			ModTime:   info.ModTime(),
			Path:      path,
		})
		return nil
	})

	return candidates
}

// FindMessageDir locates the message storage directory for a session,
// trying the directory layouts used by different OpenCode versions. It
// returns false if no non-empty message directory could be found.
func FindMessageDir(dataDir, sessionID string) (string, bool) {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", sessionID, "message"),
	}

	for _, dir := range candidates {
		entries, err := os.ReadDir(dir)
		if err == nil && len(entries) > 0 {
			return dir, true
		}
	}

	return "", false
}
