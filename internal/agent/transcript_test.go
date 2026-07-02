package agent

import (
	"encoding/json"
	"testing"
)

func TestCountTurns(t *testing.T) {
	tests := []struct {
		name     string
		entries  []TranscriptEntry
		expected int
	}{
		{
			name:     "empty transcript",
			entries:  nil,
			expected: 0,
		},
		{
			name: "single user message with text",
			entries: []TranscriptEntry{
				{Type: MessageTypeUser, Message: &Message{Content: []ContentBlock{{Type: "text", Text: "Hello"}}}},
			},
			expected: 1,
		},
		{
			name: "tool_result only does not count",
			entries: []TranscriptEntry{
				{Type: MessageTypeUser, Message: &Message{Content: []ContentBlock{{Type: "tool_result", ToolUseID: "t1", Content: json.RawMessage(`"ok"`)}}}},
			},
			expected: 0,
		},
		{
			name: "mixed user and tool_result entries",
			entries: []TranscriptEntry{
				{Type: MessageTypeUser, Message: &Message{Content: []ContentBlock{{Type: "text", Text: "Hello"}}}},
				{Type: MessageTypeAssistant, Message: &Message{Content: []ContentBlock{{Type: "text", Text: "Hi!"}}}},
				{Type: MessageTypeUser, Message: &Message{Content: []ContentBlock{{Type: "tool_result", ToolUseID: "t1"}}}},
				{Type: MessageTypeAssistant, Message: &Message{Content: []ContentBlock{{Type: "text", Text: "Done"}}}},
				{Type: MessageTypeUser, Message: &Message{Content: []ContentBlock{{Type: "text", Text: "Thanks"}}}},
			},
			expected: 2,
		},
		{
			name: "assistant entries not counted",
			entries: []TranscriptEntry{
				{Type: MessageTypeAssistant, Message: &Message{Content: []ContentBlock{{Type: "text", Text: "Hi!"}}}},
				{Type: MessageTypeAssistant, Message: &Message{Content: []ContentBlock{{Type: "text", Text: "Bye!"}}}},
			},
			expected: 0,
		},
		{
			name: "user entry with nil message",
			entries: []TranscriptEntry{
				{Type: MessageTypeUser, Message: nil},
			},
			expected: 0,
		},
		{
			name: "user entry with empty text",
			entries: []TranscriptEntry{
				{Type: MessageTypeUser, Message: &Message{Content: []ContentBlock{{Type: "text", Text: ""}}}},
			},
			expected: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transcript := &Transcript{Entries: tc.entries}
			got := transcript.CountTurns()
			if got != tc.expected {
				t.Errorf("CountTurns() = %d, want %d", got, tc.expected)
			}
		})
	}
}

func TestUsageMetricsTotalTokens(t *testing.T) {
	u := UsageMetrics{InputTokens: 100, OutputTokens: 50}
	if u.TotalTokens() != 150 {
		t.Errorf("TotalTokens() = %d, want 150", u.TotalTokens())
	}

	u = UsageMetrics{}
	if u.TotalTokens() != 0 {
		t.Errorf("zero UsageMetrics TotalTokens() = %d, want 0", u.TotalTokens())
	}
}
