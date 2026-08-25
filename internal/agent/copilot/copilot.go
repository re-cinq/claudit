```go
package copilot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

func init() {
	agent.Register(&Agent{})
}

// Agent implements the agent.Agent interface for GitHub Copilot CLI.
type Agent struct{}

func (a *Agent) Name() agent.Name   { return agent.Copilot }
func (a *Agent) DisplayName() string { return "Copilot CLI" }

// ConfigureHooks sets up Copilot CLI hooks in .github/hooks/shiftlog.json.
func (a *Agent) ConfigureHooks(repoRoot string) error {
	hf, err := ReadHooksFile(repoRoot)
	if err != nil {
		return fmt.Errorf("failed to read Copilot hooks: %w", err)
	}

	AddShiftlogHooks(hf)

	if err := WriteHooksFile(repoRoot, hf); err != nil {
		return fmt.Errorf("failed to write Copilot hooks: %w", err)
	}
	return nil
}

// RemoveHooks removes shiftlog hooks from Copilot CLI configuration.
func (a *Agent) RemoveHooks(repoRoot string) error {
	hf, err := ReadHooksFile(repoRoot)
	if err != nil {
		return nil // no hooks file means nothing to remove
	}

	RemoveShiftlogHooks(hf)

	// If no hooks remain, delete the file (it's shiftlog-owned)
	if len(hf.Hooks) == 0 && len(hf.Other) == 0 {
		path := hooksFilePath(repoRoot)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	return WriteHooksFile(repoRoot, hf)
}

// DiagnoseHooks validates Copilot CLI hook configuration.
func (a *Agent) DiagnoseHooks(repoRoot string) []agent.DiagnosticCheck {
	var checks []agent.DiagnosticCheck

	hooksPath := hooksFilePath(repoRoot)
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "Copilot CLI hook configuration",
			OK:      false,
			Message: "No .github/hooks/shiftlog.json found. Run 'shiftlog init --agent=copilot' to configure.",
		})
		return checks
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "Copilot CLI hook configuration",
			OK:      false,
			Message: fmt.Sprintf("Invalid JSON in .github/hooks/shiftlog.json: %v", err),
		})
		return checks
	}

	hooks, hasHooks := raw["hooks"].(map[string]interface{})
	if !hasHooks {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "Copilot CLI hooks",
			OK:      false,
			Message: "Missing 'hooks' key in .github/hooks/shiftlog.json. Run 'shiftlog init --agent=copilot' to fix.",
		})
		return checks
	}

	postToolUse, hasPostToolUse := hooks["postToolUse"]
	if !hasPostToolUse || !agent.HasFlatHookCommand(postToolUse, "shiftlog store") {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "postToolUse hook",
			OK:      false,
			Message: "'shiftlog store' hook not found in postToolUse. Run 'shiftlog init --agent=copilot' to fix.",
		})
	} else {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "postToolUse hook",
			OK:      true,
			Message: "Found postToolUse hook configuration",
		})
	}

	return checks
}

// ParseHookInput parses Copilot CLI's postToolUse hook JSON.
// Supports two formats:
//   - Copilot native: {"timestamp":N, "cwd":"...", "toolName":"...", "toolArgs":{...}}
//   - Generic (shared test path): {"session_id":"...", "transcript_path":"...", "tool_name":"...", "tool_input":{"command":"..."}}
func (a *Agent) ParseHookInput(raw []byte) (*agent.HookData, error) {
	var hook struct {
		// Generic format fields
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		GenericToolName string `json:"tool_name"`
		ToolInput      struct {
			Command string `json:"command"`
		} `json:"tool_input"`

		// Copilot native format fields
		Timestamp int64           `json:"timestamp"`
		CWD       string          `json:"cwd"`
		ToolName  string          `json:"toolName"`
		ToolArgs  json.RawMessage `json:"toolArgs"`
	}
	if err := json.Unmarshal(raw, &hook); err != nil {
		return nil, err
	}

	// Determine tool name: prefer Copilot-native field, fall back to generic
	toolName := hook.ToolName
	if toolName == "" {
		toolName = hook.GenericToolName
	}

	// Determine command: prefer generic tool_input.command, fall back to native toolArgs extraction
	command := hook.ToolInput.Command
	if command == "" && len(hook.ToolArgs) > 0 {
		command = extractCommand(toolName, hook.ToolArgs)
	}

	sessionID := hook.SessionID
	transcriptPath := hook.TranscriptPath
	var transcriptData []byte

	// If no session info from generic fields, try CWD-based discovery
	if sessionID == "" && hook.CWD != "" {
		si, err := scanForRecentSession(hook.CWD)
		if err == nil && si != nil {
			sessionID = si.SessionID
			transcriptPath = si.TranscriptPath
			transcriptData = si.TranscriptData
		}
	}

	return &agent.HookData{
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
		ToolName:       toolName,
		Command:        command,
		TranscriptData: transcriptData,
	}, nil
}

// shellToolNames are the known tool names Copilot CLI uses for shell execution.
var shellToolNames = map[string]bool{
	"bash": true,
}

// IsCommitCommand checks if a tool invocation represents a git commit.
func (a *Agent) IsCommitCommand(toolName, command string) bool {
	if !shellToolNames[toolName] {
		return false
	}
	return agent.IsGitCommitCommand(command)
}

// copilotEvent represents a single event line in the events.jsonl transcript.
type copilotEvent struct {
	Type string           `json:"type"`
	Data copilotEventData `json:"data"`
}

// copilotEventData represents the data payload of a Copilot event.
type copilotEventData struct {
	// For user.message events
	Content string `json:"content,omitempty"`

	// For assistant.message events
	Message string `json:"message,omitempty"`

	// For assistant.message events with tool requests
	ToolRequests []copilotToolRequest `json:"toolRequests,omitempty"`

	// For tool.execution_complete events
	ToolUseID string          `json:"toolUseId,omitempty"`
	ToolName  string          `json:"toolName,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

