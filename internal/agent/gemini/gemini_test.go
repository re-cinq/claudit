package gemini

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/re-cinq/shift-log/internal/agent"
)

func TestAgentName(t *testing.T) {
	a := &Agent{}
	if a.Name() != agent.Gemini {
		t.Errorf("Name() = %q, want %q", a.Name(), agent.Gemini)
	}
}

func TestAgentDisplayName(t *testing.T) {
	a := &Agent{}
	if a.DisplayName() != "Gemini CLI" {
		t.Errorf("DisplayName() = %q, want %q", a.DisplayName(), "Gemini CLI")
	}
}

func TestParseHookInput(t *testing.T) {
	a := &Agent{}
	input := `{"session_id":"sess-1","transcript_path":"/tmp/t.json","tool_name":"run_shell_command","tool_input":{"command":"git commit -m test"}}`

	hook, err := a.ParseHookInput([]byte(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error: %v", err)
	}
	if hook.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", hook.SessionID, "sess-1")
	}
	if hook.TranscriptPath != "/tmp/t.json" {
		t.Errorf("TranscriptPath = %q, want %q", hook.TranscriptPath, "/tmp/t.json")
	}
	if hook.ToolName != "run_shell_command" {
		t.Errorf("ToolName = %q, want %q", hook.ToolName, "run_shell_command")
	}
	if hook.Command != "git commit -m test" {
		t.Errorf("Command = %q, want %q", hook.Command, "git commit -m test")
	}
}

func TestIsCommitCommand(t *testing.T) {
	a := &Agent{}
	tests := []struct {
		tool, cmd string
		want      bool
	}{
		{"run_shell_command", "git commit -m fix", true},
		{"run_shell_command", "git commit -am msg", true},
		{"run_shell_command", "git-commit", true},
		{"run_shell_command", "ls -la", false},
		{"run_shell_command", "git status", false},
		{"read_file", "git commit -m test", false},
		{"replace", "git commit -m test", false},
	}

	for _, tc := range tests {
		got := a.IsCommitCommand(tc.tool, tc.cmd)
		if got != tc.want {
			t.Errorf("IsCommitCommand(%q, %q) = %v, want %v", tc.tool, tc.cmd, got, tc.want)
		}
	}
}

