package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/re-cinq/shift-log/internal/agent"
)

func TestAgentName(t *testing.T) {
	a := &Agent{}
	if a.Name() != agent.Copilot {
		t.Errorf("Name() = %q, want %q", a.Name(), agent.Copilot)
	}
}

func TestAgentDisplayName(t *testing.T) {
	a := &Agent{}
	if a.DisplayName() != "Copilot CLI" {
		t.Errorf("DisplayName() = %q, want %q", a.DisplayName(), "Copilot CLI")
	}
}

func TestParseHookInput(t *testing.T) {
	a := &Agent{}
	// Real Copilot native format: toolArgs is a JSON object, not a string
	input := `{"timestamp":1700000000,"cwd":"/tmp/project","toolName":"bash","toolArgs":{"command":"git commit -m test"}}`

	hook, err := a.ParseHookInput([]byte(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error: %v", err)
	}
	if hook.ToolName != "bash" {
		t.Errorf("ToolName = %q, want %q", hook.ToolName, "bash")
	}
	if hook.Command != "git commit -m test" {
		t.Errorf("Command = %q, want %q", hook.Command, "git commit -m test")
	}
}

func TestParseHookInputStringToolArgs(t *testing.T) {
	a := &Agent{}
	// Backwards compat: toolArgs as JSON string
	input := `{"timestamp":1700000000,"cwd":"/tmp/project","toolName":"bash","toolArgs":"{\"command\":\"git commit -m test\"}"}`

	hook, err := a.ParseHookInput([]byte(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error: %v", err)
	}
	if hook.ToolName != "bash" {
		t.Errorf("ToolName = %q, want %q", hook.ToolName, "bash")
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
		{"bash", "git commit -m fix", true},
		{"bash", "git commit -am msg", true},
		{"bash", "git-commit", true},
		{"bash", "ls -la", false},
		{"bash", "git status", false},
		{"view", "git commit -m test", false},
		{"edit", "git commit -m test", false},
		{"create", "git commit -m test", false},
	}

	for _, tc := range tests {
		got := a.IsCommitCommand(tc.tool, tc.cmd)
		if got != tc.want {
			t.Errorf("IsCommitCommand(%q, %q) = %v, want %v", tc.tool, tc.cmd, got, tc.want)
		}
	}
}

func TestParseCopilotTranscript(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"session.start","data":{}}`,
		`{"type":"user.message","data":{"content":"Hello"}}`,
		`{"type":"assistant.message","data":{"message":"Hi there"}}`,
		`{"type":"user.message","data":{"content":"Thanks"}}`,
	}, "\n")

	transcript, err := parseCopilotTranscript(strings.NewReader(events))
	if err != nil {
		t.Fatalf("parseCopilotTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(transcript.Entries))
	}

	if transcript.Entries[0].Type != agent.MessageTypeUser {
		t.Errorf("Entry 0 type = %q, want %q", transcript.Entries[0].Type, agent.MessageTypeUser)
	}
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

func TestParseCopilotTranscriptWithToolCalls(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"user.message","data":{"content":"Run ls"}}`,
		`{"type":"assistant.message","data":{"message":"Running command","toolRequests":[{"id":"call_1","name":"bash","input":{"command":"ls -la"}}]}}`,
		`{"type":"tool.execution_start","data":{"toolUseId":"call_1","toolName":"bash"}}`,
		`{"type":"tool.execution_complete","data":{"toolUseId":"call_1","toolName":"bash","result":"file1.txt\nfile2.txt"}}`,
	}, "\n")

	transcript, err := parseCopilotTranscript(strings.NewReader(events))
	if err != nil {
		t.Fatalf("parseCopilotTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(transcript.Entries))
	}

	// Check assistant entry has text and tool_use content blocks
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
	if entry.Message.Content[1].Name != "bash" {
		t.Errorf("Content[1] name = %q, want bash", entry.Message.Content[1].Name)
	}

	// Check tool result entry
	toolEntry := transcript.Entries[2]
	if toolEntry.Type != agent.MessageTypeUser {
		t.Errorf("Tool entry type = %q, want %q", toolEntry.Type, agent.MessageTypeUser)
	}
}

