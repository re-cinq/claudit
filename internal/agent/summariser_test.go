package agent

import (
	"strings"
	"testing"
)

func TestBuildSummaryPrompt_Basic(t *testing.T) {
	entries := []TranscriptEntry{
		{
			Type: MessageTypeUser,
			Message: &Message{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Help me refactor the auth module"},
				},
			},
		},
		{
			Type: MessageTypeAssistant,
			Message: &Message{
				Role: "assistant",
				Content: []ContentBlock{
					{Type: "text", Text: "I'll help you refactor the auth module."},
					{Type: "tool_use", Name: "Read"},
				},
			},
		},
	}

	prompt := BuildSummaryPrompt(entries, DefaultMaxPromptChars)

	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	if !strings.Contains(prompt, "Help me refactor the auth module") {
		t.Error("prompt should contain user message")
	}
	if !strings.Contains(prompt, "I'll help you refactor the auth module.") {
		t.Error("prompt should contain assistant text")
	}
	if !strings.Contains(prompt, "Used tool: Read") {
		t.Error("prompt should contain tool use name")
	}
	if !strings.Contains(prompt, "Summarise") {
		t.Error("prompt should contain instruction prefix")
	}
}

func TestBuildSummaryPrompt_FiltersSystemMessages(t *testing.T) {
	entries := []TranscriptEntry{
		{
			Type: MessageTypeSystem,
			Message: &Message{
				Role: "system",
				Content: []ContentBlock{
					{Type: "text", Text: "You are a helpful assistant"},
				},
			},
		},
		{
			Type: MessageTypeUser,
			Message: &Message{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
		},
	}

	prompt := BuildSummaryPrompt(entries, DefaultMaxPromptChars)

	if strings.Contains(prompt, "You are a helpful assistant") {
		t.Error("prompt should not contain system messages")
	}
	if !strings.Contains(prompt, "Hello") {
		t.Error("prompt should contain user message")
	}
}

func TestBuildSummaryPrompt_FiltersThinkingAndToolResults(t *testing.T) {
	entries := []TranscriptEntry{
		{
			Type: MessageTypeAssistant,
			Message: &Message{
				Role: "assistant",
				Content: []ContentBlock{
					{Type: "thinking", Thinking: "Let me think about this..."},
					{Type: "text", Text: "Here's my answer."},
					{Type: "tool_use", Name: "Bash"},
				},
			},
		},
		{
			Type: MessageTypeUser,
			Message: &Message{
				Role: "user",
				Content: []ContentBlock{
					{Type: "tool_result", ToolUseID: "tool-1"},
				},
			},
		},
	}

	prompt := BuildSummaryPrompt(entries, DefaultMaxPromptChars)

	if strings.Contains(prompt, "Let me think about this") {
		t.Error("prompt should not contain thinking blocks")
	}
	if !strings.Contains(prompt, "Here's my answer.") {
		t.Error("prompt should contain assistant text")
	}
	if !strings.Contains(prompt, "Used tool: Bash") {
		t.Error("prompt should contain tool use name")
	}
	// tool_result entries produce no output since they have no text
	if strings.Contains(prompt, "tool_result") {
		t.Error("prompt should not contain tool_result type")
	}
}

func TestBuildSummaryPrompt_TruncatesFromBeginning(t *testing.T) {
	// Create entries where the first is long and the second is short,
	// so truncation drops the first entry's content but keeps the second.
	entries := []TranscriptEntry{
		{
			Type: MessageTypeUser,
			Message: &Message{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "FIRST_MESSAGE " + strings.Repeat("a", 5000)},
				},
			},
		},
		{
			Type: MessageTypeAssistant,
			Message: &Message{
				Role: "assistant",
				Content: []ContentBlock{
					{Type: "text", Text: "LAST_MESSAGE"},
				},
			},
		},
	}

	prompt := BuildSummaryPrompt(entries, 2000)

	if !strings.Contains(prompt, "truncated") {
		t.Error("prompt should indicate truncation")
	}
	// The most recent content (short second entry) should be preserved
	if !strings.Contains(prompt, "LAST_MESSAGE") {
		t.Error("prompt should preserve most recent content")
	}
	// The beginning of the long first message should be dropped
	if strings.Contains(prompt, "FIRST_MESSAGE") {
		t.Error("prompt should have dropped the beginning of the conversation")
	}
}

