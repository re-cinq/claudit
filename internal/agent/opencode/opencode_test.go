package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/re-cinq/shift-log/internal/agent"
)

func TestAgentName(t *testing.T) {
	a := &Agent{}
	if a.Name() != agent.OpenCode {
		t.Errorf("Name() = %q, want %q", a.Name(), agent.OpenCode)
	}
}

func TestAgentDisplayName(t *testing.T) {
	a := &Agent{}
	if a.DisplayName() != "OpenCode CLI" {
		t.Errorf("DisplayName() = %q, want %q", a.DisplayName(), "OpenCode CLI")
	}
}

func TestParseHookInput(t *testing.T) {
	a := &Agent{}
	input := `{
		"session_id": "sess-1",
		"data_dir": "/home/user/.local/share/opencode",
		"project_dir": "/home/user/project",
		"tool_name": "bash",
		"tool_input": {"command": "git commit -m test"}
	}`

	hook, err := a.ParseHookInput([]byte(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error: %v", err)
	}
	if hook.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", hook.SessionID, "sess-1")
	}
	if hook.ToolName != "bash" {
		t.Errorf("ToolName = %q, want %q", hook.ToolName, "bash")
	}
	if hook.Command != "git commit -m test" {
		t.Errorf("Command = %q, want %q", hook.Command, "git commit -m test")
	}
	// TranscriptPath should be constructed from data_dir + session_id
	expectedPath := filepath.Join("/home/user/.local/share/opencode", "storage", "message", "sess-1")
	if hook.TranscriptPath != expectedPath {
		t.Errorf("TranscriptPath = %q, want %q", hook.TranscriptPath, expectedPath)
	}
}

func TestIsCommitCommand(t *testing.T) {
	a := &Agent{}
	tests := []struct {
		tool, cmd string
		want      bool
	}{
		{"bash", "git commit -m fix", true},
		{"shell", "git commit -am msg", true},
		{"terminal", "git-commit", true},
		{"execute", "git commit --amend", true},
		{"run", "ls -la", false},
		{"bash", "git status", false},
		{"read", "git commit -m test", false},
	}

	for _, tc := range tests {
		got := a.IsCommitCommand(tc.tool, tc.cmd)
		if got != tc.want {
			t.Errorf("IsCommitCommand(%q, %q) = %v, want %v", tc.tool, tc.cmd, got, tc.want)
		}
	}
}

func TestParseTranscriptJSONL(t *testing.T) {
	a := &Agent{}
	lines := []string{
		`{"role":"user","id":"u1","content":"Hello"}`,
		`{"role":"assistant","id":"a1","content":"Hi there"}`,
		`{"role":"user","id":"u2","content":"Thanks"}`,
	}
	jsonl := strings.Join(lines, "\n")

	transcript, err := a.ParseTranscript(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(transcript.Entries))
	}

	if transcript.Entries[0].Type != agent.MessageTypeUser {
		t.Errorf("Entry 0 type = %q, want %q", transcript.Entries[0].Type, agent.MessageTypeUser)
	}
	if transcript.Entries[0].UUID != "u1" {
		t.Errorf("Entry 0 UUID = %q, want %q", transcript.Entries[0].UUID, "u1")
	}
	if transcript.Entries[1].Type != agent.MessageTypeAssistant {
		t.Errorf("Entry 1 type = %q, want %q", transcript.Entries[1].Type, agent.MessageTypeAssistant)
	}
}