func TestParseCopilotTranscriptExtractsModel(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"session.start","data":{}}`,
		`{"type":"session.model_change","data":{"content":"gpt-4o"}}`,
		`{"type":"user.message","data":{"content":"Hello"}}`,
		`{"type":"assistant.message","data":{"message":"Hi there"}}`,
	}, "\n")

	transcript, err := parseCopilotTranscript(strings.NewReader(events))
	if err != nil {
		t.Fatalf("parseCopilotTranscript() error: %v", err)
	}

	if transcript.Model != "gpt-4o" {
		t.Errorf("Model = %q, want %q", transcript.Model, "gpt-4o")
	}

	// model_change events should not produce transcript entries
	if len(transcript.Entries) != 2 {
		t.Errorf("Expected 2 entries, got %d", len(transcript.Entries))
	}
}

func TestParseCopilotTranscriptNoModel(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"session.start","data":{}}`,
		`{"type":"user.message","data":{"content":"Hello"}}`,
	}, "\n")

	transcript, err := parseCopilotTranscript(strings.NewReader(events))
	if err != nil {
		t.Fatalf("parseCopilotTranscript() error: %v", err)
	}

	if transcript.Model != "" {
		t.Errorf("Model = %q, want empty", transcript.Model)
	}
}

func TestParseCopilotTranscriptEmpty(t *testing.T) {
	transcript, err := parseCopilotTranscript(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parseCopilotTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(transcript.Entries))
	}
}

