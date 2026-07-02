package gemini

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

// GetGeminiDir returns the path to Gemini's config/data directory.
func GetGeminiDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".gemini"), nil
}

// ProjectsRegistry represents Gemini's ~/.gemini/projects.json file,
// which maps project paths to slug-based directory names (v0.29+).
type ProjectsRegistry struct {
	Projects map[string]ProjectRegistryEntry `json:"projects"`
}

// ProjectRegistryEntry represents a single project in the registry.
type ProjectRegistryEntry struct {
	Slug string `json:"slug"`
}

// ReadProjectsRegistry reads and parses ~/.gemini/projects.json.
// Returns nil (no error) if the file does not exist.
func ReadProjectsRegistry() (*ProjectsRegistry, error) {
	geminiDir, err := GetGeminiDir()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filepath.Join(geminiDir, "projects.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var reg ProjectsRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("could not parse projects.json: %w", err)
	}
	return &reg, nil
}

// GetSlugForProject looks up the slug for a project path from the registry.
// Returns "" if no registry exists or the project is not registered.
func GetSlugForProject(projectPath string) string {
	reg, err := ReadProjectsRegistry()
	if err != nil || reg == nil {
		return ""
	}
	if entry, ok := reg.Projects[projectPath]; ok {
		return entry.Slug
	}
	return ""
}

// GetSessionDir returns the session directory for a project.
// Gemini v0.29+ uses slug-based paths: ~/.gemini/tmp/<slug>/chats/
// Earlier versions use SHA256 hash paths: ~/.gemini/tmp/<sha256>/chats/
// This function tries the slug first, falling back to the hash.
func GetSessionDir(projectPath string) (string, error) {
	geminiDir, err := GetGeminiDir()
	if err != nil {
		return "", err
	}

	if slug := GetSlugForProject(projectPath); slug != "" {
		return filepath.Join(geminiDir, "tmp", slug, "chats"), nil
	}

	hash := EncodeProjectPath(projectPath)
	return filepath.Join(geminiDir, "tmp", hash, "chats"), nil
}

// GetLegacySessionDir returns the SHA256-based session directory (pre-v0.29).
func GetLegacySessionDir(projectPath string) (string, error) {
	geminiDir, err := GetGeminiDir()
	if err != nil {
		return "", err
	}
	hash := EncodeProjectPath(projectPath)
	return filepath.Join(geminiDir, "tmp", hash, "chats"), nil
}

// EncodeProjectPath encodes a project path for Gemini's directory structure.
// Gemini uses a SHA256 hash of the project path.
func EncodeProjectPath(projectPath string) string {
	h := sha256.Sum256([]byte(projectPath))
	return fmt.Sprintf("%x", h)
}

// GetSessionFilePath returns the full path to a session file.
// Gemini uses session-{timestamp}-{id8}.json format.
func GetSessionFilePath(projectPath, sessionID string) (string, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(sessionDir, sessionID+".json"), nil
}

// WriteSessionFile writes a transcript to Gemini's session file location.
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

// SessionsIndex represents a sessions index for tracking Gemini sessions.
type SessionsIndex struct {
	Version int            `json:"version"`
	Entries []SessionEntry `json:"entries"`
}

// SessionEntry represents a session entry in the index.
type SessionEntry struct {
	SessionID    string `json:"sessionId"`
	FullPath     string `json:"fullPath"`
	MessageCount int    `json:"messageCount"`
	Created      string `json:"created"`
	Modified     string `json:"modified"`
	ProjectPath  string `json:"projectPath"`
}

// ReadSessionsIndex reads the sessions index for a project.
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

// WriteSessionsIndex writes the sessions index for a project.
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

// ScanAllProjectDirs scans all subdirectories of ~/.gemini/tmp/ for recent
// session files belonging to the given project. It first checks whether the
// directory name matches the project's SHA256 hash (v0.28 behaviour), then
// reads each candidate file and inspects its "projectHash" field (v0.29+
// slug-based dirs). This is a fallback used when the slug/hash directory
// lookup fails — e.g., when ~/.gemini/projects.json is absent or the project
// path key differs from what GetSlugForProject expects.
func ScanAllProjectDirs(projectPath string) (*agent.SessionInfo, error) {
	geminiDir, err := GetGeminiDir()
	if err != nil {
		return nil, nil
	}

	expectedHash := EncodeProjectPath(projectPath)
	tmpDir := filepath.Join(geminiDir, "tmp")

	dirs, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestPath string
	var bestSessionID string
	var bestModTime time.Time

	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}

		chatsDir := filepath.Join(tmpDir, dir.Name(), "chats")
		files, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}

		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") || file.Name() == "sessions-index.json" {
				continue
			}

			fileInfo, err := file.Info()
			if err != nil {
				continue
			}

			modTime := fileInfo.ModTime()
			if now.Sub(modTime) > agent.RecentSessionTimeout {
				continue
			}

			filePath := filepath.Join(chatsDir, file.Name())

			// Fast path: directory name is the project hash (v0.28 hash-based dirs).
			// Avoids reading file content for the common case.
			if dir.Name() == expectedHash {
				if bestPath == "" || modTime.After(bestModTime) {
					bestPath = filePath
					bestSessionID = strings.TrimSuffix(file.Name(), ".json")
					bestModTime = modTime
				}
				continue
			}

			// Slug-based dir (v0.29+): read the file and check the projectHash field.
			data, err := os.ReadFile(filePath)
			if err != nil {
				continue
			}

			var header struct {
				SessionID   string `json:"sessionId"`
				ProjectHash string `json:"projectHash"`
			}
			if err := json.Unmarshal(data, &header); err != nil {
				continue
			}
			if header.ProjectHash != expectedHash {
				continue
			}

			if bestPath == "" || modTime.After(bestModTime) {
				sessionID := header.SessionID
				if sessionID == "" {
					sessionID = strings.TrimSuffix(file.Name(), ".json")
				}
				bestPath = filePath
				bestSessionID = sessionID
				bestModTime = modTime
			}
		}
	}

	if bestPath == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: bestPath,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
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
