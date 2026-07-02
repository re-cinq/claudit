package testutil

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SampleOpenCodeTranscript returns a sample OpenCode JSONL transcript for testing.
// OpenCode stores messages as individual JSON objects (one per line in JSONL, or
// individual files in a message directory).
func SampleOpenCodeTranscript() string {
	entries := []map[string]interface{}{
		{
			"id":   "msg-u1",
			"role": "user",
			"content": "Hello, can you help me with a task?",
			"time": map[string]interface{}{
				"created": time.Now().Format(time.RFC3339),
			},
		},
		{
			"id":   "msg-a1",
			"role": "assistant",
			"content": "Of course! What would you like help with?",
			"time": map[string]interface{}{
				"created": time.Now().Format(time.RFC3339),
			},
		},
		{
			"id":   "msg-u2",
			"role": "user",
			"content": "Please create a file called test.txt",
			"time": map[string]interface{}{
				"created": time.Now().Format(time.RFC3339),
			},
		},
		{
			"id":   "msg-a2",
			"role": "assistant",
			"content": "I'll create that file for you.",
			"time": map[string]interface{}{
				"created": time.Now().Format(time.RFC3339),
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

// SampleOpenCodeHookInput returns sample hook JSON for OpenCode CLI testing.
// The second parameter is data_dir (the base directory for OpenCode's storage),
// not a direct transcript path. The store command reconstructs the transcript path
// as data_dir/storage/message/<sessionID>.
func SampleOpenCodeHookInput(sessionID, dataDir, command string) string {
	input := map[string]interface{}{
		"session_id":  sessionID,
		"data_dir":    dataDir,
		"project_dir": "/test/project",
		"tool_name":   "bash",
		"tool_input": map[string]interface{}{
			"command": command,
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}

// SampleOpenCodeHookInputNonShell returns hook JSON for a non-shell OpenCode tool.
func SampleOpenCodeHookInputNonShell(sessionID string) string {
	input := map[string]interface{}{
		"session_id":  sessionID,
		"data_dir":    "/test/data",
		"project_dir": "/test/project",
		"tool_name":   "read",
		"tool_input": map[string]interface{}{
			"path": "/some/file.txt",
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}

// opencodePrepareTranscript sets up an OpenCode transcript in the directory structure
// that the store command expects: data_dir/storage/message/<sessionID>/transcript.jsonl.
// Returns the data_dir path for use as the second parameter to SampleOpenCodeHookInput.
func opencodePrepareTranscript(baseDir, sessionID, transcript string) (string, error) {
	dataDir := filepath.Join(baseDir, ".opencode-test-data")
	msgDir := filepath.Join(dataDir, "storage", "message", sessionID)
	if err := os.MkdirAll(msgDir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(msgDir, "transcript.jsonl"), []byte(transcript), 0644); err != nil {
		return "", err
	}
	return dataDir, nil
}

// opencodeReadRestoredTranscript reads the restored transcript from OpenCode's
// message directory (data_dir/storage/message/<sessionID>/transcript.jsonl).
// The store process normalizes JSONL into a JSON array, so this function
// converts back to JSONL for comparison with the original transcript.
func opencodeReadRestoredTranscript(homeDir, projectPath, sessionID string) ([]byte, error) {
	dataDir := opencodeDataDir(homeDir)
	msgDir := filepath.Join(dataDir, "storage", "message", sessionID)
	data, err := os.ReadFile(filepath.Join(msgDir, "transcript.jsonl"))
	if err != nil {
		return nil, err
	}

	// If the stored data is a JSON array, convert back to JSONL
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var messages []json.RawMessage
		if err := json.Unmarshal(trimmed, &messages); err == nil {
			var lines [][]byte
			for _, msg := range messages {
				lines = append(lines, []byte(strings.TrimSpace(string(msg))))
			}
			return bytes.Join(lines, []byte("\n")), nil
		}
	}

	return data, nil
}
