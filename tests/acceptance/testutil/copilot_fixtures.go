package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// SampleCopilotTranscript returns a sample Copilot CLI events.jsonl transcript for testing.
func SampleCopilotTranscript() string {
	events := []string{
		`{"type":"session.start","data":{}}`,
		`{"type":"user.message","data":{"content":"Hello, can you help me with a task?"}}`,
		`{"type":"assistant.message","data":{"message":"Of course! What would you like help with?"}}`,
		`{"type":"user.message","data":{"content":"Please create a file called test.txt"}}`,
		`{"type":"assistant.message","data":{"message":"I'll create that file for you.","toolRequests":[{"id":"call_1","name":"bash","input":{"command":"echo 'test content' > test.txt"}}]}}`,
		`{"type":"tool.execution_start","data":{"toolUseId":"call_1","toolName":"bash"}}`,
		`{"type":"tool.execution_complete","data":{"toolUseId":"call_1","toolName":"bash","result":""}}`,
	}
	return strings.Join(events, "\n")
}

// SampleCopilotHookInput returns sample postToolUse hook JSON for Copilot CLI testing.
func SampleCopilotHookInput(sessionID, transcriptPath, command string) string {
	// Use the generic shared format with session_id and transcript_path
	// but with Copilot-native tool name "bash"
	input := map[string]interface{}{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"tool_name":       "bash",
		"tool_input": map[string]interface{}{
			"command": command,
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}

// SampleCopilotHookInputNonShell returns hook JSON for a non-shell Copilot tool.
func SampleCopilotHookInputNonShell(sessionID string) string {
	input := map[string]interface{}{
		"session_id": sessionID,
		"tool_name":  "create",
		"tool_input": map[string]interface{}{
			"path":    "/some/file.txt",
			"content": "hello",
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}

// copilotPrepareTranscript writes a Copilot events.jsonl transcript file.
func copilotPrepareTranscript(baseDir, sessionID, transcript string) (string, error) {
	path := filepath.Join(baseDir, "transcript.jsonl")
	return path, os.WriteFile(path, []byte(transcript), 0644)
}

// copilotSessionDir computes the Copilot session state directory path.
func copilotSessionDir(homeDir, projectPath string) string {
	return filepath.Join(homeDir, ".copilot", "session-state")
}

// copilotReadRestoredTranscript finds and reads the restored Copilot transcript.
func copilotReadRestoredTranscript(homeDir, projectPath, sessionID string) ([]byte, error) {
	sessionDir := filepath.Join(homeDir, ".copilot", "session-state")
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.Contains(entry.Name(), sessionID) {
			eventsPath := filepath.Join(sessionDir, entry.Name(), "events.jsonl")
			return os.ReadFile(eventsPath)
		}
	}
	return nil, os.ErrNotExist
}
