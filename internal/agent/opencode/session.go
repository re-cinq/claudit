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

// discoverByContentScan searches the OpenCode data directory for the most
// recently modified session belonging to projectPath, without assuming a
// fixed on-disk layout. OpenCode's storage layout (flat files, per-project
// subdirectories, SQLite) has changed across releases, so matching by file
// content is more resilient than hardcoding a path shape that may no
// longer exist in newer releases.
func discoverByContentScan(dataDir, projectPath string) (*agent.SessionInfo, error) {
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		absProject = projectPath
	}
	legacyProjectID := GetProjectID(projectPath)

	now := time.Now()
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
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
		fields := sessionCandidateFields(raw)

		id := firstJSONString(fields, "id", "sessionID", "session_id")
		if id == "" {
			return nil
		}

		dir := firstJSONString(fields, "directory", "cwd", "worktree", "path", "projectPath")
		pid := firstJSONString(fields, "projectID", "project_id", "projectId")

		matched := (dir != "" && (dir == absProject || dir == projectPath)) ||
			(pid != "" && legacyProjectID != "global" && pid == legacyProjectID)
		if !matched {
			return nil
		}

		if bestSessionID == "" || info.ModTime().After(bestModTime) {
			bestSessionID = id
			bestModTime = info.ModTime()
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	transcriptPath, transcriptData := collectMessagesFor(dataDir, bestSessionID)
	if transcriptPath == "" && len(transcriptData) == 0 {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: transcriptPath,
		TranscriptData: transcriptData,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// sessionCandidateFields returns the fields to inspect for session
// metadata, unwrapping a nested "info" object if the record uses one.
func sessionCandidateFields(raw map[string]json.RawMessage) map[string]json.RawMessage {
	if infoRaw, ok := raw["info"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(infoRaw, &nested); err == nil {
			return nested
		}
	}
	return raw
}

// firstJSONString returns the first non-empty string value found among the
// given keys in raw.
func firstJSONString(raw map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}

// collectMessagesFor locates the messages for sessionID under dataDir,
// either as a same-named directory of message files (returned as
// transcriptPath for the existing directory parser) or as individual
// files scattered anywhere under dataDir that reference the session ID
// (collected inline as a JSON array in transcriptData).
func collectMessagesFor(dataDir, sessionID string) (transcriptPath string, transcriptData []byte) {
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			transcriptPath = path
			return fs.SkipAll
		}
		return nil
	})
	if transcriptPath != "" {
		return transcriptPath, nil
	}

	var messages []json.RawMessage
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
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
		if firstJSONString(raw, "sessionID", "session_id") == sessionID {
			messages = append(messages, json.RawMessage(append([]byte{}, data...)))
		}
		return nil
	})

	if len(messages) == 0 {
		return "", nil
	}
	data, err := json.Marshal(messages)
	if err != nil {
		return "", nil
	}
	return "", data
}
