```go
package opencode

import (
	"encoding/json"
	"fmt"
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

// sessionCandidate is a session-like JSON file discovered by scanRecentSessions,
// along with whatever project directory it claims to belong to (if any).
type sessionCandidate struct {
	ID        string
	Directory string
	Path      string
	ModTime   time.Time
}

// firstNonEmpty returns the first non-empty string argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// scanRoots returns the directories under dataDir most likely to contain
// session storage. OpenCode has changed its on-disk layout across versions
// (e.g. moving session storage under a per-project "project/<id>/storage"
// tree instead of a flat "storage" tree at the data dir root), so we probe
// for either and fall back to scanning the whole data dir if neither exists.
func scanRoots(dataDir string) []string {
	var roots []string
	for _, sub := range []string{"storage", "project"} {
		p := filepath.Join(dataDir, sub)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			roots = append(roots, p)
		}
	}
	if len(roots) == 0 {
		roots = []string{dataDir}
	}
	return roots
}

// scanRecentSessions walks the OpenCode data directory looking for session
// metadata files modified within timeout. Rather than assuming an exact path
// shape (which has changed across OpenCode releases), it inspects the content
// of every recent JSON file: files with a "role" or "content" key are message
// files and are skipped; files with an "id" (or "sessionID") and no such keys
// are treated as session metadata. Any "directory"/"cwd"/"path"/"worktree"
// field found is used later to match the file to a project.
func scanRecentSessions(dataDir string, timeout time.Duration) []sessionCandidate {
	now := time.Now()
	var candidates []sessionCandidate

	for _, root := range scanRoots(dataDir) {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info == nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(info.Name(), ".json") {
				return nil
			}
			if now.Sub(info.ModTime()) > timeout {
				return nil
			}

			data, readErr := os.ReadFile(p)
			if readErr != nil {
				return nil
			}

			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				return nil
			}
			// Message files carry role/content; session metadata files don't.
			if _, ok := raw["role"]; ok {
				return nil
			}
			if _, ok := raw["content"]; ok {
				return nil
			}

			var fields struct {
				ID        string `json:"id"`
				SessionID string `json:"sessionID"`
				Directory string `json:"directory"`
				CWD       string `json:"cwd"`
				Path      string `json:"path"`
				Worktree  string `json:"worktree"`
			}
			if err := json.Unmarshal(data, &fields); err != nil {
				return nil
			}

			id := firstNonEmpty(fields.ID, fields.SessionID, strings.TrimSuffix(info.Name(), ".json"))
			if id == "" {
				return nil
			}

			candidates = append(candidates, sessionCandidate{
				ID:        id,
				Directory: firstNonEmpty(fields.Directory, fields.CWD, fields.Path, fields.Worktree),
				Path:      p,
				ModTime:   info.ModTime(),
			})
			return nil
		})
	}

	return candidates
}

// pickRecentSession selects the best sessionCandidate for projectPath: the
// most recently modified candidate whose recorded directory matches
// projectPath, or (if none record a matching directory — e.g. because the
// installed OpenCode version doesn't persist it) the most recently modified
// candidate overall.
func pickRecentSession(candidates []sessionCandidate, projectPath string) *sessionCandidate {
	var best *sessionCandidate
	for i := range candidates {
		c := &candidates[i]
		if c.Directory == "" || !agent.PathsEqual(c.Directory, projectPath) {
			continue
		}
		if best == nil || c.ModTime.After(best.ModTime) {
			best = c
		}
	}
	if best != nil {
		return best
	}

	for i := range candidates {
		c := &candidates[i]
		if best == nil || c.ModTime.After(best.ModTime) {
			best = c
		}
	}
	return best
}

// collectSessionMessages walks dataDir for any JSON file that lives under a
// path segment named exactly sessionID (e.g. ".../message/<sessionID>/...",
// ".../part/<sessionID>/<messageID>/...") and aggregates them into a single
// JSON array, tolerant of message/part storage moving around between
// OpenCode versions. excludePath (the session metadata file itself) is
// skipped.
func collectSessionMessages(dataDir, sessionID, excludePath string) []byte {
	if sessionID == "" {
		return nil
	}

	sep := string(filepath.Separator)
	needle := sep + sessionID + sep
	var messages []json.RawMessage

	for _, root := range scanRoots(dataDir) {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info == nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(info.Name(), ".json") && !strings.HasSuffix(info.Name(), ".jsonl") {
				return nil
			}
			if p == excludePath {
				return nil
			}
			if !strings.Contains(sep+p+sep, needle) && strings.TrimSuffix(info.Name(), filepath.Ext(info.Name())) != sessionID {
				return nil
			}

			data, readErr := os.ReadFile(p)
			if readErr != nil {
				return nil
			}

			if strings.HasSuffix(info.Name(), ".jsonl") {
				for _, line := range strings.Split(string(data), "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					messages = append(messages, json.RawMessage(line))
				}
				return nil
			}

			messages = append(messages, json.RawMessage(data))
			return nil
		})
	}

	if len(messages) == 0 {
		return nil
	}

	out, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return out
}
```
