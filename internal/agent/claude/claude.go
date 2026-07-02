package claude

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

// Agent implements the agent.Agent interface for Claude Code.
type Agent struct{}

func (a *Agent) Name() agent.Name        { return agent.Claude }
func (a *Agent) DisplayName() string      { return "Claude Code" }

// ConfigureHooks sets up Claude Code hooks in .claude/settings.local.json.
func (a *Agent) ConfigureHooks(repoRoot string) error {
	claudeDir := filepath.Join(repoRoot, ".claude")
	settings, err := ReadSettings(claudeDir)
	if err != nil {
		return fmt.Errorf("failed to read Claude settings: %w", err)
	}

	AddShiftlogHook(settings)
	AddSessionHooks(settings)

	if err := WriteSettings(claudeDir, settings); err != nil {
		return fmt.Errorf("failed to write Claude settings: %w", err)
	}
	return nil
}

// RemoveHooks removes shiftlog hooks from Claude Code settings.
func (a *Agent) RemoveHooks(repoRoot string) error {
	claudeDir := filepath.Join(repoRoot, ".claude")
	settings, err := ReadSettings(claudeDir)
	if err != nil {
		return nil // no settings file means nothing to remove
	}

	RemoveShiftlogHook(settings)
	RemoveSessionHooks(settings)

	// If only shiftlog content was present, remove the file
	if len(settings.Other) == 0 &&
		len(settings.Hooks.PostToolUse) == 0 &&
		len(settings.Hooks.SessionStart) == 0 &&
		len(settings.Hooks.SessionEnd) == 0 {
		path := filepath.Join(claudeDir, "settings.local.json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	return WriteSettings(claudeDir, settings)
}

// DiagnoseHooks validates Claude Code hook configuration.
func (a *Agent) DiagnoseHooks(repoRoot string) []agent.DiagnosticCheck {
	var checks []agent.DiagnosticCheck

	settingsPath := filepath.Join(repoRoot, ".claude", "settings.local.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "Claude Code hook configuration",
			OK:      false,
			Message: "No .claude/settings.local.json found. Run 'shiftlog init' to configure.",
		})
		return checks
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "Claude Code hook configuration",
			OK:      false,
			Message: fmt.Sprintf("Invalid JSON in settings file: %v", err),
		})
		return checks
	}

	hooks, hasHooks := settings["hooks"].(map[string]interface{})
	if !hasHooks {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "Claude Code hooks",
			OK:      false,
			Message: "Missing 'hooks' key in settings. Run 'shiftlog init' to fix.",
		})
		return checks
	}

	// Check PostToolUse
	postToolUse, hasPostToolUse := hooks["PostToolUse"]
	if !hasPostToolUse || !agent.HasNestedHookCommand(postToolUse, "shiftlog store") {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "PostToolUse hook",
			OK:      false,
			Message: "'shiftlog store' hook not found in PostToolUse. Run 'shiftlog init' to fix.",
		})
	} else {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "PostToolUse hook",
			OK:      true,
			Message: "Found PostToolUse hook configuration",
		})
	}

	// Check SessionStart
	sessionStart, hasSessionStart := hooks["SessionStart"]
	if !hasSessionStart || !agent.HasNestedHookCommand(sessionStart, "shiftlog session-start") {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "SessionStart hook",
			OK:      false,
			Message: "Missing SessionStart hook (manual commit capture won't work). Run 'shiftlog init' to add.",
		})
	} else {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "SessionStart hook",
			OK:      true,
			Message: "Found SessionStart hook",
		})
	}

	// Check SessionEnd
	sessionEnd, hasSessionEnd := hooks["SessionEnd"]
	if !hasSessionEnd || !agent.HasNestedHookCommand(sessionEnd, "shiftlog session-end") {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "SessionEnd hook",
			OK:      false,
			Message: "Missing SessionEnd hook (manual commit capture won't work). Run 'shiftlog init' to add.",
		})
	} else {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "SessionEnd hook",
			OK:      true,
			Message: "Found SessionEnd hook",
		})
	}

	return checks
}

