package copilot

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
	"gopkg.in/yaml.v3"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
type sessionMeta struct {
	ID      string `yaml:"id"`
	CWD     string `yaml:"cwd"`
	GitRoot string `yaml:"git_root,omitempty"`
}

// GetCopilotDir returns the path to Copilot's config/data directory.
func GetCopilotDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".copilot"), nil
}

// GetSessionStateDir returns the session state directory.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-state"), nil
}

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var meta sessionMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// WriteSessionFile writes a session directory structure to Copilot's session state directory.
// Creates <sessionDir>/<sessionID>/ with workspace.yaml and events.jsonl.
func WriteSessionFile(sessionID string, data []byte) (string, error) {
	stateDir, err := GetSessionStateDir()
	if err != nil {
		return "", err
	}

	sessionDir := filepath.Join(stateDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return "", fmt.Errorf("could not create session directory: %w", err)
	}

	// Write workspace.yaml
	meta := sessionMeta{ID: sessionID}
	yamlData, err := yaml.Marshal(&meta)
	if err != nil {
		return "", fmt.Errorf("could not marshal workspace.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "workspace.yaml"), yamlData, 0600); err != nil {
		return "", err
	}

	// Write events.jsonl
	eventsPath := GetTranscriptPath(sessionDir)
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}

// cwdFieldNames lists the metadata keys Copilot CLI has used (or might use in
// future releases) to record a session's working directory.
var cwdFieldNames = []string{
	"cwd", "cwd_path", "workingDirectory", "working_directory",
	"directory", "path", "workspaceRoot", "workspace_root", "root", "git_root",
}

// sessionIDFieldNames lists the metadata keys that might hold a session ID.
var sessionIDFieldNames = []string{
	"id", "sessionId", "session_id", "sessionID",
}

// sessionCandidate is a matched session metadata file found while scanning.
type sessionCandidate struct {
	path      string
	modTime   time.Time
	sessionID string
}

// scanGenericSession is a fallback session discovery strategy that does not
// assume a fixed directory layout or field names. Copilot CLI has changed its
// on-disk session metadata format across releases, so this walks Copilot's
// data directory (and, since some releases keep project-local state, the
// project's own .copilot/ directory) looking for a recently-modified
// YAML/JSON file that records a matching working directory, however it
// happens to be structured.
func scanGenericSession(projectPath string) (*agent.SessionInfo, error) {
	var best *sessionCandidate

	if copilotDir, err := GetCopilotDir(); err == nil {
		if c := scanRootForSession(copilotDir, projectPath, true); c != nil {
			best = c
		}
	}

	localDir := filepath.Join(projectPath, ".copilot")
	if c := scanRootForSession(localDir, projectPath, false); c != nil {
		if best == nil || c.modTime.After(best.modTime) {
			best = c
		}
	}

	if best == nil {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      best.sessionID,
		TranscriptPath: filepath.Dir(best.path),
		StartedAt:      best.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// scanRootForSession walks root for the most recently modified YAML/JSON file
// within agent.RecentSessionTimeout that records projectPath as its working
// directory. If requireCWDMatch is false, a file with no recognizable
// cwd-like field is still treated as a match (used for roots that are
// already scoped to the project, e.g. a project-local .copilot/ directory).
func scanRootForSession(root, projectPath string, requireCWDMatch bool) *sessionCandidate {
	if _, err := os.Stat(root); err != nil {
		return nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var best *sessionCandidate
	scanned := 0

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if scanned > 5000 {
			return filepath.SkipAll
		}
		scanned++

		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			return nil
		}
		if best != nil && !modTime.After(best.modTime) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var raw interface{}
		if ext == ".json" {
			if err := json.Unmarshal(data, &raw); err != nil {
				return nil
			}
		} else {
			if err := yaml.Unmarshal(data, &raw); err != nil {
				return nil
			}
		}

		cwd := agent.FindStringField(raw, cwdFieldNames, 3)
		matched := cwd != "" && agent.PathsEqual(cwd, projectPath)
		if !matched && requireCWDMatch {
			return nil
		}

		sessionID := agent.FindStringField(raw, sessionIDFieldNames, 3)
		if sessionID == "" {
			sessionID = filepath.Base(filepath.Dir(path))
		}

		best = &sessionCandidate{path: path, modTime: modTime, sessionID: sessionID}
		return nil
	})

	return best
}
