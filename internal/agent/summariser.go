package agent

import (
	"fmt"
	"strings"
)

// Summariser is an optional interface for agents that support non-interactive
// summarisation. Agents implement this alongside the core Agent interface.
// Checked via type assertion: if s, ok := ag.(Summariser); ok { ... }
type Summariser interface {
	// SummariseCommand returns the binary name and arguments to run the agent
	// in non-interactive mode. The prompt is appended as a positional argument.
	SummariseCommand() (binary string, args []string)
}

const (
	// DefaultMaxPromptChars is the default character budget for summary prompts.
	DefaultMaxPromptChars = 50000

	summaryInstructionBase = `Summarise the following coding conversation transcript. Focus on:
1. What the user asked for
2. What was implemented or changed
3. Key decisions made
4. Any issues encountered and how they were resolved`

	summaryInstructionSuffix = `

Be concise — aim for 5-15 bullet points. Use plain text, no markdown headers.

--- TRANSCRIPT ---
`
)

// buildSummaryInstruction returns the instruction prefix, optionally including
// a focus hint that tells the LLM what to prioritise in the summary.
func buildSummaryInstruction(focus string) string {
	instruction := summaryInstructionBase
	if focus != "" {
		instruction += fmt.Sprintf("\n\nPay particular attention to: %s", focus)
	}
	instruction += summaryInstructionSuffix
	return instruction
}

// BuildSummaryPrompt constructs a prompt for summarisation from transcript entries.
// It filters out thinking blocks, tool results, tool inputs, and system messages,
// keeping user/assistant text and tool use names. Truncates from the beginning
// if the result exceeds maxChars.
func BuildSummaryPrompt(entries []TranscriptEntry, maxChars int) string {
	return BuildSummaryPromptWithFocus(entries, maxChars, "")
}

// BuildSummaryPromptWithFocus is like BuildSummaryPrompt but accepts an optional
// focus string that hints the LLM about what to prioritise in the summary.
func BuildSummaryPromptWithFocus(entries []TranscriptEntry, maxChars int, focus string) string {
	if maxChars <= 0 {
		maxChars = DefaultMaxPromptChars
	}

	var lines []string
	for _, entry := range entries {
		if entry.Type == MessageTypeSystem {
			continue
		}
		if entry.Message == nil {
			continue
		}

		role := string(entry.Type)
		for _, block := range entry.Message.Content {
			switch block.Type {
			case "text":
				if block.Text != "" {
					lines = append(lines, fmt.Sprintf("[%s] %s", role, block.Text))
				}
			case "tool_use":
				name := block.Name
				if block.Text != "" && name == "" {
					name = block.Text // Codex puts tool name in Text
				}
				if name != "" {
					lines = append(lines, fmt.Sprintf("[assistant] Used tool: %s", name))
				}
			// Skip: thinking, tool_result
			}
		}
	}

	if len(lines) == 0 {
		return ""
	}

	transcript := strings.Join(lines, "\n")

	instruction := buildSummaryInstruction(focus)

	// Budget: maxChars minus the instruction prefix
	budget := maxChars - len(instruction)
	if budget < 1000 {
		budget = 1000
	}

	if len(transcript) > budget {
		// Truncate from the beginning, keeping the most recent content
		transcript = "[... earlier conversation truncated ...]\n" + transcript[len(transcript)-budget:]
	}

	return instruction + transcript
}
