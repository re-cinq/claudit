```go
package copilot

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

	// If no session info from generic fields, try CWD-based discovery
	if sessionID == "" && hook.CWD != "" {
		si, err := scanForRecentSession(hook.CWD)
		if err == nil && si != nil {
			sessionID = si.SessionID
			transcriptPath = si.TranscriptPath
		}
	}

	return &agent.HookData{
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
		ToolName:       toolName,
		Command:        command,
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

// ParseTranscript parses a Copilot CLI events.jsonl transcript.
func (a *Agent) ParseTranscript(r io.Reader) (*agent.Transcript, error) {
	return parseCopilotTranscript(r)
}

// ParseTranscriptFile parses a Copilot CLI events.jsonl transcript from a file.
func (a *Agent) ParseTranscriptFile(path string) (*agent.Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parseCopilotTranscript(f)
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

// maxSessionScanDepth bounds how many directory levels the session scan will
// descend into, in case a Copilot CLI version buckets sessions into
// subdirectories (e.g. by date) instead of storing them flat under the
// session state directory.
const maxSessionScanDepth = 3

// sessionCandidate is a discovered Copilot session directory.
type sessionCandidate struct {
	sessionDir string
	sessionID  string
	cwd        string
	modTime    time.Time
}

// scanForRecentSession scans Copilot's session state directory (or directories,
// since the directory name and layout have changed across CLI versions) for a
// recent session belonging to projectPath.
func scanForRecentSession(projectPath string) (*agent.SessionInfo, error) {
	dirs := CandidateSessionStateDirs()
	if len(dirs) == 0 {
		if def, err := GetSessionStateDir(); err == nil {
			dirs = []string{def}
		}
	}

	now := time.Now()
	var candidates []sessionCandidate
	for _, dir := range dirs {
		candidates = append(candidates, findSessionCandidates(dir, now)...)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	var best *sessionCandidate
	for i := range candidates {
		c := &candidates[i]
		if c.cwd == "" || !agent.PathsEqual(c.cwd, projectPath) {
			continue
		}
		if best == nil || c.modTime.After(best.modTime) {
			best = c
		}
	}

	// If no session's recorded CWD matches (e.g. the field was renamed or
	// removed by a newer CLI version), fall back to the single unambiguous
	// candidate rather than giving up entirely.
	if best == nil && len(candidates) == 1 {
		best = &candidates[0]
	}

	if best == nil {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      best.sessionID,
		TranscriptPath: findTranscriptFile(best.sessionDir),
		StartedAt:      best.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findSessionCandidates walks dir (up to maxSessionScanDepth levels) looking
// for session metadata files and returns one candidate per file found that is
// recent enough to be relevant.
func findSessionCandidates(dir string, now time.Time) []sessionCandidate {
	var candidates []sessionCandidate

	walkSessionDirs(dir, 0, func(sessionDir, metaPath string) {
		var modTime time.Time
		if info, err := os.Stat(metaPath); err == nil {
			modTime = info.ModTime()
		}
		if dirInfo, err := os.Stat(sessionDir); err == nil && dirInfo.ModTime().After(modTime) {
			modTime = dirInfo.ModTime()
		}
		if modTime.IsZero() || now.Sub(modTime) > agent.RecentSessionTimeout {
			return
		}

		meta, err := parseSessionMetaFile(metaPath)
		if err != nil || meta == nil || meta.ID == "" {
			return
		}

		candidates = append(candidates, sessionCandidate{
			sessionDir: sessionDir,
			sessionID:  meta.ID,
			cwd:        meta.CWD,
			modTime:    modTime,
		})
	})

	return candidates
}

// walkSessionDirs recursively visits dir, up to maxSessionScanDepth levels
// deep, invoking fn with the containing directory and path of every session
// metadata file it finds.
func walkSessionDirs(dir string, depth int, fn func(sessionDir, metaPath string)) {
	if depth > maxSessionScanDepth {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		full := filepath.Join(dir, name)

		if entry.IsDir() {
			walkSessionDirs(full, depth+1, fn)
			continue
		}

		for _, metaName := range sessionMetaFileNames {
			if name == metaName {
				fn(dir, full)
				break
			}
		}
	}
}

// findTranscriptFile returns the events transcript file within a session
// directory, tolerating filename changes across Copilot CLI versions.
func findTranscriptFile(sessionDir string) string {
	def := GetTranscriptPath(sessionDir)
	if _, err := os.Stat(def); err == nil {
		return def
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return def
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			return filepath.Join(sessionDir, entry.Name())
		}
	}
	return def
}
```
