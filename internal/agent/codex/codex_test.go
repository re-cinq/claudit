package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/re-cinq/shift-log/internal/agent"
)

func TestAgentName(t *testing.T) {
	a := &Agent{}
	if a.Name() != agent.Codex {
		t.Errorf("Name() = %q, want %q", a.Name(), agent.Codex)
	}
}

func TestAgentDisplayName(t *testing.T) {
	a := &Agent{}
	if a.DisplayName() != "Codex CLI" {
		t.Errorf("DisplayName() = %q, want %q", a.DisplayName(), "Codex CLI")
	}
}

func TestConfigureHooksIsNoop(t *testing.T) {
	a := &Agent{}
	tmpDir := t.TempDir()

	if err := a.ConfigureHooks(tmpDir); err != nil {
		t.Fatalf("ConfigureHooks() error: %v", err)
	}

	// Verify no files were created
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir() error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ConfigureHooks() created %d files, expected 0", len(entries))
	}
}

func TestParseHookInput(t *testing.T) {
	a := &Agent{}
	input := `{"session_id":"sess-1","transcript_path":"/tmp/rollout.jsonl","tool_name":"shell","tool_input":{"command":"git commit -m test"}}`

	hook, err := a.ParseHookInput([]byte(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error: %v", err)
	}
	if hook.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", hook.SessionID, "sess-1")
	}
	if hook.TranscriptPath != "/tmp/rollout.jsonl" {
		t.Errorf("TranscriptPath = %q, want %q", hook.TranscriptPath, "/tmp/rollout.jsonl")
	}
	if hook.ToolName != "shell" {
		t.Errorf("ToolName = %q, want %q", hook.ToolName, "shell")
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
		{"shell", "git commit -m fix", true},
		{"container.exec", "git commit -am msg", true},
		{"shell_command", "git-commit", true},
		{"shell", "ls -la", false},
		{"shell", "git status", false},
		{"read_file", "git commit -m test", false},
		{"write_file", "git commit -m test", false},
	}

	for _, tc := range tests {
		got := a.IsCommitCommand(tc.tool, tc.cmd)
		if got != tc.want {
			t.Errorf("IsCommitCommand(%q, %q) = %v, want %v", tc.tool, tc.cmd, got, tc.want)
		}
	}
}

func TestParseTranscript(t *testing.T) {
	a := &Agent{}
	rollout := strings.Join([]string{
		`{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"sess-1","cwd":"/tmp","cli_version":"1.0.0","model_provider":"openai"}}`,
		`{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]}}`,
		`{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi there!"}]}}`,
		`{"timestamp":"2025-01-01T00:00:03Z","type":"turn_context","payload":{}}`,
	}, "\n")

	transcript, err := a.ParseTranscript(strings.NewReader(rollout))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}

	// Should have 2 entries (session_meta and turn_context are skipped)
	if len(transcript.Entries) != 2 {
		t.Fatalf("Expected 2 entries, got %d", len(transcript.Entries))
	}

	// Check user message
	if transcript.Entries[0].Type != agent.MessageTypeUser {
		t.Errorf("Entry 0 type = %q, want %q", transcript.Entries[0].Type, agent.MessageTypeUser)
	}
	if transcript.Entries[0].Message.Content[0].Text != "Hello" {
		t.Errorf("Entry 0 text = %q, want %q", transcript.Entries[0].Message.Content[0].Text, "Hello")
	}

	// Check assistant message
	if transcript.Entries[1].Type != agent.MessageTypeAssistant {
		t.Errorf("Entry 1 type = %q, want %q", transcript.Entries[1].Type, agent.MessageTypeAssistant)
	}
	if transcript.Entries[1].Message.Content[0].Text != "Hi there!" {
		t.Errorf("Entry 1 text = %q, want %q", transcript.Entries[1].Message.Content[0].Text, "Hi there!")
	}
}

func TestParseTranscriptWithFunctionCalls(t *testing.T) {
	a := &Agent{}
	rollout := strings.Join([]string{
		`{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"sess-1","cwd":"/tmp"}}`,
		`{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Run ls"}]}}`,
		`{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"ls\",\"-la\"]}","call_id":"call_1"}}`,
		`{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"file1.txt\nfile2.txt"}}`,
	}, "\n")

	transcript, err := a.ParseTranscript(strings.NewReader(rollout))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}

	if len(transcript.Entries) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(transcript.Entries))
	}

	// Check function_call entry
	entry := transcript.Entries[1]
	if entry.Type != agent.MessageTypeAssistant {
		t.Errorf("function_call type = %q, want %q", entry.Type, agent.MessageTypeAssistant)
	}
	if entry.Message.Content[0].Type != "tool_use" {
		t.Errorf("function_call content type = %q, want tool_use", entry.Message.Content[0].Type)
	}
	if entry.Message.Content[0].Text != "shell" {
		t.Errorf("function_call tool name = %q, want shell", entry.Message.Content[0].Text)
	}

	// Check function_call_output entry
	output := transcript.Entries[2]
	if output.Type != agent.MessageTypeUser {
		t.Errorf("function_call_output type = %q, want %q", output.Type, agent.MessageTypeUser)
	}
	if output.Message.Content[0].Type != "tool_result" {
		t.Errorf("function_call_output content type = %q, want tool_result", output.Message.Content[0].Type)
	}
}

