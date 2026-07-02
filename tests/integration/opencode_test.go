package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("OpenCode CLI Integration", func() {
	It("should create a git note when OpenCode makes a commit", func() {
		skipIfEnvSet("SKIP_OPENCODE_INTEGRATION")

		geminiAPIKey := os.Getenv("GEMINI_API_KEY")
		googleGenAIKey := os.Getenv("GOOGLE_GENERATIVE_AI_API_KEY")
		if geminiAPIKey == "" && googleGenAIKey == "" {
			Skip("Neither GEMINI_API_KEY nor GOOGLE_GENERATIVE_AI_API_KEY set")
		}
		apiKey := googleGenAIKey
		if apiKey == "" {
			apiKey = geminiAPIKey
		}

		requireBinary("opencode")
		shiftlogPath := getShiftlogPath()
		tmpDir := initGitRepo("opencode-integration")
		DeferCleanup(os.RemoveAll, tmpDir)

		// Write opencode.json
		opencodeConfig := map[string]interface{}{
			"$schema":    "https://opencode.ai/config.json",
			"model":      "google/gemini-2.5-flash",
			"permission": "allow",
		}
		configData, err := json.MarshalIndent(opencodeConfig, "", "  ")
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(tmpDir, "opencode.json"), configData, 0644)).To(Succeed())

		// Initialize shiftlog with OpenCode agent
		cmd := exec.Command(shiftlogPath, "init", "--agent=opencode")
		cmd.Dir = tmpDir
		output, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "shiftlog init --agent=opencode failed:\n%s", output)

		// Verify plugin is installed
		pluginPath := filepath.Join(tmpDir, ".opencode", "plugins", "shiftlog.js")
		Expect(pluginPath).To(BeAnExistingFile())

		By("Plugin configuration verified successfully")

		// Create a test file
		Expect(os.WriteFile(filepath.Join(tmpDir, "todo.txt"), []byte("- Buy milk\n- Walk dog\n"), 0644)).To(Succeed())

		// Run OpenCode CLI
		opencodeCmd := exec.Command("opencode", "run",
			"Please run: git add todo.txt && git commit -m 'Add todo list'",
		)
		opencodeCmd.Dir = tmpDir
		opencodeCmd.Env = append(os.Environ(),
			"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
			"GOOGLE_GENERATIVE_AI_API_KEY="+apiKey,
		)

		opencodeOutput := runAgentWithTimeout(opencodeCmd, 90*time.Second)

		time.Sleep(2 * time.Second)

		Expect(string(opencodeOutput)).NotTo(ContainSubstring("shiftlog: warning:"),
			"shiftlog logged warnings during execution:\n%s", opencodeOutput)

		// Check if commit was made
		cmd = exec.Command("git", "log", "--oneline", "-n", "2")
		cmd.Dir = tmpDir
		logOutput, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Failed to check git log:\n%s", logOutput)

		if !strings.Contains(string(logOutput), "todo") {
			GinkgoWriter.Printf("OpenCode output:\n%s\n", opencodeOutput)
			Skip("OpenCode did not make the commit - cannot test note storage")
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
		Expect(noteData["agent"]).To(Equal("opencode"))

		By("Note content is valid and contains all required fields")
	})
})