// copilotToolRequest represents a tool request in an assistant message.
type copilotToolRequest struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ParseTranscript parses a Copilot CLI transcript. This may be the legacy
// events.jsonl NDJSON format, or a JSON array of turns queried from
// Copilot's session-store.db SQLite database (see queryTurnsFromStore) —
// Copilot CLI 1.0.x no longer writes an events.jsonl file per session.
func (a *Agent) ParseTranscript(r io.Reader) (*agent.Transcript, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return parseCopilotData(data)
}

// ParseTranscriptFile parses a Copilot CLI transcript from a file.
func (a *Agent) ParseTranscriptFile(path string) (*agent.Transcript, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCopilotData(data)
}

// parseCopilotData dispatches to the turns-array parser (data queried from
// session-store.db) or the legacy NDJSON events.jsonl parser, based on the
// shape of the data.
func parseCopilotData(data []byte) (*agent.Transcript, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if transcript, err := parseCopilotTurns(trimmed); err == nil {
			return transcript, nil
		}
	}
	return parseCopilotTranscript(bytes.NewReader(data))
}

// DiscoverSession finds an active or recent Copilot CLI session.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	return scanForRecentSession(projectPath)
}

// RestoreSession writes a transcript to Copilot CLI's expected location.
func (a *Agent) RestoreSession(projectPath, sessionID, gitBranch string,
	transcriptData []byte, messageCount int, summary string) error {
	_, err := WriteSessionFile(sessionID, transcriptData)
	return err
}

// ResumeCommand returns the command to resume a Copilot CLI session.
func (a *Agent) ResumeCommand(sessionID string) (string, []string) {
	return "copilot", []string{"--resume", sessionID}
}

// ToolAliases returns Copilot CLI's tool name mappings to canonical names.
func (a *Agent) ToolAliases() map[string]string {
	return map[string]string{
		"bash":          "Bash",
		"view":          "Read",
		"edit":          "Edit",
		"write":         "Write",
		"create":        "Write",
		"report_intent": "ReportIntent",
	}
}