// ParseHookInput parses Claude Code's PostToolUse hook JSON.
func (a *Agent) ParseHookInput(raw []byte) (*agent.HookData, error) {
	return agent.ParseStandardHookInput(raw)
}

// IsCommitCommand checks if a tool invocation represents a git commit.
func (a *Agent) IsCommitCommand(toolName, command string) bool {
	if toolName != "Bash" {
		return false
	}
	return agent.IsGitCommitCommand(command)
}

// ParseTranscript parses a Claude Code JSONL transcript.
func (a *Agent) ParseTranscript(r io.Reader) (*agent.Transcript, error) {
	return ParseJSONLTranscript(r)
}

// ParseTranscriptFile parses a Claude Code JSONL transcript from a file.
func (a *Agent) ParseTranscriptFile(path string) (*agent.Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ParseJSONLTranscript(f)
}

// DiscoverSession finds an active or recent Claude Code session.
// It checks (in order): active-session.json, sessions-index.json, directory scan.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try active-session.json first (written by shiftlog session-start hook)
	if info, err := discoverFromActiveSession(projectPath); err == nil && info != nil {
		return info, nil
	}

	// Try sessions-index.json
	index, err := ReadSessionsIndex(projectPath)
	if err == nil && len(index.Entries) > 0 {
		session := findRecentSession(index, projectPath)
		if session != nil {
			return session, nil
		}
	}

	// Fallback: scan for recent .jsonl files
	return scanForRecentSession(projectPath)
}

// discoverFromActiveSession checks the .shiftlog/active-session.json file
// written by the session-start hook for a direct pointer to the active session.
func discoverFromActiveSession(projectPath string) (*agent.SessionInfo, error) {
	root := projectPath
	sessionPath := filepath.Join(root, ".shiftlog", "active-session.json")

	data, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, err
	}

	var active struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		StartedAt      string `json:"started_at"`
		ProjectPath    string `json:"project_path"`
	}
	if err := json.Unmarshal(data, &active); err != nil {
		return nil, err
	}

	// Validate project path matches
	if !agent.PathsEqual(active.ProjectPath, projectPath) {
		return nil, nil
	}

	// Validate session is still active (transcript modified recently)
	if active.TranscriptPath == "" {
		return nil, nil
	}
	info, err := os.Stat(active.TranscriptPath)
	if err != nil {
		return nil, nil
	}
	const staleTimeout = 10 * time.Minute
	if time.Since(info.ModTime()) > staleTimeout {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      active.SessionID,
		TranscriptPath: active.TranscriptPath,
		StartedAt:      active.StartedAt,
		ProjectPath:    active.ProjectPath,
	}, nil
}

// RestoreSession writes a transcript to Claude Code's expected location.
func (a *Agent) RestoreSession(projectPath, sessionID, gitBranch string,
	transcriptData []byte, messageCount int, summary string) error {

	sessionPath, err := WriteSessionFile(projectPath, sessionID, transcriptData)
	if err != nil {
		return err
	}

	fileInfo, err := os.Stat(sessionPath)
	if err != nil {
		return fmt.Errorf("could not stat session file: %w", err)
	}

	index, err := ReadSessionsIndex(projectPath)
	if err != nil {
		return fmt.Errorf("could not read sessions index: %w", err)
	}

	firstPrompt := extractFirstPrompt(transcriptData)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	entry := SessionEntry{
		SessionID:    sessionID,
		FullPath:     sessionPath,
		FileMtime:    fileInfo.ModTime().UnixMilli(),
		FirstPrompt:  firstPrompt,
		Summary:      summary,
		MessageCount: messageCount,
		Created:      now,
		Modified:     now,
		GitBranch:    gitBranch,
		ProjectPath:  projectPath,
		IsSidechain:  false,
	}

	AddOrUpdateSessionEntry(index, entry)

	if err := WriteSessionsIndex(projectPath, index); err != nil {
		return fmt.Errorf("could not write sessions index: %w", err)
	}

	return nil
}

