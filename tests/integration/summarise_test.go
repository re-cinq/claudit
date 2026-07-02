package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Summarise", func() {
	Describe("with Claude", func() {
		It("should produce a meaningful summary of a conversation", func() {
			skipIfEnvSet("SKIP_CLAUDE_INTEGRATION")

			apiKey := os.Getenv("ANTHROPIC_API_KEY")
			oauthToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
			if apiKey == "" && oauthToken == "" {
				Skip("Neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN set")
			}

			requireBinary("claude")
			shiftlogPath := getShiftlogPath()
			tmpDir := initGitRepo("summarise-integration")
			DeferCleanup(os.RemoveAll, tmpDir)

			// Initialize shiftlog
			cmd := exec.Command(shiftlogPath, "init")
			cmd.Dir = tmpDir
			output, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "shiftlog init failed:\n%s", output)

			pathEnv := "PATH=" + filepath.Dir(shiftlogPath) + ":" + os.Getenv("PATH")

			// Create a test file for Claude to commit
			Expect(os.WriteFile(filepath.Join(tmpDir, "todo.txt"),
				[]byte("- Buy groceries\n- Write tests\n- Deploy app\n"), 0644)).To(Succeed())

			// Run Claude Code to make a commit
			claudeCmd := exec.Command("claude",
				"--print",
				"--allowedTools", "Bash(git:*),Read",
				"--max-turns", "5",
				"--dangerously-skip-permissions",
				"Please run: git add todo.txt && git commit -m 'Add todo list'",
			)
			claudeCmd.Dir = tmpDir
			claudeCmd.Env = append(os.Environ(), pathEnv)
			if apiKey != "" {
				claudeCmd.Env = append(claudeCmd.Env, "ANTHROPIC_API_KEY="+apiKey)
			}
			if oauthToken != "" {
				claudeCmd.Env = append(claudeCmd.Env, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
			}

			_ = runAgentWithTimeout(claudeCmd, 60*time.Second)

			time.Sleep(2 * time.Second)

			// Check if commit was made
			cmd = exec.Command("git", "log", "--oneline", "-n", "2")
			cmd.Dir = tmpDir
			logOutput, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred())

			if !strings.Contains(string(logOutput), "todo") {
				Skip("Claude did not make the commit - cannot test summarise")
			}
			By("Commit with conversation was created")

			// Verify note exists
			cmd = exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "list")
			cmd.Dir = tmpDir
			notesOutput, err := cmd.CombinedOutput()
			if err != nil || len(strings.TrimSpace(string(notesOutput))) == 0 {
				Skip("No git note stored - cannot test summarise")
			}
			By("Git note exists, running summarise...")

			// Run shiftlog summarise
			summariseCmd := exec.Command(shiftlogPath, "summarise")
			summariseCmd.Dir = tmpDir
			summariseCmd.Env = append(os.Environ(), pathEnv)
			if apiKey != "" {
				summariseCmd.Env = append(summariseCmd.Env, "ANTHROPIC_API_KEY="+apiKey)
			}
			if oauthToken != "" {
				summariseCmd.Env = append(summariseCmd.Env, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
			}

			summariseOutput := runAgentWithTimeout(summariseCmd, 120*time.Second)

			summary := strings.TrimSpace(string(summariseOutput))
			Expect(summary).NotTo(BeEmpty(), "summarise returned empty output")

			By("Summarise returned non-empty output")

			keywords := []string{"todo", "commit", "git", "add", "file", "list"}
			foundKeyword := false
			lowerSummary := strings.ToLower(summary)
			for _, kw := range keywords {
				if strings.Contains(lowerSummary, kw) {
					foundKeyword = true
					break
				}
			}
			Expect(foundKeyword).To(BeTrue(),
				"Summary does not appear to reference the conversation content.\nExpected one of %v in summary:\n%s", keywords, summary)

			By("Summary contains relevant content from the conversation")
		})
	})

	Describe("tldr alias", func() {
		It("should resolve to the summarise command", func() {
			shiftlogPath := getShiftlogPath()
			tmpDir := initGitRepo("tldr-alias")
			DeferCleanup(os.RemoveAll, tmpDir)

			// Run `shiftlog tldr`
			cmd := exec.Command(shiftlogPath, "tldr")
			cmd.Dir = tmpDir
			output, err := cmd.CombinedOutput()

			Expect(err).To(HaveOccurred(), "Expected error (no conversation), but command succeeded")

			outStr := string(output)
			Expect(outStr).NotTo(SatisfyAny(
				ContainSubstring("unknown command"),
				ContainSubstring("unknown flag"),
			), "tldr alias not registered. Output: %s", outStr)

			Expect(outStr).To(ContainSubstring("no conversation found"))

			By("tldr alias correctly resolves to summarise command")
		})
	})

	Describe("unsupported agent", func() {
		It("should produce a helpful error", func() {
			shiftlogPath := getShiftlogPath()
			tmpDir := initGitRepo("summarise-unsupported")
			DeferCleanup(os.RemoveAll, tmpDir)

			// Store a minimal conversation
			transcriptData := `{"uuid":"u1","type":"user","message":{"role":"user","content":[{"type":"text","text":"Hello"}]}}
{"uuid":"a1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hi there!"}]}}`
			transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(transcriptData), 0644)).To(Succeed())

			hookInput := `{"session_id":"test-session","transcript_path":"` + transcriptPath + `","tool_name":"Bash","tool_input":{"command":"git commit -m 'test'"}}`
			storeCmd := exec.Command(shiftlogPath, "store")
			storeCmd.Dir = tmpDir
			storeCmd.Stdin = strings.NewReader(hookInput)
			storeCmd.Env = append(os.Environ(), "PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"))
			storeOutput, err := storeCmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "shiftlog store failed:\n%s", storeOutput)

			// Try to summarise with copilot (unsupported)
			cmd := exec.Command(shiftlogPath, "summarise", "--agent=copilot")
			cmd.Dir = tmpDir
			output, err := cmd.CombinedOutput()

			Expect(err).To(HaveOccurred(), "Expected error for unsupported agent, but command succeeded")

			outStr := string(output)
			Expect(outStr).To(ContainSubstring("does not support summarisation"))
			Expect(outStr).To(ContainSubstring("--agent=claude"))

			By("Unsupported agent produces helpful error with suggestion")
		})
	})
})
