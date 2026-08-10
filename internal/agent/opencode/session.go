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

// discoverByWalkingStorage searches the entire OpenCode storage directory for
// the most recently modified session, without assuming a fixed on-disk shape.
// OpenCode's storage layout has changed across releases (flat files directly
// under storage/session/<projectID>, nested storage/session/info/<id> with
// storage/session/message/<id>/<messageID> siblings, etc.), so rather than
// hard-coding one shape this matches files by content: a JSON object with an
// "id" but no back-reference to a parent (role, sessionID, messageID, ...) is
// treated as session info, and an object with a "role" is treated as a
// message belonging to whichever session it references (either via a
// session-id field or by living under a directory segment named after the
// session ID).
func discoverByWalkingStorage(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	storageDir := filepath.Join(dataDir, "storage")
	if fi, err := os.Stat(storageDir); err != nil || !fi.IsDir() {
		return nil, nil
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time
	bestScore := -1

	_ = filepath.WalkDir(storageDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		fi, err := d.Info()
		if err != nil || now.Sub(fi.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil
		}

		// Messages/parts carry a back-reference to their parent; session
		// info objects are the root and don't.
		if isChildRecord(raw) {
			return nil
		}

		id := stringField(raw, "id")
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}
		if id == "" {
			return nil
		}

		score := sessionProjectScore(raw, projectPath)
		if score < 0 {
			return nil // explicitly belongs to a different project
		}

		if score > bestScore || (score == bestScore && fi.ModTime().After(bestModTime)) {
			bestScore = score
			bestID = id
			bestModTime = fi.ModTime()
		}
		return nil
	})

	if bestID == "" {
		return nil, nil
	}

	transcriptData, _ := collectSessionMessages(storageDir, bestID)

	info := &agent.SessionInfo{
		SessionID:   bestID,
		StartedAt:   bestModTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}
	if len(transcriptData) > 0 {
		info.TranscriptData = transcriptData
	} else {
		msgDir, _ := GetMessageDir(bestID)
		info.TranscriptPath = msgDir
	}
	return info, nil
}

// isChildRecord reports whether a JSON object carries a back-reference to a
// parent record (a message or content part), which disqualifies it from
// being treated as top-level session info.
func isChildRecord(raw map[string]json.RawMessage) bool {
	for _, f := range []string{"role", "sessionID", "session_id", "sessionId", "messageID", "message_id", "parentID"} {
		if _, ok := raw[f]; ok {
			return true
		}
	}
	return false
}

// sessionProjectScore rates how well a session-info object matches projectPath:
// 2 = an explicit path-like field matches, 1 = no path-like field found (unknown),
// -1 = an explicit path-like field points at a different project.
func sessionProjectScore(raw map[string]json.RawMessage, projectPath string) int {
	fields := []string{
		"directory", "cwd", "path", "worktree", "root",
		"projectPath", "project_path", "projectDir", "project_dir",
	}
	found := false
	for _, f := range fields {
		v, ok := raw[f]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			continue
		}
		found = true
		if s == projectPath || agent.PathsEqual(s, projectPath) {
			return 2
		}
	}
	if found {
		return -1
	}
	return 1
}

// stringField extracts a string value from a raw JSON object, returning ""
// if the key is absent or not a string.
func stringField(raw map[string]json.RawMessage, key string) string {
	v, ok := raw[key]
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

// collectSessionMessages walks the storage tree collecting message objects
// that belong to sessionID, returning them as a JSON array ordered by time.
func collectSessionMessages(storageDir, sessionID string) ([]byte, error) {
	type timedMessage struct {
		raw json.RawMessage
		ts  int64
	}
	var messages []timedMessage

	consider := func(p string, data []byte) {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return
		}
		if _, isMessage := raw["role"]; !isMessage {
			return
		}
		if !messageBelongsToSession(raw, sessionID, p) {
			return
		}
		messages = append(messages, timedMessage{
			raw: json.RawMessage(append([]byte{}, data...)),
			ts:  messageTimeValue(raw),
		})
	}

	_ = filepath.WalkDir(storageDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch {
		case strings.HasSuffix(d.Name(), ".json"):
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			consider(p, data)
		case strings.HasSuffix(d.Name(), ".jsonl"):
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				consider(p, []byte(line))
			}
		}
		return nil
	})

	if len(messages) == 0 {
		return nil, nil
	}

	sort.SliceStable(messages, func(i, j int) bool {
		if messages[i].ts == 0 || messages[j].ts == 0 {
			return false
		}
		return messages[i].ts < messages[j].ts
	})

	arr := make([]json.RawMessage, len(messages))
	for i, m := range messages {
		arr[i] = m.raw
	}
	return json.Marshal(arr)
}

// messageBelongsToSession checks if a message object references sessionID,
// either via a session-id field or by living under a directory segment
// named after the session.
func messageBelongsToSession(raw map[string]json.RawMessage, sessionID, path string) bool {
	for _, f := range []string{"sessionID", "session_id", "sessionId", "parentID"} {
		if v, ok := raw[f]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s == sessionID {
				return true
			}
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if seg == sessionID {
			return true
		}
	}
	return false
}

// messageTimeValue extracts a sortable timestamp from a message object,
// checking common field shapes across OpenCode versions. Returns 0 if no
// usable timestamp is found.
func messageTimeValue(raw map[string]json.RawMessage) int64 {
	if v, ok := raw["time"]; ok {
		var t struct {
			Created float64 `json:"created"`
		}
		if json.Unmarshal(v, &t) == nil && t.Created > 0 {
			return int64(t.Created)
		}
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return parsed.UnixNano()
			}
		}
	}
	for _, f := range []string{"created", "timestamp"} {
		v, ok := raw[f]
		if !ok {
			continue
		}
		var n float64
		if json.Unmarshal(v, &n) == nil && n > 0 {
			return int64(n)
		}
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return parsed.UnixNano()
			}
		}
	}
	return 0
}
```
