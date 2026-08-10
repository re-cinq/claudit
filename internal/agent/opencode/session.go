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

// resolveMessageDir returns the message directory for a session. Current
// OpenCode releases nest message storage under storage/session/message/<id>;
// older releases used storage/message/<id>. Since the on-disk layout has
// changed across releases and can't be assumed, prefer whichever directory
// actually exists, defaulting to the legacy layout when neither is present.
func resolveMessageDir(dataDir, sessionID string) string {
	nested := filepath.Join(dataDir, "storage", "session", "message", sessionID)
	if info, err := os.Stat(nested); err == nil && info.IsDir() {
		return nested
	}
	return filepath.Join(dataDir, "storage", "message", sessionID)
}

// sessionCandidate represents a parsed session info file discovered while
// scanning OpenCode's session storage tree.
type sessionCandidate struct {
	id        string
	modTime   time.Time
	projectID string
	fields    map[string]json.RawMessage
}

// sessionDirectoryKeys lists the field names OpenCode has used across
// releases to record a session's working directory.
var sessionDirectoryKeys = []string{"directory", "cwd", "worktree", "path", "root", "workingDirectory"}

// matchesDirectory reports whether the candidate's recorded working directory
// matches projectPath under any of the known field names.
func (c *sessionCandidate) matchesDirectory(projectPath string) bool {
	want := filepath.Clean(projectPath)
	for _, key := range sessionDirectoryKeys {
		raw, ok := c.fields[key]
		if !ok {
			continue
		}
		var dir string
		if err := json.Unmarshal(raw, &dir); err != nil || dir == "" {
			continue
		}
		if filepath.Clean(dir) == want {
			return true
		}
	}
	return false
}

// collectSessionInfoFiles walks OpenCode's session storage tree and returns
// every session JSON file found, skipping the message/part subtrees (which
// hold per-message data rather than session metadata). This layout-agnostic
// scan is used as a fallback when the assumed project-partitioned path
// doesn't exist, since OpenCode's on-disk partitioning scheme isn't something
// shift-log can reliably predict.
func collectSessionInfoFiles(root string) ([]*sessionCandidate, error) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, nil
	}

	var candidates []*sessionCandidate

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "message" || d.Name() == "part" {
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
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		id := strings.TrimSuffix(d.Name(), ".json")
		if idRaw, ok := fields["id"]; ok {
			var parsedID string
			if err := json.Unmarshal(idRaw, &parsedID); err == nil && parsedID != "" {
				id = parsedID
			}
		}

		var projectID string
		for _, key := range []string{"projectID", "project_id", "project"} {
			if raw, ok := fields[key]; ok {
				var pid string
				if err := json.Unmarshal(raw, &pid); err == nil && pid != "" {
					projectID = pid
					break
				}
			}
		}

		candidates = append(candidates, &sessionCandidate{
			id:        id,
			modTime:   info.ModTime(),
			projectID: projectID,
			fields:    fields,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	return candidates, nil
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
```
