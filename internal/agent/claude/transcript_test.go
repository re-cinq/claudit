package claude

import (
	"strings"
	"testing"
)

func TestGetLastEntryUUID(t *testing.T) {
	tests := []struct {
		name     string
		jsonl    string
		expected string
	}{
		{
			name:     "empty transcript",
			jsonl:    "",
			expected: "",
		},
		{
			name:     "single entry",
			jsonl:    `{"uuid":"abc123","type":"user"}`,
			expected: "abc123",
		},
		{
			name:     "multiple entries",
			jsonl:    `{"uuid":"first","type":"user"}` + "\n" + `{"uuid":"second","type":"assistant"}` + "\n" + `{"uuid":"third","type":"user"}`,
			expected: "third",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transcript, err := ParseJSONLTranscript(strings.NewReader(tc.jsonl))
			if err != nil {
				t.Fatalf("ParseJSONLTranscript failed: %v", err)
			}
			result := transcript.GetLastEntryUUID()
			if result != tc.expected {
				t.Errorf("GetLastEntryUUID() = %q, expected %q", result, tc.expected)
			}
		})
	}
}

func TestFindEntryIndex(t *testing.T) {
	jsonl := `{"uuid":"first","type":"user"}` + "\n" + `{"uuid":"second","type":"assistant"}` + "\n" + `{"uuid":"third","type":"user"}`
	transcript, err := ParseJSONLTranscript(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseJSONLTranscript failed: %v", err)
	}

	tests := []struct {
		uuid     string
		expected int
	}{
		{"first", 0},
		{"second", 1},
		{"third", 2},
		{"nonexistent", -1},
		{"", -1},
	}

	for _, tc := range tests {
		t.Run(tc.uuid, func(t *testing.T) {
			result := transcript.FindEntryIndex(tc.uuid)
			if result != tc.expected {
				t.Errorf("FindEntryIndex(%q) = %d, expected %d", tc.uuid, result, tc.expected)
			}
		})
	}
}

func TestGetEntriesSince(t *testing.T) {
	jsonl := `{"uuid":"first","type":"user"}` + "\n" + `{"uuid":"second","type":"assistant"}` + "\n" + `{"uuid":"third","type":"user"}` + "\n" + `{"uuid":"fourth","type":"assistant"}`
	transcript, err := ParseJSONLTranscript(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseJSONLTranscript failed: %v", err)
	}

	tests := []struct {
		name          string
		lastUUID      string
		expectedUUIDs []string
	}{
		{
			name:          "empty uuid returns all",
			lastUUID:      "",
			expectedUUIDs: []string{"first", "second", "third", "fourth"},
		},
		{
			name:          "from first entry",
			lastUUID:      "first",
			expectedUUIDs: []string{"second", "third", "fourth"},
		},
		{
			name:          "from middle entry",
			lastUUID:      "second",
			expectedUUIDs: []string{"third", "fourth"},
		},
		{
			name:          "from last entry",
			lastUUID:      "fourth",
			expectedUUIDs: []string{},
		},
		{
			name:          "nonexistent uuid returns all",
			lastUUID:      "nonexistent",
			expectedUUIDs: []string{"first", "second", "third", "fourth"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries := transcript.GetEntriesSince(tc.lastUUID)

			if len(entries) != len(tc.expectedUUIDs) {
				t.Errorf("GetEntriesSince(%q) returned %d entries, expected %d", tc.lastUUID, len(entries), len(tc.expectedUUIDs))
				return
			}

			for i, entry := range entries {
				if entry.UUID != tc.expectedUUIDs[i] {
					t.Errorf("Entry %d UUID = %q, expected %q", i, entry.UUID, tc.expectedUUIDs[i])
				}
			}
		})
	}
}

func TestGetEntriesSinceWithEmptyTranscript(t *testing.T) {
	transcript, err := ParseJSONLTranscript(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ParseJSONLTranscript failed: %v", err)
	}

	entries := transcript.GetEntriesSince("")
	if len(entries) != 0 {
		t.Errorf("Expected 0 entries from empty transcript, got %d", len(entries))
	}

	entries = transcript.GetEntriesSince("some-uuid")
	if len(entries) != 0 {
		t.Errorf("Expected 0 entries from empty transcript with uuid, got %d", len(entries))
	}
}

