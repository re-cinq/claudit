package gemini

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

func init() {
	agent.Register(&Agent{})
}

// Agent implements the agent.Agent interface for Gemini CLI.
type Agent struct{}

func (a *Agent) Name() agent.Name   { return agent.Gemini }
func (a *Agent) DisplayName() string { return "Gemini CLI" }

// ConfigureHooks sets up Gemini CLI hooks in .gemini/settings.json.
func (a *Agent) ConfigureHooks(repoRoot string) error {
	geminiDir := filepath.Join(repoRoot, ".gemini")
	settings, err := ReadSettings(geminiDir)
	if err != nil {
		return fmt.Errorf("failed to read Gemini settings: %w", err)
	}

	AddShiftlogHook(settings)
	AddSessionHooks(settings)

	if err := WriteSettings(geminiDir, settings); err != nil {
		return fmt.Errorf("failed to write Gemini settings: %w", err)
	}
	return nil
}

// RemoveHooks removes shiftlog hooks from Gemini CLI settings.
func (a *Agent) RemoveHooks(repoRoot string) error {
	geminiDir := filepath.Join(repoRoot, ".gemini")
	settings, err := ReadSettings(geminiDir)
	if err != nil {
		return nil // no settings file means nothing to remove
	}

	RemoveShiftlogHook(settings)
	RemoveSessionHooks(settings)

	// If only shiftlog content was present, remove the file
	if len(settings.Other) == 0 &&
		len(settings.Hooks.AfterTool) == 0 &&
		len(settings.Hooks.SessionStart) == 0 &&
		len(settings.Hooks.SessionEnd) == 0 {
		path := filepath.Join(geminiDir, "settings.json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	return WriteSettings(geminiDir, settings)
}

// DiagnoseHooks validates Gemini CLI hook configuration.
func (a *Agent) DiagnoseHooks(repoRoot string) []agent.DiagnosticCheck {
	var checks []agent.DiagnosticCheck

	settingsPath := filepath.Join(repoRoot, ".gemini", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "Gemini CLI hook configuration",
			OK:      false,
			Message: "No .gemini/settings.json found. Run 'shiftlog init --agent=gemini' to configure.",
		})
		return checks
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "Gemini CLI hook configuration",
			OK:      false,
			Message: fmt.Sprintf("Invalid JSON in settings file: %v", err),
		})
		return checks
	}

	hooks, hasHooks := settings["hooks"].(map[string]interface{})
	if !hasHooks {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "Gemini CLI hooks",
			OK:      false,
			Message: "Missing 'hooks' key in settings. Run 'shiftlog init --agent=gemini' to fix.",
		})
		return checks
	}

	afterTool, hasAfterTool := hooks["AfterTool"]
	if !hasAfterTool || !agent.HasNestedHookCommand(afterTool, "shiftlog store") {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "AfterTool hook",
			OK:      false,
			Message: "'shiftlog store' hook not found in AfterTool. Run 'shiftlog init --agent=gemini' to fix.",
		})
	} else {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "AfterTool hook",
			OK:      true,
			Message: "Found AfterTool hook configuration",
		})
	}

	return checks
}

// ParseHookInput parses Gemini CLI's AfterTool hook JSON.
func (a *Agent) ParseHookInput(raw []byte) (*agent.HookData, error) {
	return agent.ParseStandardHookInput(raw)
}

// shellToolNames are the known tool names Gemini CLI uses for shell execution.
var shellToolNames = map[string]bool{
	"run_shell_command": true,
}

// IsCommitCommand checks if a tool invocation represents a git commit.
func (a *Agent) IsCommitCommand(toolName, command string) bool {
	if !shellToolNames[toolName] {
		return false
	}
	return agent.IsGitCommitCommand(command)
}

// ParseTranscript parses a Gemini CLI session JSON transcript.
func (a *Agent) ParseTranscript(r io.Reader) (*agent.Transcript, error) {
	return ParseGeminiTranscript(r)
}

// ParseTranscriptFile parses a Gemini CLI session JSON transcript from a file.
func (a *Agent) ParseTranscriptFile(path string) (*agent.Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ParseGeminiTranscript(f)
}

// DiscoverSession finds an active or recent Gemini CLI session.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	return scanForRecentSession(projectPath)
}

