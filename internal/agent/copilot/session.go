```go
package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// SessionStoreDBPath returns the path to Copilot's session-store.db SQLite
// database. Newer Copilot CLI releases (e.g. 1.0.83) no longer write a
// per-session events.jsonl transcript into session-state/<id>/; instead they
// persist transcript data in this shared database.
func SessionStoreDBPath() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-store.db"), nil
}

// readSessionStoreTranscript reads transcript data for a session from
// Copilot's session-store.db SQLite database. The exact table/column layout
// isn't guaranteed to stay stable across Copilot CLI releases, so the schema
// is discovered at query time (a table with a session-id-like column and a
// data/event/payload/content-like column) rather than hardcoded.
func readSessionStoreTranscript(sessionID string) ([]byte, error) {
	dbPath, err := SessionStoreDBPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}

	tables, err := agent.SQLiteTables(dbPath)
	if err != nil {
		return nil, err
	}

	candidateSubstrings := []string{"data", "event", "payload", "content"}

	for _, table := range tables {
		cols, err := agent.SQLiteTableColumns(dbPath, table)
		if err != nil {
			continue
		}

		sessionCol := agent.SQLiteFindColumn(cols, "session", "id")
		if sessionCol == "" {
			continue
		}

		var dataCol string
		for _, s := range candidateSubstrings {
			if dataCol = agent.SQLiteFindColumn(cols, s); dataCol != "" {
				break
			}
		}
		if dataCol == "" || dataCol == sessionCol {
			continue
		}

		query := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY rowid;`,
			dataCol, table, sessionCol, sessionID,
		)
		lines, err := agent.SQLiteQuery(dbPath, query)
		if err != nil || len(lines) == 0 {
			continue
		}

		return []byte(strings.Join(lines, "\n")), nil
	}

	return nil, fmt.Errorf("no transcript found for session %s in session-store.db", sessionID)
}
```
