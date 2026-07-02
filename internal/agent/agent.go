package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecentSessionTimeout is the default timeout for considering a session "recent"
// during session discovery across all agents.
const RecentSessionTimeout = 5 * time.Minute

// IsGitCommitCommand checks whether a shell command string represents a git commit.
func IsGitCommitCommand(command string) bool {
	return strings.Contains(command, "git commit") ||
		strings.Contains(command, "git-commit")
}

// PathsEqual compares two filesystem paths after resolving symlinks.
// Falls back to filepath.Clean comparison if symlink resolution fails.
func PathsEqual(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = filepath.Clean(a)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = filepath.Clean(b)
	}
	return ra == rb
}

// HasNestedHookCommand checks if a nested hook config (used by Claude/Gemini)
// contains a specific command string. The structure is: [{hooks: [{command: "..."}]}].
func HasNestedHookCommand(hookConfig interface{}, command string) bool {
	hookList, ok := hookConfig.([]interface{})
	if !ok {
		return false
	}
	for _, h := range hookList {
		hookMap, _ := h.(map[string]interface{})
		hookCmds, _ := hookMap["hooks"].([]interface{})
		for _, hc := range hookCmds {
			hcMap, _ := hc.(map[string]interface{})
			if cmd, ok := hcMap["command"].(string); ok {
				if strings.Contains(cmd, command) {
					return true
				}
			}
		}
	}
	return false
}

// HasFlatHookCommand checks if a flat hook config (used by Copilot)
// contains a specific command string. The structure is: [{command: "..."}].
func HasFlatHookCommand(hookConfig interface{}, command string) bool {
	hookList, ok := hookConfig.([]interface{})
	if !ok {
		return false
	}
	for _, h := range hookList {
		hookMap, _ := h.(map[string]interface{})
		if cmd, ok := hookMap["command"].(string); ok {
			if strings.Contains(cmd, command) {
				return true
			}
		}
	}
	return false
}

// NormalizeRole converts agent-specific role strings to the common MessageType.
// Handles all known role names across Claude, Codex, Copilot, Gemini, and OpenCode.
func NormalizeRole(role string) MessageType {
	switch role {
	case "user":
		return MessageTypeUser
	case "assistant", "model", "gemini":
		return MessageTypeAssistant
	case "system":
		return MessageTypeSystem
	default:
		return ""
	}
}

// ParseStandardHookInput parses the standard hook input JSON format used by
// Claude, Codex, and Gemini. The expected structure is:
//
//	{"session_id":"...", "transcript_path":"...", "tool_name":"...", "tool_input":{"command":"..."}}
func ParseStandardHookInput(raw []byte) (*HookData, error) {
	var hook struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		ToolName       string `json:"tool_name"`
		ToolInput      struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &hook); err != nil {
		return nil, err
	}
	return &HookData{
		SessionID:      hook.SessionID,
		TranscriptPath: hook.TranscriptPath,
		ToolName:       hook.ToolName,
		Command:        hook.ToolInput.Command,
	}, nil
}

// ScanDirForRecentSession scans a directory for recently modified session files
// and returns the newest one. It filters by file extension and optionally skips
// specific filenames. Used by Claude (.jsonl) and Gemini (.json).
func ScanDirForRecentSession(sessionDir, ext string, skipNames []string, projectPath string) (*SessionInfo, error) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil, nil
	}

	skip := make(map[string]bool, len(skipNames))
	for _, name := range skipNames {
		skip[name] = true
	}

	now := time.Now()
	var bestPath string
	var bestSessionID string
	var bestModTime time.Time

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ext) {
			continue
		}
		if skip[entry.Name()] {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > RecentSessionTimeout {
			continue
		}

		if bestPath == "" || modTime.After(bestModTime) {
			bestPath = filepath.Join(sessionDir, entry.Name())
			bestSessionID = strings.TrimSuffix(entry.Name(), ext)
			bestModTime = modTime
		}
	}

	if bestPath == "" {
		return nil, nil
	}

	return &SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: bestPath,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// Name identifies a coding agent CLI.
type Name string

const (
	Claude   Name = "claude"
	Codex    Name = "codex"
	Copilot  Name = "copilot"
	Gemini   Name = "gemini"
	OpenCode Name = "opencode"
)

// DiagnosticCheck represents a single doctor check result.
type DiagnosticCheck struct {
	Name    string
	OK      bool
	Message string
}

// HookData represents parsed hook input from a coding agent.
type HookData struct {
	SessionID      string
	TranscriptPath string
	ToolName       string
	Command        string
	TranscriptData []byte // inline transcript data (e.g. from OpenCode plugin SDK)
}

// SessionInfo represents a discovered active session.
type SessionInfo struct {
	SessionID      string
	TranscriptPath string
	StartedAt      string
	ProjectPath    string
	TranscriptData []byte // inline transcript data (e.g. from SQLite discovery)
}

// Agent defines the interface that each coding agent CLI must implement.
type Agent interface {
	// Identity
	Name() Name
	DisplayName() string

	// Init: configure CLI-specific hooks/plugins
	ConfigureHooks(repoRoot string) error

	// Deinit: remove CLI-specific hooks/plugins
	RemoveHooks(repoRoot string) error

	// Doctor: validate hook configuration
	DiagnoseHooks(repoRoot string) []DiagnosticCheck

	// Store: detect commit commands from hook input
	ParseHookInput(raw []byte) (*HookData, error)
	IsCommitCommand(toolName, command string) bool

	// Transcripts: parse into common format
	ParseTranscript(r io.Reader) (*Transcript, error)
	ParseTranscriptFile(path string) (*Transcript, error)

	// Sessions: discover and restore
	DiscoverSession(projectPath string) (*SessionInfo, error)
	RestoreSession(projectPath, sessionID, gitBranch string,
		transcriptData []byte, messageCount int, summary string) error
	ResumeCommand(sessionID string) (binary string, args []string)

	// Rendering: map tool names for display
	ToolAliases() map[string]string
}
