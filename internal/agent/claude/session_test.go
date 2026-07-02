package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncodeProjectPath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/Users/dev/workspace/myproject", "-Users-dev-workspace-myproject"},
		{"/workspace", "-workspace"},
		{"/home/node/code", "-home-node-code"},
	}

	for _, tc := range tests {
		result := EncodeProjectPath(tc.input)
		if result != tc.expected {
			t.Errorf("EncodeProjectPath(%q) = %q, expected %q", tc.input, result, tc.expected)
		}
	}
}

func TestWriteAndReadSessionFile(t *testing.T) {
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tempHome)
	defer os.Setenv("HOME", origHome)

	projectPath := "/test/project"
	sessionID := "test-session-123"
	transcriptData := []byte(`{"uuid":"1","type":"user","message":{"content":[{"type":"text","text":"Hello"}]}}`)

	sessionPath, err := WriteSessionFile(projectPath, sessionID, transcriptData)
	if err != nil {
		t.Fatalf("WriteSessionFile failed: %v", err)
	}

	if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
		t.Errorf("Session file was not created at %s", sessionPath)
	}

	content, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("Could not read session file: %v", err)
	}
	if string(content) != string(transcriptData) {
		t.Errorf("Session file content mismatch")
	}

	expectedPath := filepath.Join(tempHome, ".claude", "projects", "-test-project", sessionID+".jsonl")
	if sessionPath != expectedPath {
		t.Errorf("Session path = %q, expected %q", sessionPath, expectedPath)
	}
}

func TestSessionsIndex(t *testing.T) {
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tempHome)
	defer os.Setenv("HOME", origHome)

	projectPath := "/test/project"

	index, err := ReadSessionsIndex(projectPath)
	if err != nil {
		t.Fatalf("ReadSessionsIndex failed: %v", err)
	}
	if index.Version != 1 {
		t.Errorf("Expected version 1, got %d", index.Version)
	}
	if len(index.Entries) != 0 {
		t.Errorf("Expected empty entries, got %d", len(index.Entries))
	}

	entry := SessionEntry{
		SessionID:    "session-1",
		FullPath:     "/test/path.jsonl",
		MessageCount: 10,
		ProjectPath:  projectPath,
	}
	AddOrUpdateSessionEntry(index, entry)

	err = WriteSessionsIndex(projectPath, index)
	if err != nil {
		t.Fatalf("WriteSessionsIndex failed: %v", err)
	}

	index2, err := ReadSessionsIndex(projectPath)
	if err != nil {
		t.Fatalf("ReadSessionsIndex failed: %v", err)
	}
	if len(index2.Entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(index2.Entries))
	}
	if index2.Entries[0].SessionID != "session-1" {
		t.Errorf("Session ID mismatch")
	}

	entry.MessageCount = 20
	AddOrUpdateSessionEntry(index2, entry)
	if len(index2.Entries) != 1 {
		t.Errorf("Expected 1 entry after update, got %d", len(index2.Entries))
	}
	if index2.Entries[0].MessageCount != 20 {
		t.Errorf("Message count not updated")
	}
}

func TestRestoreSession(t *testing.T) {
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tempHome)
	defer os.Setenv("HOME", origHome)

	projectPath := "/test/project"
	sessionID := "restore-test-123"
	gitBranch := "main"
	transcriptData := []byte(`{"uuid":"1","type":"user","message":{"content":[{"type":"text","text":"Hello world"}]}}
{"uuid":"2","type":"assistant","message":{"content":[{"type":"text","text":"Hi there!"}]}}`)
	messageCount := 2
	summary := "Test session"

	ag := &Agent{}
	err := ag.RestoreSession(projectPath, sessionID, gitBranch, transcriptData, messageCount, summary)
	if err != nil {
		t.Fatalf("RestoreSession failed: %v", err)
	}

	sessionPath, _ := GetSessionFilePath(projectPath, sessionID)
	if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
		t.Errorf("Session file was not created")
	}

	index, err := ReadSessionsIndex(projectPath)
	if err != nil {
		t.Fatalf("Could not read sessions index: %v", err)
	}
	if len(index.Entries) != 1 {
		t.Fatalf("Expected 1 entry in index, got %d", len(index.Entries))
	}

	e := index.Entries[0]
	if e.SessionID != sessionID {
		t.Errorf("Session ID mismatch: got %q, expected %q", e.SessionID, sessionID)
	}
	if e.GitBranch != gitBranch {
		t.Errorf("Git branch mismatch")
	}
	if e.Summary != summary {
		t.Errorf("Summary mismatch")
	}
	if e.FirstPrompt != "Hello world" {
		t.Errorf("FirstPrompt mismatch: got %q", e.FirstPrompt)
	}
}

func TestExtractFirstPrompt(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		expected string
	}{
		{
			name:     "simple user message",
			data:     `{"uuid":"1","type":"user","message":{"content":[{"type":"text","text":"Hello"}]}}`,
			expected: "Hello",
		},
		{
			name:     "empty transcript",
			data:     "",
			expected: "No prompt",
		},
		{
			name:     "only assistant message",
			data:     `{"uuid":"1","type":"assistant","message":{"content":[{"type":"text","text":"Hi"}]}}`,
			expected: "No prompt",
		},
		{
			name: "long prompt gets truncated",
			data: `{"uuid":"1","type":"user","message":{"content":[{"type":"text","text":"` +
				"This is a very long prompt that needs to be truncated because it exceeds the maximum length of 200 characters. We need to make sure that long prompts are properly truncated with an ellipsis at the end to indicate that there is more content that was cut off for display purposes." +
				`"}]}}`,
			expected: "This is a very long prompt that needs to be truncated because it exceeds the maximum length of 200 characters. We need to make sure that long prompts are properly truncated with an ellipsis at the ...",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractFirstPrompt([]byte(tc.data))
			if result != tc.expected {
				t.Errorf("extractFirstPrompt() = %q, expected %q", result, tc.expected)
			}
		})
	}
}
