package testutil

import (
	"encoding/json"
)

// SampleGeminiTranscript returns a sample Gemini CLI session JSON transcript for testing.
// Gemini sessions are stored as a single JSON object with a "messages" array.
func SampleGeminiTranscript() string {
	session := map[string]interface{}{
		"messages": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": "Hello, can you help me with a task?"},
				},
			},
			{
				"role": "model",
				"parts": []map[string]interface{}{
					{"text": "Of course! What would you like help with?"},
				},
			},
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": "Please create a file called test.txt"},
				},
			},
			{
				"role": "model",
				"parts": []map[string]interface{}{
					{"text": "I'll create that file for you."},
					{
						"functionCall": map[string]interface{}{
							"name": "run_shell_command",
							"args": map[string]interface{}{
								"command": "echo 'test content' > test.txt",
							},
						},
					},
				},
			},
		},
	}

	data, _ := json.MarshalIndent(session, "", "  ")
	return string(data)
}

// SampleGeminiHookInput returns sample AfterTool hook JSON for Gemini CLI testing.
func SampleGeminiHookInput(sessionID, transcriptPath, command string) string {
	input := map[string]interface{}{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"tool_name":       "run_shell_command",
		"tool_input": map[string]interface{}{
			"command": command,
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}

// SampleGeminiHookInputNonShell returns hook JSON for a non-shell Gemini tool.
func SampleGeminiHookInputNonShell(sessionID string) string {
	input := map[string]interface{}{
		"session_id": sessionID,
		"tool_name":  "read_file",
		"tool_input": map[string]interface{}{
			"path": "/some/file.txt",
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}
