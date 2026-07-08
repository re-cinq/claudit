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

// scanRecord is the subset of fields used to sniff an arbitrary JSON file
// under OpenCode's data directory: is it a session-info record or a message
// record, and which project/session does it belong to. OpenCode's on-disk
// layout (path nesting, project ID scheme) has changed across releases, so
// discovery below matches on record content instead of a fixed directory
// structure.
type scanRecord struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Session   string `json:"session_id"`
	Directory string `json:"directory"`
	Path      string `json:"path"`
	Worktree  string `json:"worktree"`
	CWD       string `json:"cwd"`
}

func (r scanRecord) isMessage() bool {
	return r.Role != "" || r.Type != ""
}

func (r scanRecord) matchesProject(projectPath string) bool {
	for _, candidate := range []string{r.Directory, r.Path, r.Worktree, r.CWD} {
		if candidate != "" && agent.PathsEqual(candidate, projectPath) {
			return true
		}
	}
	return false
}

// scanForSession walks the entire OpenCode data directory looking for a
// recent session-info record, without assuming any particular directory
// nesting or project ID scheme. This is a fallback for when the known flat
// file and SQLite layouts (see discoverFromFlatFiles / discoverFromSQLite in
// opencode.go) don't match what the installed OpenCode version actually
// writes to disk.
func scanForSession(dataDir, projectPath string) (*agent.SessionInfo, error) {
	info, err := os.Stat(dataDir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var bestSessionID string
	var bestModTime time.Time
	var bestMatched bool

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		fi, err := d.Info()
		if err != nil || now.Sub(fi.ModTime()) > recentTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var rec scanRecord
		if json.Unmarshal(data, &rec) != nil || rec.ID == "" || rec.isMessage() {
			return nil
		}

		matched := rec.matchesProject(projectPath)
		modTime := fi.ModTime()

		if bestSessionID == "" || (matched && !bestMatched) || (matched == bestMatched && modTime.After(bestModTime)) {
			bestSessionID = rec.ID
			bestModTime = modTime
			bestMatched = matched
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	transcriptData := gatherMessagesForSession(dataDir, bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptData: transcriptData,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// gatherMessagesForSession walks the data directory collecting any message
// records that reference sessionID, either via an explicit session ID field
// or by living in a directory named after the session. Matching by content
// (rather than a fixed path) makes discovery resilient to layout changes.
func gatherMessagesForSession(dataDir, sessionID string) []byte {
	var messages []json.RawMessage

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var rec scanRecord
		if json.Unmarshal(data, &rec) != nil || !rec.isMessage() {
			return nil
		}

		belongsToSession := rec.SessionID == sessionID || rec.Session == sessionID ||
			filepath.Base(filepath.Dir(path)) == sessionID
		if !belongsToSession {
			return nil
		}

		messages = append(messages, json.RawMessage(append([]byte{}, data...)))
		return nil
	})

	if len(messages) == 0 {
		return nil
	}

	out, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return out
}
