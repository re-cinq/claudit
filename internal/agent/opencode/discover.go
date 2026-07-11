```go
package opencode

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

// maxScanEntries bounds how many directory entries discoverByContentScan
// will visit, so a large OpenCode data directory (years of session history)
// doesn't make every manual commit pay for an unbounded filesystem walk.
const maxScanEntries = 50000

// sessionDirectoryKeys lists the field names OpenCode has used across
// releases to record which project directory a session belongs to.
var sessionDirectoryKeys = []string{"directory", "cwd", "worktree", "root", "project_dir", "projectDir", "projectPath", "project_path"}

// sessionIDKeys lists the field names OpenCode has used to record a
// session's own identifier inside its session/info JSON file.
var sessionIDKeys = []string{"id", "sessionID", "session_id", "sessionId"}

// messageArrayKeys lists field names under which some OpenCode releases
// embed the full message list directly inside the session file.
var messageArrayKeys = []string{"messages", "history", "log"}

// discoverByContentScan walks the OpenCode data directory looking for the
// most recently modified session file that belongs to projectPath, matching
// by the directory/cwd path recorded inside the file rather than assuming a
// specific directory naming scheme. OpenCode's on-disk layout (flat files
// keyed by a project id, nested per-project directories, embedded vs.
// separate message storage, ...) has changed across releases, so matching
// on recorded content is more resilient than hard-coding one shape.
// Returns nil if nothing recent is found.
func discoverByContentScan(dataDir, projectPath string) *agent.SessionInfo {
	info, err := os.Stat(dataDir)
	if err != nil || !info.IsDir() {
		return nil
	}

	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		absProject = filepath.Clean(projectPath)
	}

	now := time.Now()
	visited := 0

	var bestID string
	var bestPath string
	var bestModTime time.Time
	var bestEmbedded []byte

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > maxScanEntries {
			return filepath.SkipAll
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		fi, err := d.Info()
		if err != nil || now.Sub(fi.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}
		// Once we have a match, only a strictly newer file could replace it.
		if bestPath != "" && !fi.ModTime().After(bestModTime) {
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

		// Chat message entries (which carry a "role") are not session/info
		// files; skip them so a message that happens to reference a path
		// inside the project doesn't get mistaken for the session itself.
		if _, hasRole := raw["role"]; hasRole {
			return nil
		}

		dir := firstStringField(raw, sessionDirectoryKeys)
		if dir == "" || !pathsMatchProject(dir, absProject) {
			return nil
		}

		id := firstStringField(raw, sessionIDKeys)
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}
		if id == "" {
			return nil
		}

		bestID = id
		bestPath = path
		bestModTime = fi.ModTime()
		bestEmbedded = firstArrayField(raw, messageArrayKeys)
		return nil
	})

	if bestID == "" {
		return nil
	}

	session := &agent.SessionInfo{
		SessionID:   bestID,
		StartedAt:   bestModTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	if len(bestEmbedded) > 0 {
		session.TranscriptData = bestEmbedded
		return session
	}

	if msgDir := findMessageDir(dataDir, bestID); msgDir != "" {
		session.TranscriptPath = msgDir
		return session
	}

	// Fall back to the legacy expected message directory even if it wasn't
	// picked up by the scan above (e.g. its own mtime is stale).
	if legacyDir, err := GetMessageDir(bestID); err == nil {
		if _, err := os.Stat(legacyDir); err == nil {
			session.TranscriptPath = legacyDir
			return session
		}
	}

	// No separate message storage found; use the session file itself as the
	// best available evidence so a note still gets recorded.
	session.TranscriptPath = bestPath
	return session
}

// firstStringField returns the first non-empty string value found under any
// of the given keys.
func firstStringField(raw map[string]json.RawMessage, keys []string) string {
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

// firstArrayField returns the first non-empty JSON array value found under
// any of the given keys.
func firstArrayField(raw map[string]json.RawMessage, keys []string) []byte {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(string(v))
		if strings.HasPrefix(trimmed, "[") && trimmed != "[]" {
			return v
		}
	}
	return nil
}

// pathsMatchProject reports whether the directory recorded in a session
// file corresponds to projectPath, allowing for the recorded directory
// being the project root or a subdirectory of it (or vice versa).
func pathsMatchProject(sessionDir, absProject string) bool {
	abs, err := filepath.Abs(sessionDir)
	if err != nil {
		abs = filepath.Clean(sessionDir)
	}
	abs = filepath.Clean(abs)
	project := filepath.Clean(absProject)

	if abs == project {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(abs+sep, project+sep) || strings.HasPrefix(project+sep, abs+sep)
}

// findMessageDir searches the data directory for a non-empty directory
// named after sessionID, regardless of how deeply a given OpenCode release
// nests message storage.
func findMessageDir(dataDir, sessionID string) string {
	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			entries, err := os.ReadDir(path)
			if err == nil && len(entries) > 0 {
				found = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}
```
