package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SessionEntry represents an entry in Claude's sessions-index.json.
type SessionEntry struct {
	SessionID    string `json:"sessionId"`
	FullPath     string `json:"fullPath"`
	FileMtime    int64  `json:"fileMtime"`
	FirstPrompt  string `json:"firstPrompt"`
	Summary      string `json:"summary"`
	MessageCount int    `json:"messageCount"`
	Created      string `json:"created"`
	Modified     string `json:"modified"`
	GitBranch    string `json:"gitBranch"`
	ProjectPath  string `json:"projectPath"`
	IsSidechain  bool   `json:"isSidechain"`
}

// SessionsIndex represents Claude's sessions-index.json structure.
type SessionsIndex struct {
	Version int            `json:"version"`
	Entries []SessionEntry `json:"entries"`
}

// EncodeProjectPath converts an absolute path to Claude's encoded format.
func EncodeProjectPath(projectPath string) string {
	encoded := strings.ReplaceAll(projectPath, string(filepath.Separator), "-")
	encoded = strings.ReplaceAll(encoded, "/", "-")
	if !strings.HasPrefix(encoded, "-") {
		encoded = "-" + encoded
	}
	return encoded
}

// GetClaudeProjectsDir returns the path to Claude's projects directory.
func GetClaudeProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// GetSessionDir returns the full path to a project's session directory.
func GetSessionDir(projectPath string) (string, error) {
	projectsDir, err := GetClaudeProjectsDir()
	if err != nil {
		return "", err
	}
	encoded := EncodeProjectPath(projectPath)
	return filepath.Join(projectsDir, encoded), nil
}

// GetSessionFilePath returns the full path to a session's JSONL file.
func GetSessionFilePath(projectPath, sessionID string) (string, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(sessionDir, sessionID+".jsonl"), nil
}

// WriteSessionFile writes a transcript to Claude's session file location.
func WriteSessionFile(projectPath, sessionID string, transcriptData []byte) (string, error) {
	sessionPath, err := GetSessionFilePath(projectPath, sessionID)
	if err != nil {
		return "", err
	}

	sessionDir := filepath.Dir(sessionPath)
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return "", fmt.Errorf("could not create session directory: %w", err)
	}

	if err := os.WriteFile(sessionPath, transcriptData, 0600); err != nil {
		return "", fmt.Errorf("could not write session file: %w", err)
	}

	return sessionPath, nil
}

// ReadSessionsIndex reads the sessions-index.json for a project.
func ReadSessionsIndex(projectPath string) (*SessionsIndex, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil, err
	}

	indexPath := filepath.Join(sessionDir, "sessions-index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &SessionsIndex{Version: 1, Entries: []SessionEntry{}}, nil
		}
		return nil, err
	}

	var index SessionsIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("could not parse sessions-index.json: %w", err)
	}

	return &index, nil
}

// WriteSessionsIndex writes the sessions-index.json for a project.
func WriteSessionsIndex(projectPath string, index *SessionsIndex) error {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return fmt.Errorf("could not create session directory: %w", err)
	}

	indexPath := filepath.Join(sessionDir, "sessions-index.json")
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal sessions-index.json: %w", err)
	}

	return os.WriteFile(indexPath, data, 0600)
}

// AddOrUpdateSessionEntry adds or updates a session entry in the index.
func AddOrUpdateSessionEntry(index *SessionsIndex, entry SessionEntry) {
	for i, e := range index.Entries {
		if e.SessionID == entry.SessionID {
			index.Entries[i] = entry
			return
		}
	}
	index.Entries = append(index.Entries, entry)
}