// parseCopilotTranscript parses a Copilot CLI events.jsonl transcript.
func parseCopilotTranscript(r io.Reader) (*agent.Transcript, error) {
	scanner := bufio.NewScanner(r)
	// Increase buffer for potentially large event lines
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var entries []agent.TranscriptEntry
	var model string
	idx := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event copilotEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue // skip unparseable lines
		}

		rawBytes := []byte(line)

		switch event.Type {
		case "session.model_change":
			// Capture the model from model_change events.
			// The data.content field typically holds the model identifier.
			if event.Data.Content != "" {
				model = event.Data.Content
			}
			continue

		case "user.message":
			entries = append(entries, agent.TranscriptEntry{
				UUID: fmt.Sprintf("copilot-%d", idx),
				Type: agent.MessageTypeUser,
				Message: &agent.Message{
					Role: "user",
					Content: []agent.ContentBlock{
						{Type: "text", Text: event.Data.Content},
					},
				},
				Raw: rawBytes,
			})
			idx++

		case "assistant.message":
			var content []agent.ContentBlock

			if event.Data.Message != "" {
				content = append(content, agent.ContentBlock{
					Type: "text",
					Text: event.Data.Message,
				})
			}

			for _, tr := range event.Data.ToolRequests {
				content = append(content, agent.ContentBlock{
					Type:      "tool_use",
					ToolUseID: tr.ID,
					Name:      tr.Name,
					Input:     tr.Input,
				})
			}

			if len(content) == 0 {
				content = []agent.ContentBlock{{Type: "text", Text: ""}}
			}

			entries = append(entries, agent.TranscriptEntry{
				UUID: fmt.Sprintf("copilot-%d", idx),
				Type: agent.MessageTypeAssistant,
				Message: &agent.Message{
					Role:    "assistant",
					Content: content,
				},
				Raw: rawBytes,
			})
			idx++

		case "tool.execution_complete":
			resultStr := string(event.Data.Result)
			entries = append(entries, agent.TranscriptEntry{
				UUID: fmt.Sprintf("copilot-%d", idx),
				Type: agent.MessageTypeUser,
				Message: &agent.Message{
					Role: "user",
					Content: []agent.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: event.Data.ToolUseID,
							Content:   json.RawMessage(`"` + strings.ReplaceAll(resultStr, `"`, `\"`) + `"`),
						},
					},
				},
				Raw: rawBytes,
			})
			idx++

		default:
			// Skip session.start, assistant.turn_start/end, tool.execution_start
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read events.jsonl: %w", err)
	}

	t := &agent.Transcript{Entries: entries, Model: model}
	t.Turns = t.CountTurns()
	return t, nil
}

// copilotTurn represents a single row queried from Copilot's session-store.db
// turns table, which holds the actual conversation content — the
// session-state directory's workspace.yaml has only session metadata
// (id/cwd/branch/timestamps), no message text.
type copilotTurn struct {
	TurnIndex         int    `json:"turn_index"`
	UserMessage       string `json:"user_message"`
	AssistantResponse string `json:"assistant_response"`
	Timestamp         string `json:"timestamp"`
}

// parseCopilotTurns parses a JSON array of turns (as produced by
// queryTurnsFromStore) into the common transcript format.
func parseCopilotTurns(data []byte) (*agent.Transcript, error) {
	var turns []copilotTurn
	if err := json.Unmarshal(data, &turns); err != nil {
		return nil, err
	}

	var entries []agent.TranscriptEntry
	for _, t := range turns {
		if t.UserMessage != "" {
			entries = append(entries, agent.TranscriptEntry{
				UUID:      fmt.Sprintf("copilot-turn-%d-user", t.TurnIndex),
				Type:      agent.MessageTypeUser,
				Timestamp: t.Timestamp,
				Message: &agent.Message{
					Role:    "user",
					Content: []agent.ContentBlock{{Type: "text", Text: t.UserMessage}},
				},
			})
		}
		if t.AssistantResponse != "" {
			entries = append(entries, agent.TranscriptEntry{
				UUID:      fmt.Sprintf("copilot-turn-%d-assistant", t.TurnIndex),
				Type:      agent.MessageTypeAssistant,
				Timestamp: t.Timestamp,
				Message: &agent.Message{
					Role:    "assistant",
					Content: []agent.ContentBlock{{Type: "text", Text: t.AssistantResponse}},
				},
			})
		}
	}

	tr := &agent.Transcript{Entries: entries}
	tr.Turns = tr.CountTurns()
	return tr, nil
}

