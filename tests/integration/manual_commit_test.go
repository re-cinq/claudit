package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Manual Commit", func() {
	// agentConfig defines per-agent settings for the parameterized manual commit tests.
	type agentConfig struct {
		name     string
		skipEnv  string
		envVars  []string // at least one must be set
		binary   string
		setupCmd func(tmpDir, shiftlogPath, apiKey string) *exec.Cmd
	}

	agents := []agentConfig{
		{
			name:    "claude",
			skipEnv: "SKIP_CLAUDE_INTEGRATION",
			envVars: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
			binary:  "claude",
			setupCmd: func(tmpDir, shiftlogPath, _ string) *exec.Cmd {
				cmd := exec.Command("claude",
					"--print",
					"--allowedTools", "Bash",
					"--max-turns", "3",
					"--dangerously-skip-permissions",
					"Run: echo 'hello world'",
				)
				cmd.Dir = tmpDir
				cmd.Env = append(os.Environ(),
					"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
				)
				if apiKey := os.Getenv("ANTHROPIC_API_KEY"); apiKey != "" {
					cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY="+apiKey)
				}
				if oauthToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); oauthToken != "" {
					cmd.Env = append(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
				}
				return cmd
			},
		},
		{
			name:    "codex",
			skipEnv: "SKIP_CODEX_INTEGRATION",
			envVars: []string{"OPENAI_API_KEY"},
			binary:  "codex",
			setupCmd: func(tmpDir, shiftlogPath, apiKey string) *exec.Cmd {
				// Login first
				loginCmd := exec.Command("bash", "-c",
					fmt.Sprintf("echo %q | codex login --with-api-key", apiKey))
				loginCmd.Dir = tmpDir
				loginOutput, err := loginCmd.CombinedOutput()
				Expect(err).NotTo(HaveOccurred(), "codex login failed:\n%s", loginOutput)

				cmd := exec.Command("codex", "exec",
					"--dangerously-bypass-approvals-and-sandbox",
					"Run: echo 'hello world'",
				)
				cmd.Dir = tmpDir
				cmd.Env = append(os.Environ(),
					"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
					"OPENAI_API_KEY="+apiKey,
				)
				return cmd
			},
		},
		{
			name:    "gemini",
			skipEnv: "SKIP_GEMINI_INTEGRATION",
			envVars: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			binary:  "gemini",
			setupCmd: func(tmpDir, shiftlogPath, _ string) *exec.Cmd {
				cmd := exec.Command("gemini",
					"-p", "Run: echo 'hello world'",
					"--approval-mode", "yolo",
				)
				cmd.Dir = tmpDir
				cmd.Env = append(os.Environ(),
					"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
				)
				if k := os.Getenv("GEMINI_API_KEY"); k != "" {
					cmd.Env = append(cmd.Env, "GEMINI_API_KEY="+k)
				}
				if k := os.Getenv("GOOGLE_API_KEY"); k != "" {
					cmd.Env = append(cmd.Env, "GOOGLE_API_KEY="+k)
				}
				return cmd
			},
		},
		{
			name:    "opencode",
			skipEnv: "SKIP_OPENCODE_INTEGRATION",
			envVars: []string{"GEMINI_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY"},
			binary:  "opencode",
			setupCmd: func(tmpDir, shiftlogPath, apiKey string) *exec.Cmd {
				// Write opencode.json config
				opencodeConfig := map[string]interface{}{
					"$schema":    "https://opencode.ai/config.json",
					"model":      "google/gemini-2.5-flash",
					"permission": "allow",
				}
				configData, err := json.MarshalIndent(opencodeConfig, "", "  ")
				Expect(err).NotTo(HaveOccurred())
				Expect(os.WriteFile(filepath.Join(tmpDir, "opencode.json"), configData, 0644)).To(Succeed())

				cmd := exec.Command("opencode", "run",
					"Run: echo 'hello world'",
				)
				cmd.Dir = tmpDir
				cmd.Env = append(os.Environ(),
					"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
					"GOOGLE_GENERATIVE_AI_API_KEY="+apiKey,
				)
				return cmd
			},
		},
		{
			name:    "copilot",
			skipEnv: "SKIP_COPILOT_INTEGRATION",
			envVars: []string{"COPILOT_GITHUB_TOKEN"},
			binary:  "copilot",
			setupCmd: func(tmpDir, shiftlogPath, apiKey string) *exec.Cmd {
				cmd := exec.Command("copilot",
					"-p", "Run: echo 'hello world'",
					"--yolo",
				)
				cmd.Dir = tmpDir
				cmd.Env = append(os.Environ(),
					"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
					"COPILOT_GITHUB_TOKEN="+apiKey,
				)
				return cmd
			},
		},
	}

	for _, agent := range agents {
		agent := agent // capture range variable
		It(fmt.Sprintf("should create a note on manual commit after %s session", agent.name), func() {
			skipIfEnvSet(agent.skipEnv)
			apiKey := requireEnvVar(agent.envVars...)
			requireBinary(agent.binary)

			tmpDir, shiftlogPath := setupManualCommitRepo(agent.name)
			DeferCleanup(os.RemoveAll, tmpDir)

			// Run agent to establish a session
			agentCmd := agent.setupCmd(tmpDir, shiftlogPath, apiKey)
			_ = runAgentWithTimeout(agentCmd, 90*time.Second)

			By("Agent session established, making manual commit...")

			manualCommitNewFile(tmpDir)

			time.Sleep(2 * time.Second)

			By("Manual commit created, verifying note...")
			verifyNoteOnHead(tmpDir, agent.name)
		})
	}
})
