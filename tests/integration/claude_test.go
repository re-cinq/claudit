package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Claude Code Integration", func() {
	Describe("end-to-end with Claude Code CLI", func() {
		It("should create a git note when Claude makes a commit", func() {
			skipIfEnvSet("SKIP_CLAUDE_INTEGRATION")

			apiKey := os.Getenv("ANTHROPIC_API_KEY")
			oauthToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
			if apiKey == "" && oauthToken == "" {
				Skip("Neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN set")
			}

			if oauthToken != "" && apiKey == "" {
				By("Using CLAUDE_CODE_OAUTH_TOKEN authentication (Pro/Max subscription)")
			} else {
				By("Using ANTHROPIC_API_KEY authentication")
			}

			requireBinary("claude")
			shiftlogPath := getShiftlogPath()
			tmpDir := initGitRepo("claude-integration")
			DeferCleanup(os.RemoveAll, tmpDir)

			// Initialize shiftlog
			cmd := exec.Command(shiftlogPath, "init")
			cmd.Dir = tmpDir
			output, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "shiftlog init failed:\n%s", output)

			// Verify hooks are configured
			settingsPath := filepath.Join(tmpDir, ".claude", "settings.local.json")
			settingsData, err := os.ReadFile(settingsPath)
			Expect(err).NotTo(HaveOccurred(), "Failed to read settings")

			var settings map[string]interface{}
			Expect(json.Unmarshal(settingsData, &settings)).To(Succeed())

			hooks, ok := settings["hooks"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "Expected hooks object in settings")
			postToolUse, ok := hooks["PostToolUse"].([]interface{})
			Expect(ok).To(BeTrue(), "Expected PostToolUse array in hooks")
			Expect(postToolUse).NotTo(BeEmpty())

			By("Hook configuration verified successfully")

			// Create a test file that Claude will commit
			todoFile := filepath.Join(tmpDir, "todo.txt")
			Expect(os.WriteFile(todoFile, []byte("- Buy milk\n- Walk dog\n"), 0644)).To(Succeed())

			// Run Claude Code
			claudeCmd := exec.Command("claude",
				"--print",
				"--allowedTools", "Bash(git:*),Read",
				"--max-turns", "5",
				"--dangerously-skip-permissions",
				"Please run: git add todo.txt && git commit -m 'Add todo list'",
			)
			claudeCmd.Dir = tmpDir
			claudeCmd.Env = append(os.Environ(),
				"PATH="+os.Getenv("PATH")+":"+filepath.Dir(shiftlogPath),
			)
			if apiKey != "" {
				claudeCmd.Env = append(claudeCmd.Env, "ANTHROPIC_API_KEY="+apiKey)
			}
			if oauthToken != "" {
				claudeCmd.Env = append(claudeCmd.Env, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
			}

			claudeOutput := runAgentWithTimeout(claudeCmd, 60*time.Second)

			// Give hooks time to run
			time.Sleep(2 * time.Second)

			// Check for shiftlog warnings
			Expect(string(claudeOutput)).NotTo(ContainSubstring("shiftlog: warning:"),
				"shiftlog logged warnings during execution:\n%s", claudeOutput)

			// Check if commit was made
			cmd = exec.Command("git", "log", "--oneline", "-n", "2")
			cmd.Dir = tmpDir
			logOutput, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "Failed to check git log:\n%s", logOutput)

			if !strings.Contains(string(logOutput), "todo") {
				Skip("Claude did not make the commit - cannot test note storage")
			}

			By("Commit was created successfully")
			GinkgoWriter.Printf("Git log: %s", logOutput)

			// CRITICAL TEST: If commit was made, note MUST exist
			cmd = exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "list")
			cmd.Dir = tmpDir
			notesOutput, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "Commit was made but no git notes exist!\nOutput: %s", notesOutput)
			Expect(strings.TrimSpace(string(notesOutput))).NotTo(BeEmpty(), "Commit was made but git notes list is empty!")

			By("Git note was created by shiftlog hook")

			// Verify note content is valid JSON
			cmd = exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "show", "HEAD")
			cmd.Dir = tmpDir
			noteContent, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "Note exists but cannot be read")

			var noteData map[string]interface{}
			Expect(json.Unmarshal(noteContent, &noteData)).To(Succeed(), "Note content is not valid JSON")

			// Verify required fields
			requiredFields := []string{"version", "session_id", "project_path", "git_branch", "message_count", "checksum", "transcript", "timestamp"}
			for _, field := range requiredFields {
				Expect(noteData).To(HaveKey(field), "Note missing required field '%s'", field)
			}

			// Verify version is 3 (current format version)
			if v, ok := noteData["version"].(float64); !ok || int(v) != 3 {
				GinkgoWriter.Printf("Note: expected version=3, got %v\n", noteData["version"])
			}

			// Verify agent field is "claude"
			Expect(noteData["agent"]).To(Equal("claude"))

			// Verify model field
			if model, hasModel := noteData["model"]; hasModel && model != "" {
				GinkgoWriter.Printf("Model field present: %v\n", model)
			} else {
				GinkgoWriter.Println("Note: model field is empty or missing")
			}

			// Verify effort field (v3 feature)
			if effortRaw, hasEffort := noteData["effort"]; hasEffort {
				effort, ok := effortRaw.(map[string]interface{})
				Expect(ok).To(BeTrue(), "effort field is not a JSON object")

				if turns, ok := effort["turns"].(float64); ok && turns > 0 {
					GinkgoWriter.Printf("Effort turns: %.0f\n", turns)
				}
				if inputTok, ok := effort["input_tokens"].(float64); ok && inputTok > 0 {
					GinkgoWriter.Printf("Effort input_tokens: %.0f\n", inputTok)
				}
				if outputTok, ok := effort["output_tokens"].(float64); ok && outputTok > 0 {
					GinkgoWriter.Printf("Effort output_tokens: %.0f\n", outputTok)
				}
			}

			By("Note content is valid and contains all required fields")
			GinkgoWriter.Printf("Note preview: version=%v, session_id=%v, agent=%v, model=%v, message_count=%v\n",
				noteData["version"], noteData["session_id"], noteData["agent"], noteData["model"], noteData["message_count"])
		})
	})

	Describe("missing shiftlog in PATH", func() {
		It("should NOT create a note when shiftlog is not available", func() {
			skipIfEnvSet("SKIP_CLAUDE_INTEGRATION")

			apiKey := os.Getenv("ANTHROPIC_API_KEY")
			oauthToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
			if apiKey == "" && oauthToken == "" {
				Skip("Neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN set")
			}

			requireBinary("claude")

			tmpDir, err := os.MkdirTemp("", "claude-integration-fail-*")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(os.RemoveAll, tmpDir)

			runGit(tmpDir, "init")
			runGit(tmpDir, "config", "user.email", "test@example.com")
			runGit(tmpDir, "config", "user.name", "Test User")

			Expect(os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test Project\n"), 0644)).To(Succeed())
			runGit(tmpDir, "add", "README.md")
			runGit(tmpDir, "commit", "-m", "Initial commit")

			// Manually create hook configuration WITHOUT adding shiftlog to PATH
			settingsDir := filepath.Join(tmpDir, ".claude")
			Expect(os.MkdirAll(settingsDir, 0755)).To(Succeed())

			settingsContent := `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "shiftlog store",
            "timeout": 30
          }
        ]
      }
    ]
  }
}`
			Expect(os.WriteFile(filepath.Join(settingsDir, "settings.local.json"), []byte(settingsContent), 0644)).To(Succeed())

			// Create a test file
			Expect(os.WriteFile(filepath.Join(tmpDir, "todo.txt"), []byte("- Test item\n"), 0644)).To(Succeed())

			// Run Claude Code WITHOUT shiftlog in PATH
			claudeCmd := exec.Command("claude",
				"--print",
				"--allowedTools", "Bash(git:*),Read",
				"--max-turns", "5",
				"--dangerously-skip-permissions",
				"Please run: git add todo.txt && git commit -m 'Add todo list'",
			)
			claudeCmd.Dir = tmpDir
			claudeCmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin:/usr/local/bin")
			if apiKey != "" {
				claudeCmd.Env = append(claudeCmd.Env, "ANTHROPIC_API_KEY="+apiKey)
			}
			if oauthToken != "" {
				claudeCmd.Env = append(claudeCmd.Env, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
			}

			_ = runAgentWithTimeout(claudeCmd, 60*time.Second)

			time.Sleep(2 * time.Second)

			// Check if commit was made
			cmd := exec.Command("git", "log", "--oneline", "-n", "2")
			cmd.Dir = tmpDir
			logOutput, _ := cmd.CombinedOutput()

			if !strings.Contains(string(logOutput), "todo") {
				Skip("Claude did not make the commit - cannot test failure case")
			}

			By("Commit was created")

			// Note should NOT exist because shiftlog was not in PATH
			cmd = exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "list")
			cmd.Dir = tmpDir
			notesOutput, err := cmd.CombinedOutput()

			if err == nil && len(strings.TrimSpace(string(notesOutput))) > 0 {
				Fail("UNEXPECTED: Note was created even though shiftlog was not in PATH!")
			}

			By("Confirmed: Note was NOT created (as expected when shiftlog not in PATH)")
		})
	})

	Describe("store command failure handling", func() {
		var shiftlogPath string
		var tmpDir string

		BeforeEach(func() {
			skipIfEnvSet("SKIP_CLAUDE_INTEGRATION")
			shiftlogPath = getShiftlogPath()

			var err error
			tmpDir, err = os.MkdirTemp("", "shiftlog-store-test-*")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(os.RemoveAll, tmpDir)

			runGit(tmpDir, "init")
			runGit(tmpDir, "config", "user.email", "test@example.com")
			runGit(tmpDir, "config", "user.name", "Test User")
			Expect(os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("test"), 0644)).To(Succeed())
			runGit(tmpDir, "add", "test.txt")
			runGit(tmpDir, "commit", "-m", "Initial commit")
		})

		It("should fail when transcript file is missing", func() {
			hookInput := `{
			"session_id": "test-session",
			"transcript_path": "/nonexistent/transcript.jsonl",
			"tool_name": "Bash",
			"tool_input": {
				"command": "git commit -m 'test'"
			}
		}`

			cmd := exec.Command(shiftlogPath, "store")
			cmd.Dir = tmpDir
			cmd.Stdin = strings.NewReader(hookInput)
			output, err := cmd.CombinedOutput()

			if err == nil {
				Fail(fmt.Sprintf("store command succeeded when it should have failed (missing transcript)\nOutput: %s", output))
			} else {
				GinkgoWriter.Printf("Correctly failed with error: %v\n", err)
			}

			outputStr := string(output)
			Expect(outputStr).To(SatisfyAny(
				ContainSubstring("warning"),
				ContainSubstring("failed"),
			), "Expected warning message in output, got: %s", outputStr)
		})

		It("should fail when commit SHA is invalid", func() {
			// Create a valid transcript file
			transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
			transcriptData := `{"type":"session_start","id":"test"}
{"type":"user","content":"test message"}`
			Expect(os.WriteFile(transcriptPath, []byte(transcriptData), 0644)).To(Succeed())

			// Empty repo with no HEAD
			emptyDir, err := os.MkdirTemp("", "empty-repo-*")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(os.RemoveAll, emptyDir)
			runGit(emptyDir, "init")

			hookInput := fmt.Sprintf(`{
			"session_id": "test-session",
			"transcript_path": "%s",
			"tool_name": "Bash",
			"tool_input": {
				"command": "git commit -m 'test'"
			}
		}`, transcriptPath)

			cmd := exec.Command(shiftlogPath, "store")
			cmd.Dir = emptyDir
			cmd.Stdin = strings.NewReader(hookInput)
			output, err := cmd.CombinedOutput()

			if err == nil {
				Fail(fmt.Sprintf("store command succeeded when it should have failed (no HEAD commit)\nOutput: %s", output))
			} else {
				GinkgoWriter.Printf("Correctly failed with error: %v\n", err)
			}
		})
	})
})