func TestParseGeminiTranscript(t *testing.T) {
	session := `{
		"messages": [
			{"role": "user", "parts": [{"text": "Hello"}]},
			{"role": "model", "parts": [{"text": "Hi there"}]},
			{"role": "user", "parts": [{"text": "Thanks"}]}
		]
	}`

	transcript, err := ParseGeminiTranscript(strings.NewReader(session))
	if err != nil {
		t.Fatalf("ParseGeminiTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(transcript.Entries))
	}

	// Check first entry (user)
	if transcript.Entries[0].Type != agent.MessageTypeUser {
		t.Errorf("Entry 0 type = %q, want %q", transcript.Entries[0].Type, agent.MessageTypeUser)
	}

	// Check second entry (model -> assistant)
	if transcript.Entries[1].Type != agent.MessageTypeAssistant {
		t.Errorf("Entry 1 type = %q, want %q", transcript.Entries[1].Type, agent.MessageTypeAssistant)
	}
	if transcript.Entries[1].Message == nil || len(transcript.Entries[1].Message.Content) == 0 {
		t.Fatal("Entry 1 has no message content")
	}
	if transcript.Entries[1].Message.Content[0].Text != "Hi there" {
		t.Errorf("Entry 1 text = %q, want %q", transcript.Entries[1].Message.Content[0].Text, "Hi there")
	}
}

func TestParseGeminiTranscriptWithToolCalls(t *testing.T) {
	session := `{
		"messages": [
			{"role": "user", "parts": [{"text": "Run ls"}]},
			{"role": "model", "parts": [{"text": "Running command"}, {"functionCall": {"name": "run_shell_command", "args": {"command": "ls -la"}}}]}
		]
	}`

	transcript, err := ParseGeminiTranscript(strings.NewReader(session))
	if err != nil {
		t.Fatalf("ParseGeminiTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 2 {
		t.Fatalf("Expected 2 entries, got %d", len(transcript.Entries))
	}

	// Check model entry has both text and tool_use content blocks
	entry := transcript.Entries[1]
	if len(entry.Message.Content) != 2 {
		t.Fatalf("Expected 2 content blocks, got %d", len(entry.Message.Content))
	}
	if entry.Message.Content[0].Type != "text" {
		t.Errorf("Content[0] type = %q, want text", entry.Message.Content[0].Type)
	}
	if entry.Message.Content[1].Type != "tool_use" {
		t.Errorf("Content[1] type = %q, want tool_use", entry.Message.Content[1].Type)
	}
	if entry.Message.Content[1].Name != "run_shell_command" {
		t.Errorf("Content[1] name = %q, want run_shell_command", entry.Message.Content[1].Name)
	}
}

func TestParseGeminiTranscriptEmpty(t *testing.T) {
	transcript, err := ParseGeminiTranscript(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ParseGeminiTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(transcript.Entries))
	}
}

func TestParseGeminiTranscriptEmptyMessages(t *testing.T) {
	session := `{"messages": []}`
	transcript, err := ParseGeminiTranscript(strings.NewReader(session))
	if err != nil {
		t.Fatalf("ParseGeminiTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(transcript.Entries))
	}
}

func TestToolAliases(t *testing.T) {
	a := &Agent{}
	aliases := a.ToolAliases()
	if aliases["run_shell_command"] != "Bash" {
		t.Errorf("ToolAliases[run_shell_command] = %q, want Bash", aliases["run_shell_command"])
	}
	if aliases["replace"] != "Edit" {
		t.Errorf("ToolAliases[replace] = %q, want Edit", aliases["replace"])
	}
	if aliases["grep_search"] != "Grep" {
		t.Errorf("ToolAliases[grep_search] = %q, want Grep", aliases["grep_search"])
	}
	if aliases["google_web_search"] != "WebSearch" {
		t.Errorf("ToolAliases[google_web_search] = %q, want WebSearch", aliases["google_web_search"])
	}
}

func TestResumeCommand(t *testing.T) {
	a := &Agent{}
	bin, args := a.ResumeCommand("sess-123")
	if bin != "gemini" {
		t.Errorf("ResumeCommand binary = %q, want gemini", bin)
	}
	if len(args) != 2 || args[0] != "--resume" || args[1] != "sess-123" {
		t.Errorf("ResumeCommand args = %v, want [--resume sess-123]", args)
	}
}

func TestConfigureHooks(t *testing.T) {
	a := &Agent{}
	tmpDir := t.TempDir()

	if err := a.ConfigureHooks(tmpDir); err != nil {
		t.Fatalf("ConfigureHooks() error: %v", err)
	}

	// Check settings file was created
	settingsPath := filepath.Join(tmpDir, ".gemini", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("Failed to read settings: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}

	hooks, ok := raw["hooks"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing hooks key in settings")
	}

	afterTool, ok := hooks["AfterTool"].([]interface{})
	if !ok || len(afterTool) == 0 {
		t.Fatal("Missing AfterTool hook")
	}

	// Verify matcher is run_shell_command
	hookObj := afterTool[0].(map[string]interface{})
	if hookObj["matcher"] != "run_shell_command" {
		t.Errorf("AfterTool matcher = %q, want run_shell_command", hookObj["matcher"])
	}

	// Verify timeout is 30000 (milliseconds)
	hookCmds := hookObj["hooks"].([]interface{})
	hookCmd := hookCmds[0].(map[string]interface{})
	timeout := hookCmd["timeout"].(float64)
	if timeout != 30000 {
		t.Errorf("AfterTool timeout = %v, want 30000", timeout)
	}
}

func TestDiagnoseHooks(t *testing.T) {
	a := &Agent{}

	t.Run("no settings file", func(t *testing.T) {
		tmpDir := t.TempDir()
		checks := a.DiagnoseHooks(tmpDir)
		if len(checks) == 0 {
			t.Fatal("Expected diagnostic checks")
		}
		if checks[0].OK {
			t.Error("Expected check to fail when no settings file")
		}
	})

	t.Run("with hooks configured", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := a.ConfigureHooks(tmpDir); err != nil {
			t.Fatalf("ConfigureHooks() error: %v", err)
		}
		checks := a.DiagnoseHooks(tmpDir)
		allOK := true
		for _, c := range checks {
			if !c.OK {
				allOK = false
				t.Errorf("Check %q failed: %s", c.Name, c.Message)
			}
		}
		if !allOK {
			t.Error("Expected all checks to pass after ConfigureHooks")
		}
	})
}

func TestEncodeProjectPath(t *testing.T) {
	tests := []struct {
		path string
	}{
		{"/home/user/project"},
		{"/tmp/test"},
	}
	for _, tc := range tests {
		got := EncodeProjectPath(tc.path)
		// Should be a valid SHA256 hex string (64 chars)
		if len(got) != 64 {
			t.Errorf("EncodeProjectPath(%q) = %q, expected 64-char hex hash", tc.path, got)
		}
		// Verify it matches SHA256
		h := sha256.Sum256([]byte(tc.path))
		want := fmt.Sprintf("%x", h)
		if got != want {
			t.Errorf("EncodeProjectPath(%q) = %q, want %q", tc.path, got, want)
		}
	}
}

func TestReadProjectsRegistry(t *testing.T) {
	t.Run("valid registry", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)

		geminiDir := filepath.Join(tmpDir, ".gemini")
		if err := os.MkdirAll(geminiDir, 0700); err != nil {
			t.Fatal(err)
		}
		registryJSON := `{
			"projects": {
				"/home/user/myproject": {"slug": "myproject-abc123"},
				"/tmp/other": {"slug": "other-def456"}
			}
		}`
		if err := os.WriteFile(filepath.Join(geminiDir, "projects.json"), []byte(registryJSON), 0600); err != nil {
			t.Fatal(err)
		}

		reg, err := ReadProjectsRegistry()
		if err != nil {
			t.Fatalf("ReadProjectsRegistry() error: %v", err)
		}
		if reg == nil {
			t.Fatal("ReadProjectsRegistry() returned nil")
		}
		if len(reg.Projects) != 2 {
			t.Fatalf("Expected 2 projects, got %d", len(reg.Projects))
		}
		if reg.Projects["/home/user/myproject"].Slug != "myproject-abc123" {
			t.Errorf("Slug = %q, want %q", reg.Projects["/home/user/myproject"].Slug, "myproject-abc123")
		}
	})

	t.Run("missing file returns nil", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)

		reg, err := ReadProjectsRegistry()
		if err != nil {
			t.Fatalf("ReadProjectsRegistry() error: %v", err)
		}
		if reg != nil {
			t.Errorf("Expected nil registry when file missing, got %+v", reg)
		}
	})
}