func TestParseTranscriptEmpty(t *testing.T) {
	a := &Agent{}
	transcript, err := a.ParseTranscript(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(transcript.Entries))
	}
}

func TestToolAliases(t *testing.T) {
	a := &Agent{}
	aliases := a.ToolAliases()
	if aliases["shell"] != "Bash" {
		t.Errorf("ToolAliases[shell] = %q, want Bash", aliases["shell"])
	}
	if aliases["container.exec"] != "Bash" {
		t.Errorf("ToolAliases[container.exec] = %q, want Bash", aliases["container.exec"])
	}
	if aliases["shell_command"] != "Bash" {
		t.Errorf("ToolAliases[shell_command] = %q, want Bash", aliases["shell_command"])
	}
}

func TestResumeCommand(t *testing.T) {
	a := &Agent{}
	bin, args := a.ResumeCommand("sess-123")
	if bin != "codex" {
		t.Errorf("ResumeCommand binary = %q, want codex", bin)
	}
	if len(args) != 2 || args[0] != "resume" || args[1] != "sess-123" {
		t.Errorf("ResumeCommand args = %v, want [resume sess-123]", args)
	}
}

func TestNormalizeCodexRole(t *testing.T) {
	tests := []struct {
		input string
		want  agent.MessageType
	}{
		{"user", agent.MessageTypeUser},
		{"assistant", agent.MessageTypeAssistant},
		{"system", agent.MessageTypeSystem},
		{"unknown", ""},
	}
	for _, tc := range tests {
		got := agent.NormalizeRole(tc.input)
		if got != tc.want {
			t.Errorf("NormalizeRole(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseSessionMeta(t *testing.T) {
	tmpDir := t.TempDir()
	rolloutPath := filepath.Join(tmpDir, "rollout.jsonl")

	content := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"abc-123","cwd":"/home/user/project","cli_version":"1.0.0","model_provider":"openai"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]}}
`
	if err := os.WriteFile(rolloutPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	meta, err := ParseSessionMeta(rolloutPath)
	if err != nil {
		t.Fatalf("ParseSessionMeta() error: %v", err)
	}
	if meta == nil {
		t.Fatal("ParseSessionMeta() returned nil")
	}
	if meta.ID != "abc-123" {
		t.Errorf("ID = %q, want %q", meta.ID, "abc-123")
	}
	if meta.CWD != "/home/user/project" {
		t.Errorf("CWD = %q, want %q", meta.CWD, "/home/user/project")
	}
	if meta.CLIVersion != "1.0.0" {
		t.Errorf("CLIVersion = %q, want %q", meta.CLIVersion, "1.0.0")
	}
}

func TestParseTranscriptExtractsModel(t *testing.T) {
	a := &Agent{}
	rollout := strings.Join([]string{
		`{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"sess-1","cwd":"/tmp","cli_version":"1.0.0","model_provider":"openai"}}`,
		`{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]}}`,
	}, "\n")

	transcript, err := a.ParseTranscript(strings.NewReader(rollout))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}

	if transcript.Model != "openai" {
		t.Errorf("Model = %q, want %q", transcript.Model, "openai")
	}
}

func TestParseTranscriptNoSessionMeta(t *testing.T) {
	a := &Agent{}
	rollout := `{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]}}`

	transcript, err := a.ParseTranscript(strings.NewReader(rollout))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}

	if transcript.Model != "" {
		t.Errorf("Model = %q, want empty", transcript.Model)
	}
}

func TestGetCodexHome(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		// Unset CODEX_HOME to test default
		orig := os.Getenv("CODEX_HOME")
		os.Unsetenv("CODEX_HOME")
		defer os.Setenv("CODEX_HOME", orig)

		home, err := GetCodexHome()
		if err != nil {
			t.Fatalf("GetCodexHome() error: %v", err)
		}
		if !strings.HasSuffix(home, ".codex") {
			t.Errorf("GetCodexHome() = %q, expected to end with .codex", home)
		}
	})

	t.Run("with CODEX_HOME", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "/custom/codex")

		home, err := GetCodexHome()
		if err != nil {
			t.Fatalf("GetCodexHome() error: %v", err)
		}
		if home != "/custom/codex" {
			t.Errorf("GetCodexHome() = %q, want /custom/codex", home)
		}
	})
}
