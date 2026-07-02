package cmd

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is set at build time via ldflags.
// If not set (e.g. go install), we fall back to the module version
// embedded by the Go toolchain.
var version = "dev"

var rootCmd = &cobra.Command{
	Use:   "shiftlog",
	Short: "Store and resume AI coding conversations as Git Notes",
	Long: `Shiftlog captures AI coding agent conversation history and stores it as Git Notes
attached to commits. This enables teams to preserve AI-assisted development
context alongside their code and resume interrupted sessions.

Supports Claude Code, Codex CLI, Copilot CLI, Gemini CLI, and OpenCode.`,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	if version == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
	}
	rootCmd.Version = version
	rootCmd.SetVersionTemplate(fmt.Sprintf("shiftlog version %s\n", version))

	// Add command groups
	rootCmd.AddGroup(
		&cobra.Group{ID: "human", Title: "Commands for humans:"},
		&cobra.Group{ID: "hooks", Title: "Commands mostly used by hooks:"},
	)
}