func TestGetSlugForProject(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	geminiDir := filepath.Join(tmpDir, ".gemini")
	if err := os.MkdirAll(geminiDir, 0700); err != nil {
		t.Fatal(err)
	}
	registryJSON := `{"projects": {"/home/user/proj": {"slug": "proj-slug"}}}`
	if err := os.WriteFile(filepath.Join(geminiDir, "projects.json"), []byte(registryJSON), 0600); err != nil {
		t.Fatal(err)
	}

	if slug := GetSlugForProject("/home/user/proj"); slug != "proj-slug" {
		t.Errorf("GetSlugForProject() = %q, want %q", slug, "proj-slug")
	}
	if slug := GetSlugForProject("/unknown"); slug != "" {
		t.Errorf("GetSlugForProject(unknown) = %q, want empty", slug)
	}
}

func TestGetSessionDir_SlugVsHash(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	projectPath := "/home/user/myproject"

	t.Run("without registry uses hash", func(t *testing.T) {
		dir, err := GetSessionDir(projectPath)
		if err != nil {
			t.Fatalf("GetSessionDir() error: %v", err)
		}
		hash := EncodeProjectPath(projectPath)
		want := filepath.Join(tmpDir, ".gemini", "tmp", hash, "chats")
		if dir != want {
			t.Errorf("GetSessionDir() = %q, want %q", dir, want)
		}
	})

	t.Run("with registry uses slug", func(t *testing.T) {
		geminiDir := filepath.Join(tmpDir, ".gemini")
		if err := os.MkdirAll(geminiDir, 0700); err != nil {
			t.Fatal(err)
		}
		registryJSON := fmt.Sprintf(`{"projects": {%q: {"slug": "myproj-abc"}}}`, projectPath)
		if err := os.WriteFile(filepath.Join(geminiDir, "projects.json"), []byte(registryJSON), 0600); err != nil {
			t.Fatal(err)
		}

		dir, err := GetSessionDir(projectPath)
		if err != nil {
			t.Fatalf("GetSessionDir() error: %v", err)
		}
		want := filepath.Join(tmpDir, ".gemini", "tmp", "myproj-abc", "chats")
		if dir != want {
			t.Errorf("GetSessionDir() = %q, want %q", dir, want)
		}
	})
}