func TestParseTranscriptJSONArray(t *testing.T) {
	a := &Agent{}
	jsonArray := `[
		{"role":"user","id":"u1","content":"Hello"},
		{"role":"assistant","id":"a1","content":"Response"}
	]`

	transcript, err := a.ParseTranscript(strings.NewReader(jsonArray))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 2 {
		t.Fatalf("Expected 2 entries, got %d", len(transcript.Entries))
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

func TestParseTranscriptWithContentBlocks(t *testing.T) {
	a := &Agent{}
	jsonl := `{"role":"assistant","id":"a1","content":[{"type":"text","text":"Hello world"}]}`

	transcript, err := a.ParseTranscript(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseTranscript() error: %v", err)
	}
	if len(transcript.Entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(transcript.Entries))
	}
	msg := transcript.Entries[0].Message
	if msg == nil {
		t.Fatal("Expected message to be non-nil")
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "Hello world" {
		t.Errorf("Message content = %v, want [{text Hello world}]", msg.Content)
	}
}

func TestParseTranscriptFile(t *testing.T) {
	a := &Agent{}

	t.Run("single file", func(t *testing.T) {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "transcript.jsonl")
		content := `{"role":"user","id":"u1","content":"Hello"}` + "\n" +
			`{"role":"assistant","id":"a1","content":"Hi"}`
		os.WriteFile(filePath, []byte(content), 0644)

		transcript, err := a.ParseTranscriptFile(filePath)
		if err != nil {
			t.Fatalf("ParseTranscriptFile() error: %v", err)
		}
		if len(transcript.Entries) != 2 {
			t.Errorf("Expected 2 entries, got %d", len(transcript.Entries))
		}
	})

	t.Run("directory with json files", func(t *testing.T) {
		tmpDir := t.TempDir()
		msgDir := filepath.Join(tmpDir, "messages")
		os.MkdirAll(msgDir, 0755)

		msg1 := map[string]interface{}{"role": "user", "id": "u1", "content": "Hello"}
		msg2 := map[string]interface{}{"role": "assistant", "id": "a1", "content": "Hi"}
		data1, _ := json.Marshal(msg1)
		data2, _ := json.Marshal(msg2)

		os.WriteFile(filepath.Join(msgDir, "001.json"), data1, 0644)
		os.WriteFile(filepath.Join(msgDir, "002.json"), data2, 0644)

		transcript, err := a.ParseTranscriptFile(msgDir)
		if err != nil {
			t.Fatalf("ParseTranscriptFile() error: %v", err)
		}
		if len(transcript.Entries) != 2 {
			t.Errorf("Expected 2 entries, got %d", len(transcript.Entries))
		}
	})
}

func TestToolAliases(t *testing.T) {
	a := &Agent{}
	aliases := a.ToolAliases()
	if aliases["bash"] != "Bash" {
		t.Errorf("ToolAliases[bash] = %q, want Bash", aliases["bash"])
	}
	if aliases["write"] != "Write" {
		t.Errorf("ToolAliases[write] = %q, want Write", aliases["write"])
	}
}

func TestResumeCommand(t *testing.T) {
	a := &Agent{}
	bin, args := a.ResumeCommand("sess-123")
	if bin != "opencode" {
		t.Errorf("ResumeCommand binary = %q, want opencode", bin)
	}
	if len(args) != 2 || args[0] != "--session" || args[1] != "sess-123" {
		t.Errorf("ResumeCommand args = %v, want [--session sess-123]", args)
	}
}

func TestConfigureHooks(t *testing.T) {
	a := &Agent{}
	tmpDir := t.TempDir()

	if err := a.ConfigureHooks(tmpDir); err != nil {
		t.Fatalf("ConfigureHooks() error: %v", err)
	}

	pluginPath := filepath.Join(tmpDir, ".opencode", "plugins", "shiftlog.js")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("Failed to read plugin: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "shiftlog store --agent=opencode") {
		t.Error("Plugin should contain 'shiftlog store --agent=opencode'")
	}
	if !strings.Contains(content, "tool.execute.after") {
		t.Error("Plugin should contain 'tool.execute.after'")
	}
}

func TestDiagnoseHooks(t *testing.T) {
	a := &Agent{}

	t.Run("no plugin", func(t *testing.T) {
		tmpDir := t.TempDir()
		checks := a.DiagnoseHooks(tmpDir)
		if len(checks) == 0 {
			t.Fatal("Expected diagnostic checks")
		}
		if checks[0].OK {
			t.Error("Expected check to fail when no plugin installed")
		}
	})

	t.Run("with plugin installed", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := a.ConfigureHooks(tmpDir); err != nil {
			t.Fatalf("ConfigureHooks() error: %v", err)
		}
		checks := a.DiagnoseHooks(tmpDir)
		for _, c := range checks {
			if !c.OK {
				t.Errorf("Check %q failed: %s", c.Name, c.Message)
			}
		}
	})
}

func TestHasPlugin(t *testing.T) {
	tmpDir := t.TempDir()

	if HasPlugin(tmpDir) {
		t.Error("HasPlugin() should be false before install")
	}

	InstallPlugin(tmpDir)

	if !HasPlugin(tmpDir) {
		t.Error("HasPlugin() should be true after install")
	}
}

