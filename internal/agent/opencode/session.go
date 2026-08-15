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

// sessionCandidate is a session info file discovered by walking OpenCode's
// data directory, used when the fixed storage/session/<projectID> layout
// doesn't match the installed OpenCode version.
type sessionCandidate struct {
	ID        string
	Directory string
	ProjectID string
	ModTime   time.Time
}

// maxCandidateFileSize bounds how large a JSON file we'll parse while
// scanning for session info files, so we don't read large message/part
// payloads while walking the data directory.
const maxCandidateFileSize = 256 * 1024

// findSessionCandidates walks the OpenCode data directory looking for
// session info JSON files, wherever OpenCode happens to nest them on disk.
// A file is treated as session info if it parses into a sessionInfo with a
// non-empty ID and either a Directory or ProjectID field — message and part
// files don't carry those fields.
func findSessionCandidates(dataDir string) []sessionCandidate {
	var candidates []sessionCandidate

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil || info.Size() > maxCandidateFileSize {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var si sessionInfo
		if err := json.Unmarshal(data, &si); err != nil || si.ID == "" {
			return nil
		}
		if si.Directory == "" && si.ProjectID == "" {
			return nil
		}

		candidates = append(candidates, sessionCandidate{
			ID:        si.ID,
			Directory: si.Directory,
			ProjectID: si.ProjectID,
			ModTime:   info.ModTime(),
		})
		return nil
	})

	return candidates
}

// findSessionMessages searches the data directory for message JSON files
// belonging to sessionID and returns them combined as a single JSON array,
// regardless of how OpenCode nests them on disk. Newer layouts group
// messages in a directory named after the session (e.g.
// storage/session/message/<sessionID>/<messageID>.json); this also matches
// layouts that instead tag each message with a "sessionID"/"session_id"
// field rather than nesting by directory.
func findSessionMessages(dataDir, sessionID string) []byte {
	var raws []json.RawMessage

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		inSessionDir := filepath.Base(filepath.Dir(path)) == sessionID

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		if !inSessionDir {
			var tag struct {
				SessionID     string `json:"sessionID"`
				SessionIDSnak string `json:"session_id"`
			}
			if err := json.Unmarshal(data, &tag); err != nil {
				return nil
			}
			if tag.SessionID != sessionID && tag.SessionIDSnak != sessionID {
				return nil
			}
		}

		raws = append(raws, json.RawMessage(append([]byte{}, data...)))
		return nil
	})

	if len(raws) == 0 {
		return nil
	}

	combined, err := json.Marshal(raws)
	if err != nil {
		return nil
	}
	return combined
}

// discoverByWalk performs a structure-agnostic search of OpenCode's data
// directory for the most recent session belonging to projectPath. OpenCode
// has changed how it nests session/message files on disk across releases,
// so rather than assuming a fixed path scheme this walks the whole data
// directory and matches session info files by their embedded
// "directory"/"projectID" fields.
func discoverByWalk(dataDir, projectPath string) *agent.SessionInfo {
	candidates := findSessionCandidates(dataDir)
	if len(candidates) == 0 {
		return nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()

	var best *sessionCandidate
	for i := range candidates {
		c := &candidates[i]

		matches := (c.Directory != "" && agent.PathsEqual(c.Directory, projectPath)) ||
			(c.Directory == "" && c.ProjectID != "" && c.ProjectID == projectID)
		if !matches {
			continue
		}
		if now.Sub(c.ModTime) > agent.RecentSessionTimeout {
			continue
		}
		if best == nil || c.ModTime.After(best.ModTime) {
			best = c
		}
	}

	if best == nil {
		return nil
	}

	transcriptData := findSessionMessages(dataDir, best.ID)
	if transcriptData == nil {
		transcriptData = []byte("[]")
	}

	return &agent.SessionInfo{
		SessionID:      best.ID,
		StartedAt:      best.ModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}
}
```