// ResumeCommand returns the command to resume a Claude Code session.
func (a *Agent) ResumeCommand(sessionID string) (string, []string) {
	return "claude", []string{"--resume", sessionID}
}

// SummariseCommand returns the command to run Claude Code in non-interactive mode.
func (a *Agent) SummariseCommand() (string, []string) {
	return "claude", []string{"-p", "--output-format", "text"}
}

// ToolAliases returns Claude Code's tool name mappings.
// Claude Code uses canonical names directly, so no aliasing needed.
func (a *Agent) ToolAliases() map[string]string {
	return nil
}

// lineMetadata holds fields extracted from each JSONL line during parsing.
type lineMetadata struct {
	Model   string `json:"model"`
	Message *struct {
		Role  string `json:"role"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ParseJSONLTranscript parses a JSONL transcript from a reader.
func ParseJSONLTranscript(r io.Reader) (*agent.Transcript, error) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	var entries []agent.TranscriptEntry
	var model string
	var usage agent.UsageMetrics

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry agent.TranscriptEntry
		_ = json.Unmarshal(line, &entry)
		entry.Raw = json.RawMessage(line)
		entries = append(entries, entry)

		// Extract model and usage from each line
		var meta lineMetadata
		if json.Unmarshal(line, &meta) == nil {
			if model == "" && meta.Model != "" {
				model = meta.Model
			}
			// Accumulate token usage from assistant message.usage
			if meta.Message != nil && meta.Message.Usage != nil {
				u := meta.Message.Usage
				usage.InputTokens += u.InputTokens
				usage.OutputTokens += u.OutputTokens
				usage.CacheCreationInputTokens += u.CacheCreationInputTokens
				usage.CacheReadInputTokens += u.CacheReadInputTokens
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	t := &agent.Transcript{Entries: entries, Model: model, Usage: usage}
	t.Turns = t.CountTurns()
	return t, nil
}

// extractFirstPrompt extracts the first user message from transcript data.
func extractFirstPrompt(transcriptData []byte) string {
	transcript, err := ParseJSONLTranscript(strings.NewReader(string(transcriptData)))
	if err != nil {
		return "No prompt"
	}

	for _, entry := range transcript.Entries {
		if entry.Type == agent.MessageTypeUser && entry.Message != nil {
			var prompt string
			for _, block := range entry.Message.Content {
				if block.Type == "text" && block.Text != "" {
					prompt = block.Text
					break
				}
			}
			if prompt == "" {
				continue
			}
			if len(prompt) > 200 {
				return prompt[:197] + "..."
			}
			return prompt
		}
	}

	return "No prompt"
}

// findRecentSession finds a recent session from the sessions-index.
func findRecentSession(index *SessionsIndex, projectPath string) *agent.SessionInfo {
	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var bestEntry *SessionEntry
	var bestModified time.Time

	for i := range index.Entries {
		entry := &index.Entries[i]

		if !agent.PathsEqual(entry.ProjectPath, projectPath) {
			continue
		}

		modified, err := time.Parse(time.RFC3339Nano, entry.Modified)
		if err != nil {
			modified, err = time.Parse(time.RFC3339, entry.Modified)
			if err != nil {
				continue
			}
		}

		if now.Sub(modified) > recentTimeout {
			continue
		}

		if bestEntry == nil || modified.After(bestModified) {
			bestEntry = entry
			bestModified = modified
		}
	}

	if bestEntry == nil {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      bestEntry.SessionID,
		TranscriptPath: bestEntry.FullPath,
		StartedAt:      bestEntry.Created,
		ProjectPath:    bestEntry.ProjectPath,
	}
}

// scanForRecentSession scans Claude's session directory for recently modified .jsonl files.
func scanForRecentSession(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil, nil
	}
	return agent.ScanDirForRecentSession(sessionDir, ".jsonl", nil, projectPath)
}