func TestParseJSONLTranscriptExtractsModel(t *testing.T) {
	jsonl := `{"uuid":"user-1","type":"user","message":{"role":"user","content":[{"type":"text","text":"Hello"}]}}
{"uuid":"assistant-1","type":"assistant","model":"claude-sonnet-4-5-20250514","message":{"role":"assistant","content":[{"type":"text","text":"Hi!"}]}}
{"uuid":"user-2","type":"user","message":{"role":"user","content":[{"type":"text","text":"Thanks"}]}}`

	transcript, err := ParseJSONLTranscript(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseJSONLTranscript failed: %v", err)
	}

	if transcript.Model != "claude-sonnet-4-5-20250514" {
		t.Errorf("Model = %q, want %q", transcript.Model, "claude-sonnet-4-5-20250514")
	}
}

func TestParseJSONLTranscriptNoModel(t *testing.T) {
	jsonl := `{"uuid":"user-1","type":"user","message":{"role":"user","content":[{"type":"text","text":"Hello"}]}}
{"uuid":"assistant-1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hi!"}]}}`

	transcript, err := ParseJSONLTranscript(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseJSONLTranscript failed: %v", err)
	}

	if transcript.Model != "" {
		t.Errorf("Model = %q, want empty", transcript.Model)
	}
}

func TestParseJSONLTranscriptExtractsUsage(t *testing.T) {
	jsonl := `{"uuid":"user-1","type":"user","message":{"role":"user","content":[{"type":"text","text":"Hello"}]}}
{"uuid":"assistant-1","type":"assistant","model":"claude-sonnet-4-5-20250514","message":{"role":"assistant","content":[{"type":"text","text":"Hi!"}],"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":20}}}
{"uuid":"user-2","type":"user","message":{"role":"user","content":[{"type":"text","text":"Thanks"}]}}
{"uuid":"assistant-2","type":"assistant","model":"claude-sonnet-4-5-20250514","message":{"role":"assistant","content":[{"type":"text","text":"You're welcome!"}],"usage":{"input_tokens":200,"output_tokens":75,"cache_creation_input_tokens":0,"cache_read_input_tokens":30}}}`

	transcript, err := ParseJSONLTranscript(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseJSONLTranscript failed: %v", err)
	}

	if transcript.Usage.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", transcript.Usage.InputTokens)
	}
	if transcript.Usage.OutputTokens != 125 {
		t.Errorf("OutputTokens = %d, want 125", transcript.Usage.OutputTokens)
	}
	if transcript.Usage.CacheCreationInputTokens != 10 {
		t.Errorf("CacheCreationInputTokens = %d, want 10", transcript.Usage.CacheCreationInputTokens)
	}
	if transcript.Usage.CacheReadInputTokens != 50 {
		t.Errorf("CacheReadInputTokens = %d, want 50", transcript.Usage.CacheReadInputTokens)
	}
	if transcript.Usage.TotalTokens() != 425 {
		t.Errorf("TotalTokens() = %d, want 425", transcript.Usage.TotalTokens())
	}
}

func TestParseJSONLTranscriptCountsTurns(t *testing.T) {
	jsonl := `{"uuid":"user-1","type":"user","message":{"role":"user","content":[{"type":"text","text":"Hello"}]}}
{"uuid":"assistant-1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hi!"}]}}
{"uuid":"tool-result-1","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"uuid":"assistant-2","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done"}]}}
{"uuid":"user-2","type":"user","message":{"role":"user","content":[{"type":"text","text":"Thanks"}]}}`

	transcript, err := ParseJSONLTranscript(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseJSONLTranscript failed: %v", err)
	}

	// Only user entries with text content count as turns (not tool_result entries)
	if transcript.Turns != 2 {
		t.Errorf("Turns = %d, want 2", transcript.Turns)
	}
}

func TestParseJSONLTranscriptNoUsage(t *testing.T) {
	jsonl := `{"uuid":"user-1","type":"user","message":{"role":"user","content":[{"type":"text","text":"Hello"}]}}
{"uuid":"assistant-1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hi!"}]}}`

	transcript, err := ParseJSONLTranscript(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseJSONLTranscript failed: %v", err)
	}

	if transcript.Usage.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", transcript.Usage.InputTokens)
	}
	if transcript.Usage.OutputTokens != 0 {
		t.Errorf("OutputTokens = %d, want 0", transcript.Usage.OutputTokens)
	}
	if transcript.Turns != 1 {
		t.Errorf("Turns = %d, want 1", transcript.Turns)
	}
}
