package storage

import (
	"encoding/json"
	"time"
)

// NoteFormatVersion is the current version of the git note format.
// Version history:
//   - 1: initial format (agent field added later with omitempty for compat)
//   - 2: added model field for tracking the AI model used
//   - 3: added effort field for tracking turns and token usage
const NoteFormatVersion = 3

// Effort captures quantified AI effort metrics for a commit.
type Effort struct {
	Turns                    int   `json:"turns,omitempty"`
	InputTokens              int64 `json:"input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
}

// TotalTokens returns the sum of input and output tokens, nil-safe.
func (e *Effort) TotalTokens() int64 {
	if e == nil {
		return 0
	}
	return e.InputTokens + e.OutputTokens
}

// StoredConversation represents the format stored in git notes
type StoredConversation struct {
	Version      int    `json:"version"`
	SessionID    string `json:"session_id"`
	Timestamp    string `json:"timestamp"`
	ProjectPath  string `json:"project_path"`
	GitBranch    string `json:"git_branch"`
	MessageCount int    `json:"message_count"`
	Checksum     string `json:"checksum"`
	Transcript   string `json:"transcript"`        // base64-encoded gzipped JSONL
	Agent        string  `json:"agent,omitempty"`    // coding agent name (empty = "claude" for backward compat)
	Model        string  `json:"model,omitempty"`    // AI model identifier (e.g. "claude-sonnet-4-5-20250514")
	Effort       *Effort `json:"effort,omitempty"`   // AI effort metrics (turns, tokens)
}

// NewStoredConversation creates a new StoredConversation from transcript data
func NewStoredConversation(sessionID, projectPath, gitBranch string, messageCount int, transcriptData []byte) (*StoredConversation, error) {
	checksum := Checksum(transcriptData)

	encoded, err := CompressAndEncode(transcriptData)
	if err != nil {
		return nil, err
	}

	return &StoredConversation{
		Version:      NoteFormatVersion,
		SessionID:    sessionID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		ProjectPath:  projectPath,
		GitBranch:    gitBranch,
		MessageCount: messageCount,
		Checksum:     checksum,
		Transcript:   encoded,
	}, nil
}

// Marshal serializes the stored conversation to JSON
func (sc *StoredConversation) Marshal() ([]byte, error) {
	return json.MarshalIndent(sc, "", "  ")
}

// UnmarshalStoredConversation deserializes a stored conversation from JSON
func UnmarshalStoredConversation(data []byte) (*StoredConversation, error) {
	var sc StoredConversation
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// GetTranscript decompresses and returns the original transcript data
func (sc *StoredConversation) GetTranscript() ([]byte, error) {
	return DecodeAndDecompress(sc.Transcript)
}

// VerifyIntegrity checks if the transcript matches the stored checksum
func (sc *StoredConversation) VerifyIntegrity() (bool, error) {
	transcript, err := sc.GetTranscript()
	if err != nil {
		return false, err
	}
	return VerifyChecksum(transcript, sc.Checksum), nil
}
