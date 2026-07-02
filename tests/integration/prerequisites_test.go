package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Prerequisites", func() {
	type prereq struct {
		agent   string
		envVars []string
		binary  string
		skipEnv string
	}

	prereqs := []prereq{
		{agent: "Claude", envVars: []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}, binary: "claude", skipEnv: "SKIP_CLAUDE_INTEGRATION"},
		{agent: "Codex", envVars: []string{"OPENAI_API_KEY"}, binary: "codex", skipEnv: "SKIP_CODEX_INTEGRATION"},
		{agent: "Gemini", envVars: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, binary: "gemini", skipEnv: "SKIP_GEMINI_INTEGRATION"},
		{agent: "OpenCode", envVars: []string{"GEMINI_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY"}, binary: "opencode", skipEnv: "SKIP_OPENCODE_INTEGRATION"},
		{agent: "Copilot", envVars: []string{"COPILOT_GITHUB_TOKEN"}, binary: "copilot", skipEnv: "SKIP_COPILOT_INTEGRATION"},
	}

	It("should have all required prerequisites available", func() {
		var summary []string
		var missing []string

		for _, p := range prereqs {
			if os.Getenv(p.skipEnv) == "1" {
				summary = append(summary, fmt.Sprintf("  %-10s SKIPPED (%s=1)", p.agent, p.skipEnv))
				continue
			}

			hasKey := false
			for _, env := range p.envVars {
				if os.Getenv(env) != "" {
					hasKey = true
					break
				}
			}

			_, hasbin := exec.LookPath(p.binary)

			keyStatus := "OK"
			if !hasKey {
				keyStatus = "MISSING (" + strings.Join(p.envVars, " or ") + ")"
				missing = append(missing, fmt.Sprintf("%s: missing API key (%s)", p.agent, strings.Join(p.envVars, " or ")))
			}

			binStatus := "OK"
			if hasbin != nil {
				binStatus = fmt.Sprintf("MISSING (%s not in PATH)", p.binary)
				missing = append(missing, fmt.Sprintf("%s: %s not in PATH", p.agent, p.binary))
			}

			summary = append(summary, fmt.Sprintf("  %-10s key=%-8s binary=%s", p.agent, keyStatus, binStatus))
		}

		GinkgoWriter.Printf("\n=== Integration Test Prerequisites ===\n%s\n", strings.Join(summary, "\n"))

		Expect(missing).To(BeEmpty(),
			"Missing prerequisites — these integration tests will fail:\n  %s\n\nSet the required secrets/env vars or skip with SKIP_<AGENT>_INTEGRATION=1",
			strings.Join(missing, "\n  "))
	})
})
