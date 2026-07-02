package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

// SessionMeta represents the first line of a Codex rollout JSONL file.
type SessionMeta struct {
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	CLIVersion    string `json:"cli_version"`
	ModelProvider string `json:"model_provider"`
}

// GetCodexHome returns the Codex home directory.
// Respects $CODEX_HOME, defaulting to ~/.codex.
func GetCodexHome() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// GetSessionsDir returns the Codex sessions directory.
func GetSessionsDir() (string, error) {
	codexHome, err := GetCodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(codexHome, "sessions"), nil
}

// ParseSessionMeta reads the first line of a rollout JSONL file
// and extracts the session_meta payload.
func ParseSessionMeta(path string) (*SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return nil, scanner.Err()
	}

	var envelope struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
		return nil, err
	}
	if envelope.Type != "session_meta" {
		return nil, nil
	}

	var meta SessionMeta
	if err := json.Unmarshal(envelope.Payload, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// FindRecentRollout scans the sessions directory for a rollout file
// matching the given project path, modified within the timeout window.
func FindRecentRollout(projectPath string, timeout time.Duration) (path string, sessionID string, err error) {
	sessionsDir, err := GetSessionsDir()
	if err != nil {
		return "", "", err
	}

	now := time.Now()
	var bestPath string
	var bestID string
	var bestModTime time.Time

	_ = filepath.Walk(sessionsDir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		if now.Sub(info.ModTime()) > timeout {
			return nil
		}

		meta, err := ParseSessionMeta(p)
		if err != nil || meta == nil {
			return nil
		}

		if !agent.PathsEqual(meta.CWD, projectPath) {
			return nil
		}

		if bestPath == "" || info.ModTime().After(bestModTime) {
			bestPath = p
			bestID = meta.ID
			bestModTime = info.ModTime()
		}
		return nil
	})

	return bestPath, bestID, nil
}

// WriteSessionFile writes transcript data to the Codex sessions directory,
// organized by date as Codex expects.
func WriteSessionFile(sessionID string, data []byte) (string, error) {
	sessionsDir, err := GetSessionsDir()
	if err != nil {
		return "", err
	}

	now := time.Now()
	dateDir := filepath.Join(sessionsDir, now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dateDir, 0755); err != nil {
		return "", err
	}

	filename := "rollout-" + now.Format("20060102T150405") + "-" + sessionID + ".jsonl"
	path := filepath.Join(dateDir, filename)
	return path, os.WriteFile(path, data, 0644)
}

