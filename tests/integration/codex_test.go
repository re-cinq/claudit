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

var _ = Describe("Codex CLI Integration", func() {
	It("should create a git note when Codex makes a commit", func() {
		skipIfEnvSet("SKIP_CODEX_INTEGRATION")
		apiKey := requireEnvVar("OPENAI_API_KEY")
		requireBinary("codex")
		shiftlogPath := getShiftlogPath()
		tmpDir := initGitRepo("codex-integration")
		DeferCleanup(os.RemoveAll, tmpDir)

		// Initialize shiftlog with Codex agent
		cmd := exec.Command(shiftlogPath, "init", "--agent=codex")
		cmd.Dir = tmpDir
		output, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "shiftlog init --agent=codex failed:\n%s", output)

		// Verify no agent-specific config files were created (hookless agent)
		for _, path := range []string{".codex/settings.json", ".claude/settings.local.json"} {
			_, err := os.Stat(filepath.Join(tmpDir, path))
			Expect(err).To(HaveOccurred(), "Unexpected config file created: %s", path)
		}

		// Verify git hooks were installed
		hookPath := filepath.Join(tmpDir, ".git", "hooks", "post-commit")
		Expect(hookPath).To(BeAnExistingFile())

		By("Hookless init verified — git hooks installed, no agent config files")

		// Create a test file
		Expect(os.WriteFile(filepath.Join(tmpDir, "todo.txt"), []byte("- Buy milk\n- Walk dog\n"), 0644)).To(Succeed())

		// Login Codex CLI
		loginCmd := exec.Command("bash", "-c",
			fmt.Sprintf("echo %q | codex login --with-api-key", apiKey))
		loginCmd.Dir = tmpDir
		loginOutput, err := loginCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "codex login failed:\n%s", loginOutput)

		// Run Codex CLI
		codexCmd := exec.Command("codex", "exec",
			"--dangerously-bypass-approvals-and-sandbox",
			"Please run: git add todo.txt && git commit -m 'Add todo list'",
		)
		codexCmd.Dir = tmpDir
		codexCmd.Env = append(os.Environ(),
			"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
			"OPENAI_API_KEY="+apiKey,
		)

		codexOutput := runAgentWithTimeout(codexCmd, 90*time.Second)

		time.Sleep(2 * time.Second)

		Expect(string(codexOutput)).NotTo(ContainSubstring("shiftlog: warning:"),
			"shiftlog logged warnings during execution:\n%s", codexOutput)

		// Check if commit was made
		cmd = exec.Command("git", "log", "--oneline", "-n", "2")
		cmd.Dir = tmpDir
		logOutput, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Failed to check git log:\n%s", logOutput)

		if !strings.Contains(string(logOutput), "todo") {
			Skip("Codex did not make the commit - cannot test note storage")
		}

		By("Commit was created successfully")

		// Verify note
		cmd = exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "list")
		cmd.Dir = tmpDir
		notesOutput, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Commit was made but no git notes exist!\nOutput: %s", notesOutput)
		Expect(strings.TrimSpace(string(notesOutput))).NotTo(BeEmpty())

		cmd = exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "show", "HEAD")
		cmd.Dir = tmpDir
		noteContent, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred())

		var noteData map[string]interface{}
		Expect(json.Unmarshal(noteContent, &noteData)).To(Succeed())

		requiredFields := []string{"version", "session_id", "project_path", "git_branch", "message_count", "checksum", "transcript", "timestamp"}
		for _, field := range requiredFields {
			Expect(noteData).To(HaveKey(field))
		}
		Expect(noteData["agent"]).To(Equal("codex"))

		By("Note content is valid and contains all required fields")
	})
})
