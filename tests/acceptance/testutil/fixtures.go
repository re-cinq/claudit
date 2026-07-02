package testutil

import (
	"encoding/json"
	"time"
)

// SampleTranscript returns a sample JSONL transcript for testing
func SampleTranscript() string {
	entries := []map[string]interface{}{
		{
			"uuid":       "user-1",
			"parentUuid": "",
			"type":       "user",
			"timestamp":  time.Now().Format(time.RFC3339),
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Hello, can you help me with a task?"},
				},
			},
		},
		{
			"uuid":       "assistant-1",
			"parentUuid": "user-1",
			"type":       "assistant",
			"model":      "claude-sonnet-4-5-20250514",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Of course! What would you like help with?"},
				},
				"usage": map[string]interface{}{
					"input_tokens":                100,
					"output_tokens":               50,
					"cache_creation_input_tokens": 10,
					"cache_read_input_tokens":     20,
				},
			},
		},
		{
			"uuid":       "user-2",
			"parentUuid": "assistant-1",
			"type":       "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Please create a file called test.txt"},
				},
			},
		},
		{
			"uuid":       "assistant-2",
			"parentUuid": "user-2",
			"type":       "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "I'll create that file for you."},
					{
						"type":  "tool_use",
						"id":    "tool-1",
						"name":  "Bash",
						"input": map[string]interface{}{"command": "echo 'test content' > test.txt"},
					},
				},
				"usage": map[string]interface{}{
					"input_tokens":                200,
					"output_tokens":               75,
					"cache_creation_input_tokens": 0,
					"cache_read_input_tokens":     30,
				},
			},
		},
	}

	var result string
	for i, entry := range entries {
		data, _ := json.Marshal(entry)
		if i > 0 {
			result += "\n"
		}
		result += string(data)
	}
	return result
}

// SampleHookInput returns sample PostToolUse hook JSON for testing
func SampleHookInput(sessionID, transcriptPath, command string) string {
	input := map[string]interface{}{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"tool_name":       "Bash",
		"tool_input": map[string]interface{}{
			"command": command,
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}

// SampleHookInputNonBash returns hook JSON for a non-Bash tool
func SampleHookInputNonBash(sessionID string) string {
	input := map[string]interface{}{
		"session_id": sessionID,
		"tool_name":  "Read",
		"tool_input": map[string]interface{}{
			"path": "/some/file.txt",
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}

// SampleTranscriptWithIDs returns a sample JSONL transcript with specified UUIDs
// Useful for testing incremental conversation display
func SampleTranscriptWithIDs(uuids []string, messages []string) string {
	var entries []map[string]interface{}
	for i, uuid := range uuids {
		msg := "Message " + uuid
		if i < len(messages) {
			msg = messages[i]
		}

		entryType := "user"
		role := "user"
		if i%2 == 1 {
			entryType = "assistant"
			role = "assistant"
		}

		parentUUID := ""
		if i > 0 {
			parentUUID = uuids[i-1]
		}

		entries = append(entries, map[string]interface{}{
			"uuid":       uuid,
			"parentUuid": parentUUID,
			"type":       entryType,
			"timestamp":  time.Now().Format(time.RFC3339),
			"message": map[string]interface{}{
				"role": role,
				"content": []map[string]interface{}{
					{"type": "text", "text": msg},
				},
			},
		})
	}

	var result string
	for i, entry := range entries {
		data, _ := json.Marshal(entry)
		if i > 0 {
			result += "\n"
		}
		result += string(data)
	}
	return result
}
