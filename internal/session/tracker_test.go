package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteAndReadActiveSession(t *testing.T) {
	// Create temp directory to act as git repo root
	tmpDir, err := os.MkdirTemp("", "session-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize as git repo
	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Change to temp directory for the test
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	// Create session to write
	session := &ActiveSession{
		SessionID:      "test-session-123",
		TranscriptPath: "/home/user/.claude/projects/-test/test-session-123.jsonl",
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		ProjectPath:    tmpDir,
	}

	// Write session
	err = WriteActiveSession(session)
	if err != nil {
		t.Fatalf("WriteActiveSession failed: %v", err)
	}

	// Verify file was created
	sessionPath := filepath.Join(tmpDir, ".shiftlog", "active-session.json")
	if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
		t.Error("active-session.json was not created")
	}

	// Read session back
	readSession, err := ReadActiveSession()
	if err != nil {
		t.Fatalf("ReadActiveSession failed: %v", err)
	}

	if readSession == nil {
		t.Fatal("ReadActiveSession returned nil")
	}

	if readSession.SessionID != session.SessionID {
		t.Errorf("SessionID mismatch: got %s, want %s", readSession.SessionID, session.SessionID)
	}

	if readSession.TranscriptPath != session.TranscriptPath {
		t.Errorf("TranscriptPath mismatch: got %s, want %s", readSession.TranscriptPath, session.TranscriptPath)
	}

	if readSession.ProjectPath != session.ProjectPath {
		t.Errorf("ProjectPath mismatch: got %s, want %s", readSession.ProjectPath, session.ProjectPath)
	}
}

func TestReadActiveSessionNotExists(t *testing.T) {
	// Create temp directory without active session file
	tmpDir, err := os.MkdirTemp("", "session-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize as git repo
	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Change to temp directory for the test
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	// Read should return nil when file doesn't exist
	session, err := ReadActiveSession()
	if err != nil {
		t.Fatalf("ReadActiveSession failed: %v", err)
	}

	if session != nil {
		t.Error("Expected nil session when file doesn't exist")
	}
}

func TestClearActiveSession(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "session-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize as git repo
	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Change to temp directory for the test
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	// Create session file
	session := &ActiveSession{
		SessionID:      "test-session-456",
		TranscriptPath: "/home/user/.claude/projects/-test/test-session-456.jsonl",
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		ProjectPath:    tmpDir,
	}

	err = WriteActiveSession(session)
	if err != nil {
		t.Fatalf("WriteActiveSession failed: %v", err)
	}

	// Clear session
	err = ClearActiveSession()
	if err != nil {
		t.Fatalf("ClearActiveSession failed: %v", err)
	}

	// Verify file was removed
	sessionPath := filepath.Join(tmpDir, ".shiftlog", "active-session.json")
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Error("active-session.json was not removed")
	}

	// Read should return nil now
	readSession, err := ReadActiveSession()
	if err != nil {
		t.Fatalf("ReadActiveSession failed: %v", err)
	}

	if readSession != nil {
		t.Error("Expected nil session after clear")
	}
}

func TestClearActiveSessionNotExists(t *testing.T) {
	// Create temp directory without session file
	tmpDir, err := os.MkdirTemp("", "session-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize as git repo
	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Change to temp directory for the test
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	// Clear should not error when file doesn't exist
	err = ClearActiveSession()
	if err != nil {
		t.Fatalf("ClearActiveSession failed when file doesn't exist: %v", err)
	}
}

func TestIsSessionActive(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "session-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create transcript file
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	err = os.WriteFile(transcriptPath, []byte("{}"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Test with recent transcript (should be active)
	session := &ActiveSession{
		SessionID:      "test-session",
		TranscriptPath: transcriptPath,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		ProjectPath:    tmpDir,
	}

	if !IsSessionActive(session) {
		t.Error("Expected recent transcript to be active")
	}

	// Test with nil session
	if IsSessionActive(nil) {
		t.Error("Expected nil session to be inactive")
	}

	// Test with empty transcript path
	emptySession := &ActiveSession{
		SessionID:      "test-session",
		TranscriptPath: "",
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		ProjectPath:    tmpDir,
	}

	if IsSessionActive(emptySession) {
		t.Error("Expected session with empty transcript path to be inactive")
	}

	// Test with non-existent transcript file
	missingSession := &ActiveSession{
		SessionID:      "test-session",
		TranscriptPath: filepath.Join(tmpDir, "nonexistent.jsonl"),
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		ProjectPath:    tmpDir,
	}

	if IsSessionActive(missingSession) {
		t.Error("Expected session with missing transcript to be inactive")
	}
}

func TestActiveSessionJSONFormat(t *testing.T) {
	session := &ActiveSession{
		SessionID:      "abc123",
		TranscriptPath: "/home/user/.claude/projects/-path/abc123.jsonl",
		StartedAt:      "2024-01-15T10:30:00Z",
		ProjectPath:    "/path/to/repo",
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	// Verify JSON keys match design spec
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	expectedKeys := []string{"session_id", "transcript_path", "started_at", "project_path"}
	for _, key := range expectedKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("Expected JSON key %q not found", key)
		}
	}
}

