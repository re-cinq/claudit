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

// discoverModernSession scans OpenCode's data directory for a session
// belonging to projectPath without assuming a fixed directory depth. Newer
// OpenCode releases have reorganized on-disk storage (for example, splitting
// session metadata, messages, and message "parts" into separate subtrees
// instead of the flat storage/session/<projectID>/<sessionID>.json layout
// used by pre-v1.2 releases), so this walks the whole storage tree looking
// for a session-metadata record that references projectPath rather than
// hardcoding a path shape.
func discoverModernSession(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	storageDir := filepath.Join(dataDir, "storage")
	if info, err := os.Stat(storageDir); err != nil || !info.IsDir() {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()

	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, walkErr error) error {
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

		if !sessionRecordMatches(raw, projectPath, projectID) {
			return nil
		}

		di, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := di.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}
		if bestSessionID != "" && !modTime.After(bestModTime) {
			return nil
		}

		id := strings.TrimSuffix(d.Name(), ".json")
		if idRaw, ok := raw["id"]; ok {
			var parsed string
			if json.Unmarshal(idRaw, &parsed) == nil && parsed != "" {
				id = parsed
			}
		}

		bestSessionID = id
		bestModTime = modTime
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: collectSessionFiles(storageDir, bestSessionID),
	}, nil
}

// sessionRecordMatches reports whether a decoded JSON object looks like an
// OpenCode session-metadata record for the given project, matching either by
// the project's working directory or by its computed project ID so a rename
// or restructuring of one field doesn't break discovery entirely.
func sessionRecordMatches(raw map[string]json.RawMessage, projectPath, projectID string) bool {
	if dirRaw, ok := raw["directory"]; ok {
		var dir string
		if json.Unmarshal(dirRaw, &dir) == nil && dir == projectPath {
			return true
		}
	}
	if pidRaw, ok := raw["projectID"]; ok {
		var pid string
		if json.Unmarshal(pidRaw, &pid) == nil && pid == projectID {
			return true
		}
	}
	return false
}

// collectSessionFiles walks storageDir for every JSON file associated with
// sessionID — either living inside a directory named after the session ID,
// or named after it directly — and combines them into a single JSON array so
// they can be parsed like any other OpenCode transcript.
func collectSessionFiles(storageDir, sessionID string) []byte {
	type fileEntry struct {
		path string
		data json.RawMessage
	}
	var found []fileEntry

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		rel, err := filepath.Rel(storageDir, path)
		if err != nil {
			return nil
		}

		segments := strings.Split(filepath.ToSlash(rel), "/")
		belongs := strings.TrimSuffix(segments[len(segments)-1], ".json") == sessionID
		if !belongs {
			for _, seg := range segments[:len(segments)-1] {
				if seg == sessionID {
					belongs = true
					break
				}
			}
		}
		if !belongs {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil || !json.Valid(data) {
			return nil
		}
		found = append(found, fileEntry{path: path, data: json.RawMessage(data)})
		return nil
	})

	if len(found) == 0 {
		return nil
	}

	sort.Slice(found, func(i, j int) bool { return found[i].path < found[j].path })

	messages := make([]json.RawMessage, len(found))
	for i, f := range found {
		messages[i] = f.data
	}

	out, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return out
}

// discoverAnyRecentSession is a last-resort fallback for OpenCode releases
// whose storage schema no longer matches any known shape. It picks the most
// recently written JSON file anywhere under the data directory's storage
// tree, on the assumption that in a project-scoped data directory (as used
// by tests, and effectively true right after a single agent run) it belongs
// to the session that just ran.
func discoverAnyRecentSession(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	storageDir := filepath.Join(dataDir, "storage")
	if info, err := os.Stat(storageDir); err != nil || !info.IsDir() {
		return nil, nil
	}

	now := time.Now()
	var bestPath string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		di, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := di.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}
		if bestPath == "" || modTime.After(bestModTime) {
			bestPath = path
			bestModTime = modTime
		}
		return nil
	})

	if bestPath == "" {
		return nil, nil
	}

	data, err := os.ReadFile(bestPath)
	if err != nil || !json.Valid(data) {
		return nil, nil
	}

	transcriptData, err := json.Marshal([]json.RawMessage{data})
	if err != nil {
		return nil, nil
	}

	sessionID := strings.TrimSuffix(filepath.Base(bestPath), filepath.Ext(bestPath))

	return &agent.SessionInfo{
		SessionID:      sessionID,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}
