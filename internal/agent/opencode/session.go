package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

// sessionRecord captures the fields OpenCode's session-info JSON may use to
// identify itself and its working directory. Field names are checked in
// priority order since OpenCode has used different names across releases.
type sessionRecord struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Path      string `json:"path"`
	Cwd       string `json:"cwd"`
	Worktree  string `json:"worktree"`
}

func (r sessionRecord) projectDir() string {
	for _, v := range []string{r.Directory, r.Path, r.Cwd, r.Worktree} {
		if v != "" {
			return v
		}
	}
	return ""
}

// DiscoverByScanning walks the entire OpenCode storage tree looking for the
// most recently modified session record belonging to projectPath, without
// assuming a specific directory nesting scheme. OpenCode has repeatedly
// reorganized its on-disk session layout across releases (flat per-project
// folders, session/info subdirectories, etc.), so GetSessionDir's
// project-ID-folder scheme can silently stop matching reality after an
// upgrade. This is a resilient fallback: it identifies session records by
// content (an "id" that looks like a session ID, plus a working-directory
// field) rather than by a fixed path shape.
func DiscoverByScanning(dataDir, projectPath string) (*agent.SessionInfo, error) {
	storageDir := filepath.Join(dataDir, "storage")
	if info, err := os.Stat(storageDir); err != nil || !info.IsDir() {
		return nil, nil
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time

	_ = filepath.Walk(storageDir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		var rec sessionRecord
		if err := json.Unmarshal(data, &rec); err != nil || rec.ID == "" {
			return nil
		}
		// Session IDs are distinguished from message/part IDs by a "ses"
		// prefix. Skip anything else so we don't mistake a message file for
		// a session record.
		if !strings.HasPrefix(rec.ID, "ses") {
			return nil
		}
		if !agent.PathsEqual(rec.projectDir(), projectPath) {
			return nil
		}

		if bestID == "" || info.ModTime().After(bestModTime) {
			bestID = rec.ID
			bestModTime = info.ModTime()
		}
		return nil
	})

	if bestID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: collectSessionMessages(storageDir, bestID),
	}, nil
}

// collectSessionMessages gathers every message/part JSON file referencing
// sessionID anywhere under storageDir into a single JSON array, ordered by
// file modification time. It matches either an explicit "sessionID" field
// inside the file or the file living in a directory named after the
// session, so it tolerates message storage being nested differently than
// session storage across OpenCode releases.
func collectSessionMessages(storageDir, sessionID string) []byte {
	type match struct {
		modTime time.Time
		data    json.RawMessage
	}
	var matches []match

	_ = filepath.Walk(storageDir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		var meta struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil
		}
		if meta.ID == sessionID {
			// This is the session record itself, not one of its messages.
			return nil
		}
		if meta.SessionID != sessionID && filepath.Base(filepath.Dir(p)) != sessionID {
			return nil
		}

		matches = append(matches, match{
			modTime: info.ModTime(),
			data:    json.RawMessage(append([]byte{}, data...)),
		})
		return nil
	})

	if len(matches) == 0 {
		return nil
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].modTime.Before(matches[j].modTime) })

	raws := make([]json.RawMessage, len(matches))
	for i, m := range matches {
		raws[i] = m.data
	}

	out, err := json.Marshal(raws)
	if err != nil {
		return nil
	}
	return out
}