func TestGetProjectID(t *testing.T) {
	tmpDir := t.TempDir()

	// Init git repo
	gitInit := exec.Command("git", "init")
	gitInit.Dir = tmpDir
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	gitConfig := exec.Command("git", "config", "user.email", "test@test.com")
	gitConfig.Dir = tmpDir
	gitConfig.CombinedOutput()

	gitConfig2 := exec.Command("git", "config", "user.name", "Test")
	gitConfig2.Dir = tmpDir
	gitConfig2.CombinedOutput()

	// Before any commits, should return "global"
	if id := GetProjectID(tmpDir); id != "global" {
		t.Errorf("GetProjectID before any commit = %q, want 'global'", id)
	}

	// Create initial commit
	os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("hello"), 0644)
	gitAdd := exec.Command("git", "add", ".")
	gitAdd.Dir = tmpDir
	gitAdd.CombinedOutput()

	gitCommit := exec.Command("git", "commit", "-m", "Initial")
	gitCommit.Dir = tmpDir
	gitCommit.CombinedOutput()

	// After commit, should return root commit hash
	gitRevList := exec.Command("git", "rev-list", "--max-parents=0", "--all")
	gitRevList.Dir = tmpDir
	rootOutput, _ := gitRevList.Output()
	expectedRoot := strings.TrimSpace(string(rootOutput))

	got := GetProjectID(tmpDir)
	if got != expectedRoot {
		t.Errorf("GetProjectID = %q, want root commit %q", got, expectedRoot)
	}

	// Add more commits — project ID should still be the root commit
	os.WriteFile(filepath.Join(tmpDir, "test2.txt"), []byte("world"), 0644)
	gitAdd2 := exec.Command("git", "add", ".")
	gitAdd2.Dir = tmpDir
	gitAdd2.CombinedOutput()

	gitCommit2 := exec.Command("git", "commit", "-m", "Second")
	gitCommit2.Dir = tmpDir
	gitCommit2.CombinedOutput()

	got2 := GetProjectID(tmpDir)
	if got2 != expectedRoot {
		t.Errorf("GetProjectID after 2nd commit = %q, want root commit %q", got2, expectedRoot)
	}
}

func TestGetDataDir(t *testing.T) {
	if runtime.GOOS != "darwin" {
		// XDG_DATA_HOME is only respected on non-darwin platforms
		t.Setenv("XDG_DATA_HOME", "/custom/data")
		dir, err := GetDataDir()
		if err != nil {
			t.Fatalf("GetDataDir with XDG_DATA_HOME error: %v", err)
		}
		if dir != "/custom/data/opencode" {
			t.Errorf("GetDataDir with XDG_DATA_HOME = %q, want /custom/data/opencode", dir)
		}
	}

	// Without XDG_DATA_HOME, should return default path containing "opencode"
	t.Setenv("XDG_DATA_HOME", "")
	dir, err := GetDataDir()
	if err != nil {
		t.Fatalf("GetDataDir without env error: %v", err)
	}
	if !strings.Contains(dir, "opencode") {
		t.Errorf("GetDataDir default = %q, should contain 'opencode'", dir)
	}
	if runtime.GOOS == "darwin" {
		if !strings.Contains(dir, "Application Support/opencode") {
			t.Errorf("GetDataDir default = %q, expected Application Support/opencode on macOS", dir)
		}
	} else {
		if !strings.HasSuffix(dir, ".local/share/opencode") {
			t.Errorf("GetDataDir default = %q, expected .local/share/opencode on Linux", dir)
		}
	}
}

func TestGetSessionDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Init git repo with a commit
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@t.com"},
		{"config", "user.name", "T"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		cmd.CombinedOutput()
	}
	os.WriteFile(filepath.Join(tmpDir, "f.txt"), []byte("x"), 0644)
	gitAdd := exec.Command("git", "add", ".")
	gitAdd.Dir = tmpDir
	gitAdd.CombinedOutput()
	gitCommit := exec.Command("git", "commit", "-m", "Init")
	gitCommit.Dir = tmpDir
	gitCommit.CombinedOutput()

	dir, err := GetSessionDir(tmpDir)
	if err != nil {
		t.Fatalf("GetSessionDir error: %v", err)
	}

	projectID := GetProjectID(tmpDir)

	// On darwin, GetDataDir ignores XDG_DATA_HOME and uses ~/Library/Application Support
	dataDir, err := GetDataDir()
	if err != nil {
		t.Fatalf("GetDataDir error: %v", err)
	}
	expected := filepath.Join(dataDir, "storage", "session", projectID)
	if dir != expected {
		t.Errorf("GetSessionDir = %q, want %q", dir, expected)
	}
}

func TestGetMessageDir(t *testing.T) {
	dir, err := GetMessageDir("sess-abc")
	if err != nil {
		t.Fatalf("GetMessageDir error: %v", err)
	}

	dataDir, err := GetDataDir()
	if err != nil {
		t.Fatalf("GetDataDir error: %v", err)
	}
	expected := filepath.Join(dataDir, "storage", "message", "sess-abc")
	if dir != expected {
		t.Errorf("GetMessageDir = %q, want %q", dir, expected)
	}
}

func TestNormalizeOpenCodeRole(t *testing.T) {
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