func TestScanAllProjectDirs(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectPath := "/tmp/test-project"
	projectHash := EncodeProjectPath(projectPath)

	t.Run("finds session in slug-based dir via projectHash", func(t *testing.T) {
		// Create a slug-based dir (v0.29 style) — NOT the hash dir
		slugDir := filepath.Join(tmpHome, ".gemini", "tmp", "my-project-slug123", "chats")
		if err := os.MkdirAll(slugDir, 0755); err != nil {
			t.Fatal(err)
		}
		sessionContent := fmt.Sprintf(`{"sessionId":"uuid-1234","projectHash":%q,"messages":[]}`, projectHash)
		sessionFile := filepath.Join(slugDir, "session-2026-01-01T00-00-abc12345.json")
		if err := os.WriteFile(sessionFile, []byte(sessionContent), 0644); err != nil {
			t.Fatal(err)
		}

		info, err := ScanAllProjectDirs(projectPath)
		if err != nil {
			t.Fatalf("ScanAllProjectDirs() error: %v", err)
		}
		if info == nil {
			t.Fatal("ScanAllProjectDirs() returned nil, expected session")
		}
		if info.SessionID != "uuid-1234" {
			t.Errorf("SessionID = %q, want %q", info.SessionID, "uuid-1234")
		}
		if info.TranscriptPath != sessionFile {
			t.Errorf("TranscriptPath = %q, want %q", info.TranscriptPath, sessionFile)
		}
	})

	t.Run("finds session in hash-based dir", func(t *testing.T) {
		hashDir := filepath.Join(tmpHome, ".gemini", "tmp", projectHash, "chats")
		if err := os.MkdirAll(hashDir, 0755); err != nil {
			t.Fatal(err)
		}
		sessionFile := filepath.Join(hashDir, "session-2026-01-01T00-00-hash5678.json")
		if err := os.WriteFile(sessionFile, []byte(`{"messages":[]}`), 0644); err != nil {
			t.Fatal(err)
		}

		info, err := ScanAllProjectDirs(projectPath)
		if err != nil {
			t.Fatalf("ScanAllProjectDirs() error: %v", err)
		}
		if info == nil {
			t.Fatal("ScanAllProjectDirs() returned nil, expected session")
		}
		if info.TranscriptPath != sessionFile {
			t.Errorf("TranscriptPath = %q, want %q", info.TranscriptPath, sessionFile)
		}
	})

	t.Run("ignores sessions for other projects", func(t *testing.T) {
		otherTmpHome := t.TempDir()
		t.Setenv("HOME", otherTmpHome)

		otherHash := EncodeProjectPath("/tmp/other-project")
		otherDir := filepath.Join(otherTmpHome, ".gemini", "tmp", "other-slug", "chats")
		if err := os.MkdirAll(otherDir, 0755); err != nil {
			t.Fatal(err)
		}
		sessionContent := fmt.Sprintf(`{"sessionId":"other-session","projectHash":%q,"messages":[]}`, otherHash)
		if err := os.WriteFile(filepath.Join(otherDir, "session-other.json"), []byte(sessionContent), 0644); err != nil {
			t.Fatal(err)
		}

		info, err := ScanAllProjectDirs(projectPath)
		if err != nil {
			t.Fatalf("ScanAllProjectDirs() error: %v", err)
		}
		if info != nil {
			t.Errorf("ScanAllProjectDirs() = %+v, want nil for mismatched project", info)
		}
	})

	t.Run("returns nil when no gemini tmp dir", func(t *testing.T) {
		emptyHome := t.TempDir()
		t.Setenv("HOME", emptyHome)

		info, err := ScanAllProjectDirs(projectPath)
		if err != nil {
			t.Fatalf("ScanAllProjectDirs() error: %v", err)
		}
		if info != nil {
			t.Errorf("ScanAllProjectDirs() = %+v, want nil", info)
		}
	})
}

func TestNormalizeGeminiType(t *testing.T) {
	tests := []struct {
		input string
		want  agent.MessageType
	}{
		{"user", agent.MessageTypeUser},
		{"gemini", agent.MessageTypeAssistant},
		{"model", agent.MessageTypeAssistant},
		{"assistant", agent.MessageTypeAssistant},
		{"system", agent.MessageTypeSystem},
		{"session_metadata", ""},
		{"unknown", ""},
	}
	for _, tc := range tests {
		got := agent.NormalizeRole(tc.input)
		if got != tc.want {
			t.Errorf("NormalizeRole(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
