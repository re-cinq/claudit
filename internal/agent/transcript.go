package agent

import (
	"encoding/json"
)

// MessageType represents the type of a transcript entry.
type MessageType string

const (
	MessageTypeUser      MessageType = "user"
	MessageTypeAssistant MessageType = "assistant"
	MessageTypeSystem    MessageType = "system"
)

// ContentBlock represents a content block in a message.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// TranscriptEntry represents a single entry in a transcript.
type TranscriptEntry struct {
	UUID                    string          `json:"uuid"`
	ParentUUID              string          `json:"parentUuid,omitempty"`
	Type                    MessageType     `json:"type"`
	Timestamp               string          `json:"timestamp,omitempty"`
	Message                 *Message        `json:"message,omitempty"`
	SourceToolAssistantUUID string          `json:"sourceToolAssistantUUID,omitempty"`
	Raw                     json.RawMessage `json:"-"`
}

// Message represents a message content structure.
type Message struct {
	Role       string          `json:"role,omitempty"`
	Content    []ContentBlock  `json:"-"` // Custom unmarshal handles string or array
	RawContent json.RawMessage `json:"content,omitempty"`
}

// UnmarshalJSON handles content being either a string or array of blocks.
func (m *Message) UnmarshalJSON(data []byte) error {
	type Alias Message
	aux := &struct {
		*Alias
	}{
		Alias: (*Alias)(m),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}

	if len(m.RawContent) > 0 {
		if err := json.Unmarshal(m.RawContent, &m.Content); err != nil {
			var text string
			if err := json.Unmarshal(m.RawContent, &text); err == nil {
				m.Content = []ContentBlock{{Type: "text", Text: text}}
			}
		}
	}
	return nil
}

// UsageMetrics holds cumulative API token usage from a transcript.
type UsageMetrics struct {
	InputTokens              int64 `json:"input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
}

// TotalTokens returns the sum of input and output tokens.
func (u UsageMetrics) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens
}

// Transcript represents a parsed conversation transcript.
type Transcript struct {
	Entries []TranscriptEntry
	Model   string       // model identifier extracted from transcript (e.g. "claude-sonnet-4-5-20250514")
	Usage   UsageMetrics // cumulative token usage (Claude Code only)
	Turns   int          // number of user turns (all agents)
}

// MessageCount returns the number of entries in the transcript.
func (t *Transcript) MessageCount() int {
	return len(t.Entries)
}

// GetLastEntryUUID returns the UUID of the last entry in the transcript.
func (t *Transcript) GetLastEntryUUID() string {
	if len(t.Entries) == 0 {
		return ""
	}
	return t.Entries[len(t.Entries)-1].UUID
}

// FindEntryIndex finds the index of an entry by UUID, returns -1 if not found.
func (t *Transcript) FindEntryIndex(uuid string) int {
	for i, entry := range t.Entries {
		if entry.UUID == uuid {
			return i
		}
	}
	return -1
}

// CountTurns counts user turns that have text content (excludes pure tool_result entries).
func (t *Transcript) CountTurns() int {
	turns := 0
	for _, entry := range t.Entries {
		if entry.Type != MessageTypeUser {
			continue
		}
		if entry.Message == nil {
			continue
		}
		hasText := false
		for _, block := range entry.Message.Content {
			if block.Type == "text" && block.Text != "" {
				hasText = true
				break
			}
		}
		if hasText {
			turns++
		}
	}
	return turns
}

// GetEntriesSince returns entries that come after the given UUID.
// If uuid is empty, returns all entries (handles initial commit case).
func (t *Transcript) GetEntriesSince(lastUUID string) []TranscriptEntry {
	if lastUUID == "" {
		return t.Entries
	}

	idx := t.FindEntryIndex(lastUUID)
	if idx == -1 {
		return t.Entries
	}

	if idx+1 >= len(t.Entries) {
		return nil
	}
	return t.Entries[idx+1:]
}
