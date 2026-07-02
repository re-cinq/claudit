package storage

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewStoredConversation(t *testing.T) {
	transcript := []byte(`{"uuid":"1","type":"user"}`)

	sc, err := NewStoredConversation("session-1", "/test/project", "main", 5, transcript)
	if err != nil {
		t.Fatalf("NewStoredConversation() error: %v", err)
	}

	if sc.Version != NoteFormatVersion {
		t.Errorf("Version = %d, want %d", sc.Version, NoteFormatVersion)
	}
	if sc.SessionID != "session-1" {
		t.Errorf("SessionID = %q, want %q", sc.SessionID, "session-1")
	}
	if sc.ProjectPath != "/test/project" {
		t.Errorf("ProjectPath = %q, want %q", sc.ProjectPath, "/test/project")
	}
	if sc.GitBranch != "main" {
		t.Errorf("GitBranch = %q, want %q", sc.GitBranch, "main")
	}
	if sc.MessageCount != 5 {
		t.Errorf("MessageCount = %d, want 5", sc.MessageCount)
	}
	if !strings.HasPrefix(sc.Checksum, "sha256:") {
		t.Errorf("Checksum = %q, want sha256: prefix", sc.Checksum)
	}
	if sc.Transcript == "" {
		t.Error("Transcript should not be empty")
	}
	if sc.Timestamp == "" {
		t.Error("Timestamp should not be empty")
	}
}

func TestStoredConversationGetTranscript(t *testing.T) {
	original := []byte(`{"uuid":"1","type":"user","message":{"content":[{"type":"text","text":"Hello"}]}}`)

	sc, err := NewStoredConversation("session-1", "/test", "main", 1, original)
	if err != nil {
		t.Fatalf("NewStoredConversation() error: %v", err)
	}

	got, err := sc.GetTranscript()
	if err != nil {
		t.Fatalf("GetTranscript() error: %v", err)
	}

	if string(got) != string(original) {
		t.Errorf("GetTranscript() returned different data: got %d bytes, want %d bytes", len(got), len(original))
	}
}

func TestStoredConversationVerifyIntegrity(t *testing.T) {
	transcript := []byte(`{"uuid":"1","type":"user"}`)

	sc, err := NewStoredConversation("session-1", "/test", "main", 1, transcript)
	if err != nil {
		t.Fatalf("NewStoredConversation() error: %v", err)
	}

	valid, err := sc.VerifyIntegrity()
	if err != nil {
		t.Fatalf("VerifyIntegrity() error: %v", err)
	}
	if !valid {
		t.Error("VerifyIntegrity() returned false for valid conversation")
	}
}

func TestStoredConversationVerifyIntegrityTampered(t *testing.T) {
	transcript := []byte(`{"uuid":"1","type":"user"}`)

	sc, err := NewStoredConversation("session-1", "/test", "main", 1, transcript)
	if err != nil {
		t.Fatalf("NewStoredConversation() error: %v", err)
	}

	// Tamper with the checksum
	sc.Checksum = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	valid, err := sc.VerifyIntegrity()
	if err != nil {
		t.Fatalf("VerifyIntegrity() error: %v", err)
	}
	if valid {
		t.Error("VerifyIntegrity() returned true for tampered checksum")
	}
}