// RestoreSession writes a transcript to Gemini CLI's expected location.
func (a *Agent) RestoreSession(projectPath, sessionID, gitBranch string,
	transcriptData []byte, messageCount int, summary string) error {

	sessionPath, err := WriteSessionFile(projectPath, sessionID, transcriptData)
	if err != nil {
		return err
	}

	index, err := ReadSessionsIndex(projectPath)
	if err != nil {
		return fmt.Errorf("could not read sessions index: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	entry := SessionEntry{
		SessionID:    sessionID,
		FullPath:     sessionPath,
		MessageCount: messageCount,
		Created:      now,
		Modified:     now,
		ProjectPath:  projectPath,
	}

	AddOrUpdateSessionEntry(index, entry)

	if err := WriteSessionsIndex(projectPath, index); err != nil {
		return fmt.Errorf("could not write sessions index: %w", err)
	}

	return nil
}

// ResumeCommand returns the command to resume a Gemini CLI session.
func (a *Agent) ResumeCommand(sessionID string) (string, []string) {
	return "gemini", []string{"--resume", sessionID}
}

// ToolAliases returns Gemini CLI's tool name mappings to canonical names.
func (a *Agent) ToolAliases() map[string]string {
	return map[string]string{
		"run_shell_command": "Bash",
		"replace":          "Edit",
		"grep_search":      "Grep",
		"glob":             "Glob",
		"list_directory":   "Glob",
		"google_web_search": "WebSearch",
		"web_fetch":        "WebFetch",
	}
}

// geminiSession represents the top-level JSON structure of a Gemini CLI session file.
type geminiSession struct {
	Messages []geminiMessage `json:"messages"`
}

// geminiMessage represents a single message in a Gemini session.
type geminiMessage struct {
	Role      string           `json:"role"`
	Parts     []geminiPart     `json:"parts,omitempty"`
	ToolCalls []geminiToolCall `json:"toolCalls,omitempty"`
}

// geminiPart represents a part of a Gemini message.
type geminiPart struct {
	Text         string          `json:"text,omitempty"`
	FunctionCall *geminiFuncCall `json:"functionCall,omitempty"`
}

// geminiFuncCall represents a function call within a part.
type geminiFuncCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args,omitempty"`
}

// geminiToolCall represents a tool call in a Gemini message.
type geminiToolCall struct {
	Name  string                 `json:"name"`
	Input map[string]interface{} `json:"input,omitempty"`
}

// ParseGeminiTranscript parses a Gemini CLI session JSON transcript.
// Gemini sessions are stored as a single JSON object with a "messages" array.
func ParseGeminiTranscript(r io.Reader) (*agent.Transcript, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return &agent.Transcript{}, nil
	}

	var session geminiSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to parse Gemini session JSON: %w", err)
	}

	var entries []agent.TranscriptEntry
	for i, msg := range session.Messages {
		msgType := agent.NormalizeRole(msg.Role)
		if msgType == "" {
			continue
		}

		// Build content blocks from parts
		var content []agent.ContentBlock
		for _, part := range msg.Parts {
			if part.Text != "" {
				content = append(content, agent.ContentBlock{
					Type: "text",
					Text: part.Text,
				})
			}
			if part.FunctionCall != nil {
				inputJSON, _ := json.Marshal(part.FunctionCall.Args)
				content = append(content, agent.ContentBlock{
					Type:  "tool_use",
					Name:  part.FunctionCall.Name,
					Input: inputJSON,
				})
			}
		}

		// Also handle toolCalls field
		for _, tc := range msg.ToolCalls {
			inputJSON, _ := json.Marshal(tc.Input)
			content = append(content, agent.ContentBlock{
				Type:  "tool_use",
				Name:  tc.Name,
				Input: inputJSON,
			})
		}

		if len(content) == 0 {
			content = []agent.ContentBlock{{Type: "text", Text: ""}}
		}

		rawBytes, _ := json.Marshal(msg)

		entry := agent.TranscriptEntry{
			UUID: fmt.Sprintf("gemini-%d", i),
			Type: msgType,
			Message: &agent.Message{
				Role:    string(msgType),
				Content: content,
			},
			Raw: rawBytes,
		}

		entries = append(entries, entry)
	}

	t := &agent.Transcript{Entries: entries}
	t.Turns = t.CountTurns()
	return t, nil
}


// scanForRecentSession scans Gemini's session directory for recent files.
// It tries the primary directory (slug-based for v0.29+, hash-based otherwise),
// falls back to the legacy hash directory, and finally does a broad scan of all
// ~/.gemini/tmp/ subdirectories (handles v0.29 slug dirs when projects.json lookup fails).
func scanForRecentSession(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil, nil
	}

	skipFiles := []string{"sessions-index.json"}
	info, err := agent.ScanDirForRecentSession(sessionDir, ".json", skipFiles, projectPath)
	if info != nil || err != nil {
		return info, err
	}

	// If the primary dir was slug-based, also check the legacy hash dir.
	legacyDir, err := GetLegacySessionDir(projectPath)
	if err == nil && legacyDir != sessionDir {
		info, err = agent.ScanDirForRecentSession(legacyDir, ".json", skipFiles, projectPath)
		if info != nil || err != nil {
			return info, err
		}
	}

	// Final fallback: scan all project dirs under ~/.gemini/tmp/.
	// Handles v0.29 slug-based dirs when the projects.json lookup fails
	// (e.g., absent file, path key mismatch, or non-interactive -p mode).
	return ScanAllProjectDirs(projectPath)
}