func TestBuildSummaryPrompt_EmptyEntries(t *testing.T) {
	prompt := BuildSummaryPrompt(nil, DefaultMaxPromptChars)
	if prompt != "" {
		t.Errorf("expected empty prompt for nil entries, got %q", prompt)
	}

	prompt = BuildSummaryPrompt([]TranscriptEntry{}, DefaultMaxPromptChars)
	if prompt != "" {
		t.Errorf("expected empty prompt for empty entries, got %q", prompt)
	}
}

func TestBuildSummaryPrompt_OnlySystemMessages(t *testing.T) {
	entries := []TranscriptEntry{
		{
			Type: MessageTypeSystem,
			Message: &Message{
				Role: "system",
				Content: []ContentBlock{
					{Type: "text", Text: "System prompt"},
				},
			},
		},
	}

	prompt := BuildSummaryPrompt(entries, DefaultMaxPromptChars)
	if prompt != "" {
		t.Errorf("expected empty prompt for only system messages, got %q", prompt)
	}
}

func TestBuildSummaryPrompt_CodexToolUseInText(t *testing.T) {
	// Codex puts tool name in Text field instead of Name field
	entries := []TranscriptEntry{
		{
			Type: MessageTypeAssistant,
			Message: &Message{
				Role: "assistant",
				Content: []ContentBlock{
					{Type: "tool_use", Text: "shell"},
				},
			},
		},
	}

	prompt := BuildSummaryPrompt(entries, DefaultMaxPromptChars)
	if !strings.Contains(prompt, "Used tool: shell") {
		t.Error("prompt should handle Codex tool names in Text field")
	}
}

func TestBuildSummaryPromptWithFocus(t *testing.T) {
	entries := []TranscriptEntry{
		{
			Type: MessageTypeUser,
			Message: &Message{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Refactor the auth module and fix the SQL injection"},
				},
			},
		},
		{
			Type: MessageTypeAssistant,
			Message: &Message{
				Role: "assistant",
				Content: []ContentBlock{
					{Type: "text", Text: "I'll fix the security issues first."},
				},
			},
		},
	}

	prompt := BuildSummaryPromptWithFocus(entries, DefaultMaxPromptChars, "security changes")

	if !strings.Contains(prompt, "Pay particular attention to: security changes") {
		t.Error("prompt should contain the focus hint")
	}
	if !strings.Contains(prompt, "Refactor the auth module") {
		t.Error("prompt should still contain the transcript")
	}
	if !strings.Contains(prompt, "Summarise") {
		t.Error("prompt should still contain the base instruction")
	}
}

func TestBuildSummaryPromptWithFocus_EmptyFocus(t *testing.T) {
	entries := []TranscriptEntry{
		{
			Type: MessageTypeUser,
			Message: &Message{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
		},
	}

	withFocus := BuildSummaryPromptWithFocus(entries, DefaultMaxPromptChars, "")
	withoutFocus := BuildSummaryPrompt(entries, DefaultMaxPromptChars)

	if withFocus != withoutFocus {
		t.Error("empty focus should produce the same prompt as BuildSummaryPrompt")
	}
	if strings.Contains(withFocus, "Pay particular attention to") {
		t.Error("empty focus should not include focus hint")
	}
}

func TestBuildSummaryPrompt_NilMessage(t *testing.T) {
	entries := []TranscriptEntry{
		{
			Type:    MessageTypeUser,
			Message: nil,
		},
		{
			Type: MessageTypeUser,
			Message: &Message{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
		},
	}

	prompt := BuildSummaryPrompt(entries, DefaultMaxPromptChars)
	if !strings.Contains(prompt, "Hello") {
		t.Error("prompt should skip nil messages and include valid ones")
	}
}
