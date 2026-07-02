package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SampleCodexTranscript returns a sample Codex CLI rollout JSONL transcript for testing.
// Codex sessions are JSONL files with session_meta on the first line followed by response_items.
func SampleCodexTranscript() string {
	now := time.Now().Format(time.RFC3339)

	lines := []map[string]interface{}{
		{
			"timestamp": now,
			"type":      "session_meta",
			"payload": map[string]interface{}{
				"id":             "test-session",
				"cwd":            "/test/project",
				"cli_version":    "1.0.0",
				"model_provider": "openai",
			},
		},
		{
			"timestamp": now,
			"type":      "response_item",
			"payload": map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "input_text", "text": "Hello, can you help me with a task?"},
				},
			},
		},
		{
			"timestamp": now,
			"type":      "response_item",
			"payload": map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "output_text", "text": "Of course! What would you like help with?"},
				},
			},
		},
		{
			"timestamp": now,
			"type":      "response_item",
			"payload": map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "input_text", "text": "Please create a file called test.txt"},
				},
			},
		},
		{
			"timestamp": now,
			"type":      "response_item",
			"payload": map[string]interface{}{
				"type":      "function_call",
				"name":      "shell",
				"arguments": `{"command":["echo","test content",">","test.txt"]}`,
				"call_id":   "call_1",
			},
		},
	}

	var parts []string
	for _, line := range lines {
		data, _ := json.Marshal(line)
		parts = append(parts, string(data))
	}
	return strings.Join(parts, "\n")
}

// SampleCodexHookInput returns sample hook JSON for Codex CLI testing.
// Since Codex is hookless (no per-tool hooks), this is used for the hook-based
// store path that receives JSON via stdin from the post-commit hook.
func SampleCodexHookInput(sessionID, transcriptPath, command string) string {
	input := map[string]interface{}{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"tool_name":       "shell",
		"tool_input": map[string]interface{}{
			"command": command,
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}

// SampleCodexHookInputNonShell returns hook JSON for a non-shell Codex tool.
func SampleCodexHookInputNonShell(sessionID string) string {
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

// codexPrepareTranscript writes a Codex JSONL rollout transcript file.
func codexPrepareTranscript(baseDir, sessionID, transcript string) (string, error) {
	path := filepath.Join(baseDir, "rollout.jsonl")
	return path, os.WriteFile(path, []byte(transcript), 0644)
}

// codexSessionDir computes the Codex session directory path.
// Codex sessions are date-organized under ~/.codex/sessions/YYYY/MM/DD/
// We return the date-specific directory where WriteSessionFile will write today.
func codexSessionDir(homeDir, projectPath string) string {
	now := time.Now()
	return filepath.Join(homeDir, ".codex", "sessions",
		now.Format("2006"), now.Format("01"), now.Format("02"))
}

// codexReadRestoredTranscript finds and reads the restored Codex transcript.
// Since Codex files are named rollout-<timestamp>-<sessionID>.jsonl, we walk
// the sessions directory to find a file containing the session ID.
func codexReadRestoredTranscript(homeDir, projectPath, sessionID string) ([]byte, error) {
	sessionsDir := filepath.Join(homeDir, ".codex", "sessions")
	var found string
	_ = filepath.Walk(sessionsDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(info.Name(), sessionID) && strings.HasSuffix(info.Name(), ".jsonl") {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(found)
}
