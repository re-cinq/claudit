<complete corrected file content>
package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// sqliteColumns returns the set of column names for a table by introspecting
// the schema at runtime, so discovery tolerates schema changes across OpenCode
// versions instead of assuming a fixed set of column names.
func sqliteColumns(dbPath, table string) map[string]bool {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	cols := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols[fields[1]] = true
		}
	}
	return cols
}

// querySessionID runs a single-column "SELECT id FROM session ..." query and
// returns the first result, or "" if the query fails or returns nothing.
func querySessionID(dbPath, whereClause, orderCol string) string {
	clause := ""
	if whereClause != "" {
		clause = "WHERE " + whereClause
	}
	query := fmt.Sprintf(`SELECT id FROM session %s ORDER BY %s DESC LIMIT 1;`, clause, orderCol)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// findRecentSessionID resolves the most recent session id for a project.
// It introspects the session table's columns so it can match on whichever
// project-identity column the installed OpenCode version actually uses
// (older releases keyed sessions by a computed project_id hash; newer
// releases have been observed to key by the literal working directory
// instead), falling back to "most recent session overall" as a last resort.
func findRecentSessionID(dbPath, projectID, projectPath string) string {
	cols := sqliteColumns(dbPath, "session")

	orderCol := "rowid"
	if cols != nil {
		for _, c := range []string{"time_updated", "updated", "time_created", "created"} {
			if cols[c] {
				orderCol = c
				break
			}
		}
	}

	var whereClauses []string
	if cols == nil || cols["directory"] {
		resolved := projectPath
		if r, err := filepath.EvalSymlinks(projectPath); err == nil {
			resolved = r
		}
		if resolved != projectPath {
			whereClauses = append(whereClauses, fmt.Sprintf("directory='%s'", resolved))
		}
		whereClauses = append(whereClauses, fmt.Sprintf("directory='%s'", projectPath))
	}
	if cols == nil || cols["project_id"] {
		whereClauses = append(whereClauses, fmt.Sprintf("project_id='%s'", projectID))
	}
	// Last resort: no project filter at all (safe when only one project's
	// sessions are recent, e.g. a single-repo CI runner).
	whereClauses = append(whereClauses, "")

	for _, where := range whereClauses {
		if id := querySessionID(dbPath, where, orderCol); id != "" {
			return id
		}
	}
	return ""
}

// fetchMessages fetches all messages for a session as a JSON array, tolerating
// the message table's JSON payload column being named either "data" (older
// releases) or "content" (observed on newer releases).
func fetchMessages(dbPath, sessionID string) []byte {
	cols := sqliteColumns(dbPath, "message")

	dataCol := "data"
	if cols != nil && !cols["data"] && cols["content"] {
		dataCol = "content"
	}

	orderCol := "rowid"
	if cols != nil {
		for _, c := range []string{"time_created", "created"} {
			if cols[c] {
				orderCol = c
				break
			}
		}
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY %s;`,
		dataCol, sessionID, orderCol,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return []byte(strings.TrimSpace(string(out)))
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID := findRecentSessionID(dbPath, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	transcriptData := fetchMessages(dbPath, sessionID)
	// sqlite3 returns "[null]" when no rows match
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}
</complete>