func TestStoredConversationVerifyIntegrityCorruptTranscript(t *testing.T) {
	sc := &StoredConversation{
		Version:    1,
		SessionID:  "session-1",
		Checksum:   "sha256:abcd",
		Transcript: "not valid base64!!!",
	}

	_, err := sc.VerifyIntegrity()
	if err == nil {
		t.Error("VerifyIntegrity() should fail on corrupt transcript")
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	transcript := []byte(`{"uuid":"1","type":"user"}`)

	original, err := NewStoredConversation("session-1", "/test", "main", 1, transcript)
	if err != nil {
		t.Fatalf("NewStoredConversation() error: %v", err)
	}

	data, err := original.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	// Verify it's valid JSON
	if !json.Valid(data) {
		t.Fatal("Marshal() produced invalid JSON")
	}

	restored, err := UnmarshalStoredConversation(data)
	if err != nil {
		t.Fatalf("UnmarshalStoredConversation() error: %v", err)
	}

	if restored.SessionID != original.SessionID {
		t.Errorf("SessionID mismatch after round-trip")
	}
	if restored.Checksum != original.Checksum {
		t.Errorf("Checksum mismatch after round-trip")
	}
	if restored.Transcript != original.Transcript {
		t.Errorf("Transcript mismatch after round-trip")
	}

	// Verify the restored conversation still passes integrity check
	valid, err := restored.VerifyIntegrity()
	if err != nil {
		t.Fatalf("VerifyIntegrity() error after round-trip: %v", err)
	}
	if !valid {
		t.Error("VerifyIntegrity() failed after marshal/unmarshal round-trip")
	}
}

func TestUnmarshalInvalidJSON(t *testing.T) {
	_, err := UnmarshalStoredConversation([]byte("not json"))
	if err == nil {
		t.Error("UnmarshalStoredConversation() should fail on invalid JSON")
	}
}

func TestUnmarshalV1NoteBackwardCompat(t *testing.T) {
	// Simulate a v1 note (no model field) to verify backward compatibility
	v1Note := `{
		"version": 1,
		"session_id": "old-session",
		"timestamp": "2025-01-01T00:00:00Z",
		"project_path": "/old/project",
		"git_branch": "main",
		"message_count": 3,
		"checksum": "sha256:abc123",
		"transcript": "H4sIAAAAAAAAA6tWKkktLlGyUlAqS8wpTtVRSs7PS8nMS1eqBQBHsjzMGgAAAA==",
		"agent": "claude"
	}`

	sc, err := UnmarshalStoredConversation([]byte(v1Note))
	if err != nil {
		t.Fatalf("UnmarshalStoredConversation() failed on v1 note: %v", err)
	}

	if sc.Version != 1 {
		t.Errorf("Version = %d, want 1", sc.Version)
	}
	if sc.Agent != "claude" {
		t.Errorf("Agent = %q, want %q", sc.Agent, "claude")
	}
	if sc.Model != "" {
		t.Errorf("Model = %q, want empty for v1 note", sc.Model)
	}
}

func TestMarshalIncludesModelField(t *testing.T) {
	transcript := []byte(`{"uuid":"1","type":"user"}`)

	sc, err := NewStoredConversation("session-1", "/test", "main", 1, transcript)
	if err != nil {
		t.Fatalf("NewStoredConversation() error: %v", err)
	}

	sc.Agent = "claude"
	sc.Model = "claude-sonnet-4-5-20250514"

	data, err := sc.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	// Verify round-trip preserves model
	restored, err := UnmarshalStoredConversation(data)
	if err != nil {
		t.Fatalf("UnmarshalStoredConversation() error: %v", err)
	}
	if restored.Model != "claude-sonnet-4-5-20250514" {
		t.Errorf("Model = %q, want %q", restored.Model, "claude-sonnet-4-5-20250514")
	}
	if restored.Version != NoteFormatVersion {
		t.Errorf("Version = %d, want %d", restored.Version, NoteFormatVersion)
	}

	// Verify JSON contains the model field
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to parse marshaled JSON: %v", err)
	}
	if raw["model"] != "claude-sonnet-4-5-20250514" {
		t.Errorf("JSON model = %v, want %q", raw["model"], "claude-sonnet-4-5-20250514")
	}
}

func TestMarshalOmitsEmptyModel(t *testing.T) {
	transcript := []byte(`{"uuid":"1","type":"user"}`)

	sc, err := NewStoredConversation("session-1", "/test", "main", 1, transcript)
	if err != nil {
		t.Fatalf("NewStoredConversation() error: %v", err)
	}

	// Don't set Model — it should be omitted from JSON
	data, err := sc.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to parse marshaled JSON: %v", err)
	}
	if _, exists := raw["model"]; exists {
		t.Error("JSON should not contain 'model' key when Model is empty")
	}
}