// queryTurnsFromStore queries Copilot's session-store.db (SQLite) for the
// turns belonging to a session, returning them as a JSON array of turn
// objects. Copilot CLI 1.0.x persists transcript content here rather than
// in an events.jsonl file under the session-state directory.
func queryTurnsFromStore(sessionID string) ([]byte, error) {
	dbPath, err := GetSessionStoreDBPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, fmt.Errorf("sqlite3 not available")
	}

	query := fmt.Sprintf(
		`SELECT json_group_array(json_object('turn_index', turn_index, 'user_message', user_message, 'assistant_response', assistant_response, 'timestamp', timestamp)) FROM turns WHERE session_id='%s' ORDER BY turn_index;`,
		sessionID,
	)
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	data := bytes.TrimSpace(output)
	if len(data) == 0 || string(data) == "[]" || string(data) == "[null]" {
		return nil, fmt.Errorf("no turns found for session %s", sessionID)
	}
	return data, nil
}

// extractCommand extracts the shell command from toolArgs.
// toolArgs can be a JSON object or a JSON string containing an object.
func extractCommand(toolName string, toolArgs json.RawMessage) string {
	if !shellToolNames[toolName] {
		return ""
	}
	if len(toolArgs) == 0 {
		return ""
	}

	// Try parsing as JSON object directly
	var args map[string]interface{}
	if err := json.Unmarshal(toolArgs, &args); err == nil {
		if cmd, ok := args["command"].(string); ok {
			return cmd
		}
		if cmd, ok := args["cmd"].(string); ok {
			return cmd
		}
		return ""
	}

	// Try parsing as JSON string (backwards compat: toolArgs is a JSON-escaped string)
	var argsStr string
	if err := json.Unmarshal(toolArgs, &argsStr); err == nil {
		var innerArgs map[string]interface{}
		if err := json.Unmarshal([]byte(argsStr), &innerArgs); err == nil {
			if cmd, ok := innerArgs["command"].(string); ok {
				return cmd
			}
			if cmd, ok := innerArgs["cmd"].(string); ok {
				return cmd
			}
		}
		return argsStr
	}

	return ""
}

// scanForRecentSession scans Copilot's session state directory for recent session directories.
func scanForRecentSession(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionStateDir()
	if err != nil {
		return nil, nil
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil, nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestDir string
	var bestSessionID string
	var bestModTime time.Time

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			continue
		}

		// Check if this session directory has a workspace.yaml
		entryPath := filepath.Join(sessionDir, entry.Name())
		meta, err := parseSessionMeta(entryPath)
		if err != nil || meta == nil {
			continue
		}

		if !agent.PathsEqual(meta.CWD, projectPath) {
			continue
		}

		if bestDir == "" || modTime.After(bestModTime) {
			bestDir = entryPath
			bestSessionID = meta.ID
			bestModTime = modTime
		}
	}

	if bestDir == "" {
		return nil, nil
	}

	info := &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: GetTranscriptPath(bestDir),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}

	// Copilot CLI 1.0.x persists conversation content in session-store.db
	// rather than an events.jsonl file in the session directory; prefer
	// that when available and fall back to the (likely absent) file path
	// for older CLI versions.
	if data, err := queryTurnsFromStore(bestSessionID); err == nil {
		info.TranscriptData = data
		info.TranscriptPath = ""
	}

	return info, nil
}
```
