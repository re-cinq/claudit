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

// FindRecentSession scans the OpenCode data directory for the most recently
// modified session that belongs to projectPath. OpenCode's on-disk storage
// layout (how deeply session/message files are nested under the data dir)
// has changed across releases, so this walks the whole storage tree rather
// than assuming a fixed directory depth. Sessions are identified by content
// (a JSON object without a "role" field, optionally with an "id" field)
// rather than by a predicted path, and matched to projectPath via any of
// the directory-like field names OpenCode has used across versions.
//
// It prefers the most recently modified session whose content matches
// projectPath; if none match by content, it falls back to the most
// recently modified session file overall (within the recency window).
func FindRecentSession(dataDir, projectPath string) (sessionID, sessionDir string, found bool) {
	storageDir := filepath.Join(dataDir, "storage")
	if _, err := os.Stat(storageDir); err != nil {
		return "", "", false
	}

	now := time.Now()
	var bestModTime time.Time
	var bestID, bestDir string
	var bestMatches bool

	_ = filepath.WalkDir(storageDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		base := strings.TrimSuffix(d.Name(), ".json")
		// Message/part records use distinct ID prefixes in OpenCode; skip
		// them so we don't mistake them for session records.
		if strings.HasPrefix(base, "msg_") || strings.HasPrefix(base, "prt_") || strings.HasPrefix(base, "part_") {
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
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil
		}
		// Message records have a "role" field; session records don't.
		if _, isMessage := raw["role"]; isMessage {
			return nil
		}

		id := base
		if idRaw, ok := raw["id"]; ok {
			var idStr string
			if json.Unmarshal(idRaw, &idStr) == nil && idStr != "" {
				id = idStr
			}
		}

		matches := sessionMatchesProject(raw, projectPath)

		better := bestID == "" ||
			(matches && !bestMatches) ||
			(matches == bestMatches && modTime.After(bestModTime))
		if better {
			bestID = id
			bestDir = filepath.Dir(path)
			bestModTime = modTime
			bestMatches = matches
		}
		return nil
	})

	if bestID == "" {
		return "", "", false
	}
	return bestID, bestDir, true
}

// sessionMatchesProject checks whether a session's JSON record references
// projectPath via any of the field names OpenCode has used across versions
// for the working directory a session was created in.
func sessionMatchesProject(raw map[string]json.RawMessage, projectPath string) bool {
	for _, key := range []string{"directory", "cwd", "path", "worktree", "projectPath", "root"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) != nil || s == "" {
			continue
		}
		if agent.PathsEqual(s, projectPath) {
			return true
		}
	}
	return false
}

// FindMessageDir locates the directory holding a session's messages. It
// tries known layouts first, then falls back to a recursive search for a
// directory literally named after the session ID, and finally falls back
// to the session's own directory (which is guaranteed to exist) so callers
// always get a valid, readable path.
func FindMessageDir(dataDir, sessionID, sessionDir string) string {
	storageDir := filepath.Join(dataDir, "storage")
	candidates := []string{
		filepath.Join(storageDir, "message", sessionID),
		filepath.Join(storageDir, "session", "message", sessionID),
		filepath.Join(sessionDir, sessionID),
		filepath.Join(sessionDir, "message", sessionID),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}

	var found string
	_ = filepath.WalkDir(storageDir, func(path string, d os.DirEntry, err error) error {
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
		return found
	}

	return sessionDir
}
</parameter>{"agents":{"claude-code":{"package":"@anthropic-ai/claude-code","constraint":"~2.1","last-known-good":"2.1.138"},"copilot":{"package":"@github/copilot","constraint":null,"last-known-good":"1.0.44"},"gemini-cli":{"package":"@google/gemini-cli","constraint":"~0.29","last-known-good":"0.29.7"},"opencode-ai":{"package":"opencode-ai","constraint":null,"last-known-good":"1.14.41"},"codex":{"package":"@openai/codex","constraint":null,"last-known-good":"0.130.0"}}}