func TestEffortV3RoundTrip(t *testing.T) {
	transcript := []byte(`{"uuid":"1","type":"user"}`)

	sc, err := NewStoredConversation("session-1", "/test", "main", 1, transcript)
	if err != nil {
		t.Fatalf("NewStoredConversation() error: %v", err)
	}

	sc.Effort = &Effort{
		Turns:                    5,
		InputTokens:              12345,
		OutputTokens:             6789,
		CacheCreationInputTokens: 1000,
		CacheReadInputTokens:     2000,
	}

	data, err := sc.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	restored, err := UnmarshalStoredConversation(data)
	if err != nil {
		t.Fatalf("UnmarshalStoredConversation() error: %v", err)
	}

	if restored.Effort == nil {
		t.Fatal("Effort should not be nil after round-trip")
	}
	if restored.Effort.Turns != 5 {
		t.Errorf("Turns = %d, want 5", restored.Effort.Turns)
	}
	if restored.Effort.InputTokens != 12345 {
		t.Errorf("InputTokens = %d, want 12345", restored.Effort.InputTokens)
	}
	if restored.Effort.OutputTokens != 6789 {
		t.Errorf("OutputTokens = %d, want 6789", restored.Effort.OutputTokens)
	}
	if restored.Effort.CacheCreationInputTokens != 1000 {
		t.Errorf("CacheCreationInputTokens = %d, want 1000", restored.Effort.CacheCreationInputTokens)
	}
	if restored.Effort.CacheReadInputTokens != 2000 {
		t.Errorf("CacheReadInputTokens = %d, want 2000", restored.Effort.CacheReadInputTokens)
	}
	if restored.Version != NoteFormatVersion {
		t.Errorf("Version = %d, want %d", restored.Version, NoteFormatVersion)
	}
}

func TestEffortV1V2BackwardCompat(t *testing.T) {
	// v1 note has no effort field
	v1Note := `{
		"version": 1,
		"session_id": "old-session",
		"timestamp": "2025-01-01T00:00:00Z",
		"project_path": "/old/project",
		"git_branch": "main",
		"message_count": 3,
		"checksum": "sha256:abc123",
		"transcript": "H4sIAAAAAAAAA6tWKkktLlGyUlAqS8wpTtVRSs7PS8nMS1eqBQBHsjzMGgAAAA=="
	}`

	sc, err := UnmarshalStoredConversation([]byte(v1Note))
	if err != nil {
		t.Fatalf("UnmarshalStoredConversation() failed on v1 note: %v", err)
	}

	if sc.Effort != nil {
		t.Error("Effort should be nil for v1 note")
	}

	// v2 note with model but no effort
	v2Note := `{
		"version": 2,
		"session_id": "v2-session",
		"timestamp": "2025-06-01T00:00:00Z",
		"project_path": "/v2/project",
		"git_branch": "main",
		"message_count": 5,
		"checksum": "sha256:def456",
		"transcript": "H4sIAAAAAAAAA6tWKkktLlGyUlAqS8wpTtVRSs7PS8nMS1eqBQBHsjzMGgAAAA==",
		"model": "claude-sonnet-4-5-20250514"
	}`

	sc, err = UnmarshalStoredConversation([]byte(v2Note))
	if err != nil {
		t.Fatalf("UnmarshalStoredConversation() failed on v2 note: %v", err)
	}

	if sc.Effort != nil {
		t.Error("Effort should be nil for v2 note")
	}
	if sc.Model != "claude-sonnet-4-5-20250514" {
		t.Errorf("Model = %q, want %q", sc.Model, "claude-sonnet-4-5-20250514")
	}
}

func TestEffortTotalTokensNilSafe(t *testing.T) {
	var e *Effort
	if e.TotalTokens() != 0 {
		t.Errorf("nil Effort TotalTokens() = %d, want 0", e.TotalTokens())
	}

	e = &Effort{InputTokens: 100, OutputTokens: 50}
	if e.TotalTokens() != 150 {
		t.Errorf("TotalTokens() = %d, want 150", e.TotalTokens())
	}
}

func TestEffortOmittedWhenNil(t *testing.T) {
	transcript := []byte(`{"uuid":"1","type":"user"}`)

	sc, err := NewStoredConversation("session-1", "/test", "main", 1, transcript)
	if err != nil {
		t.Fatalf("NewStoredConversation() error: %v", err)
	}

	// Don't set Effort — it should be omitted from JSON
	data, err := sc.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to parse marshaled JSON: %v", err)
	}
	if _, exists := raw["effort"]; exists {
		t.Error("JSON should not contain 'effort' key when Effort is nil")
	}
}