func TestParseCopilotTranscriptEmptyLines(t *testing.T) {
	events := "\n\n\n"
	transcript, err := parseCopilotTranscript(strings.NewReader(events))
	if err != nil {
		t.Fatalf("parseCopilotTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(transcript.Entries))
	}
}

func TestToolAliases(t *testing.T) {
	a := &Agent{}
	aliases := a.ToolAliases()
	if aliases["bash"] != "Bash" {
		t.Errorf("ToolAliases[bash] = %q, want Bash", aliases["bash"])
	}
	if aliases["view"] != "Read" {
		t.Errorf("ToolAliases[view] = %q, want Read", aliases["view"])
	}
	if aliases["edit"] != "Edit" {
		t.Errorf("ToolAliases[edit] = %q, want Edit", aliases["edit"])
	}
	if aliases["write"] != "Write" {
		t.Errorf("ToolAliases[write] = %q, want Write", aliases["write"])
	}
	if aliases["create"] != "Write" {
		t.Errorf("ToolAliases[create] = %q, want Write", aliases["create"])
	}
}

func TestResumeCommand(t *testing.T) {
	a := &Agent{}
	bin, args := a.ResumeCommand("sess-123")
	if bin != "copilot" {
		t.Errorf("ResumeCommand binary = %q, want copilot", bin)
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

	hooksPath := filepath.Join(tmpDir, ".github", "hooks", "shiftlog.json")
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("Failed to read .github/hooks/shiftlog.json: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}

	// Check version
	if v, ok := raw["version"].(float64); !ok || int(v) != 1 {
		t.Errorf("version = %v, want 1", raw["version"])
	}

	hooks, ok := raw["hooks"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing hooks key")
	}

	postToolUse, ok := hooks["postToolUse"].([]interface{})
	if !ok || len(postToolUse) == 0 {
		t.Fatal("Missing postToolUse hook")
	}

	hookObj := postToolUse[0].(map[string]interface{})
	if hookObj["type"] != "command" {
		t.Errorf("hook type = %q, want command", hookObj["type"])
	}
	if hookObj["command"] != "shiftlog store --agent=copilot" {
		t.Errorf("hook command = %q, want 'shiftlog store --agent=copilot'", hookObj["command"])
	}
	if timeout, ok := hookObj["timeoutSec"].(float64); !ok || timeout != 30 {
		t.Errorf("hook timeoutSec = %v, want 30", hookObj["timeoutSec"])
	}
}

func TestConfigureHooksIdempotent(t *testing.T) {
	a := &Agent{}
	tmpDir := t.TempDir()

	if err := a.ConfigureHooks(tmpDir); err != nil {
		t.Fatalf("ConfigureHooks() first call error: %v", err)
	}
	if err := a.ConfigureHooks(tmpDir); err != nil {
		t.Fatalf("ConfigureHooks() second call error: %v", err)
	}

	hooksPath := filepath.Join(tmpDir, ".github", "hooks", "shiftlog.json")
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("Failed to read .github/hooks/shiftlog.json: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}

	hooks := raw["hooks"].(map[string]interface{})
	postToolUse := hooks["postToolUse"].([]interface{})
	if len(postToolUse) != 1 {
		t.Errorf("Expected 1 postToolUse entry, got %d", len(postToolUse))
	}
}

func TestConfigureHooksPreservesExisting(t *testing.T) {
	a := &Agent{}
	tmpDir := t.TempDir()

	// Write pre-existing hooks file with extra data
	hooksDir := filepath.Join(tmpDir, ".github", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	existing := `{"version":1,"hooks":{"postToolUse":[{"type":"command","command":"other-tool","timeoutSec":10}]},"customKey":"customValue"}`
	if err := os.WriteFile(filepath.Join(hooksDir, "shiftlog.json"), []byte(existing), 0644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	if err := a.ConfigureHooks(tmpDir); err != nil {
		t.Fatalf("ConfigureHooks() error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(hooksDir, "shiftlog.json"))
	if err != nil {
		t.Fatalf("Failed to read shiftlog.json: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}

	// Verify custom key preserved
	if raw["customKey"] != "customValue" {
		t.Errorf("customKey = %v, want customValue", raw["customKey"])
	}

	// Verify existing hook preserved and shiftlog hook added
	hooks := raw["hooks"].(map[string]interface{})
	postToolUse := hooks["postToolUse"].([]interface{})
	if len(postToolUse) != 2 {
		t.Errorf("Expected 2 postToolUse entries, got %d", len(postToolUse))
	}

	// First should be the existing hook
	first := postToolUse[0].(map[string]interface{})
	if first["command"] != "other-tool" {
		t.Errorf("First entry command = %q, want 'other-tool'", first["command"])
	}

	// Second should be the shiftlog hook
	second := postToolUse[1].(map[string]interface{})
	if second["command"] != "shiftlog store --agent=copilot" {
		t.Errorf("Second entry command = %q, want 'shiftlog store --agent=copilot'", second["command"])
	}
}

func TestDiagnoseHooks(t *testing.T) {
	a := &Agent{}

	t.Run("no hooks file", func(t *testing.T) {
		tmpDir := t.TempDir()
		checks := a.DiagnoseHooks(tmpDir)
		if len(checks) == 0 {
			t.Fatal("Expected diagnostic checks")
		}
		if checks[0].OK {
			t.Error("Expected check to fail when no hooks file")
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

func TestExtractCommand(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		toolArgs json.RawMessage
		want     string
	}{
		{"bash with command key (object)", "bash", json.RawMessage(`{"command":"git commit -m test"}`), "git commit -m test"},
		{"bash with cmd key (object)", "bash", json.RawMessage(`{"cmd":"ls -la"}`), "ls -la"},
		{"bash with command key (string)", "bash", json.RawMessage(`"{\"command\":\"git commit -m test\"}"`), "git commit -m test"},
		{"non-shell tool", "view", json.RawMessage(`{"path":"/some/file"}`), ""},
		{"empty toolArgs", "bash", nil, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCommand(tc.toolName, tc.toolArgs)
			if got != tc.want {
				t.Errorf("extractCommand(%q, %q) = %q, want %q", tc.toolName, tc.toolArgs, got, tc.want)
			}
		})
	}
}
