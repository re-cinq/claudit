package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HookType represents the type of git hook
type HookType string

const (
	HookPrePush      HookType = "pre-push"
	HookPostMerge    HookType = "post-merge"
	HookPostCheckout HookType = "post-checkout"
	HookPostCommit   HookType = "post-commit"
)

// shiftlogMarker identifies shiftlog-managed hook sections
const shiftlogMarker = "# shiftlog-managed"

// InstallHook installs or updates a git hook with shiftlog commands
func InstallHook(gitDir string, hookType HookType, command string) error {
	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return err
	}

	hookPath := filepath.Join(hooksDir, string(hookType))

	// Read existing hook content if any
	var existingContent string
	if data, err := os.ReadFile(hookPath); err == nil {
		existingContent = string(data)
	}

	// Build the shiftlog section
	shiftlogSection := fmt.Sprintf(`
%s start
%s
%s end
`, shiftlogMarker, command, shiftlogMarker)

	var newContent string

	if existingContent == "" {
		// New hook file
		newContent = "#!/bin/sh\n" + shiftlogSection
	} else if strings.Contains(existingContent, shiftlogMarker) {
		// Update existing shiftlog section
		newContent = replaceShiftlogSection(existingContent, shiftlogSection)
	} else {
		// Append to existing hook
		newContent = existingContent + "\n" + shiftlogSection
	}

	return os.WriteFile(hookPath, []byte(newContent), 0755)
}

// replaceShiftlogSection replaces the shiftlog-managed section in hook content
func replaceShiftlogSection(content, newSection string) string {
	startMarker := shiftlogMarker + " start"
	endMarker := shiftlogMarker + " end"

	startIdx := strings.Index(content, startMarker)
	endIdx := strings.Index(content, endMarker)

	if startIdx == -1 || endIdx == -1 {
		return content + "\n" + newSection
	}

	// Find the newline before start marker (if any)
	lineStart := startIdx
	if lineStart > 0 {
		lineStart = strings.LastIndex(content[:startIdx], "\n")
		if lineStart == -1 {
			lineStart = 0
		}
	}

	// Find the newline after end marker
	lineEnd := endIdx + len(endMarker)
	if lineEnd < len(content) && content[lineEnd] == '\n' {
		lineEnd++
	}

	return content[:lineStart] + newSection + content[lineEnd:]
}

// RemoveHook removes the shiftlog-managed section from a git hook.
// If the file reduces to just a shebang line (with optional whitespace),
// it is deleted entirely. No-op if the hook file doesn't exist or has no shiftlog section.
func RemoveHook(gitDir string, hookType HookType) error {
	hookPath := filepath.Join(gitDir, "hooks", string(hookType))

	data, err := os.ReadFile(hookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	content := string(data)
	if !strings.Contains(content, shiftlogMarker) {
		return nil
	}

	// Replace the shiftlog section with nothing
	newContent := replaceShiftlogSection(content, "")

	// If only a shebang line remains (with optional whitespace), delete the file
	trimmed := strings.TrimSpace(newContent)
	if trimmed == "" || trimmed == "#!/bin/sh" || trimmed == "#!/bin/bash" {
		return os.Remove(hookPath)
	}

	return os.WriteFile(hookPath, []byte(newContent), 0755)
}

// RemoveAllHooks removes shiftlog-managed sections from all git hooks.
func RemoveAllHooks(gitDir string) error {
	hookTypes := []HookType{HookPrePush, HookPostMerge, HookPostCheckout, HookPostCommit}
	for _, ht := range hookTypes {
		if err := RemoveHook(gitDir, ht); err != nil {
			return fmt.Errorf("failed to remove %s hook: %w", ht, err)
		}
	}
	return nil
}

// InstallAllHooks installs all shiftlog git hooks.
// It resolves the absolute path to the running shiftlog binary so hooks work
// even when the shell environment strips PATH (e.g. Codex CLI sandbox).
func InstallAllHooks(gitDir string) error {
	bin, err := resolveShiftlogBinary()
	if err != nil {
		return fmt.Errorf("failed to resolve shiftlog binary path: %w", err)
	}

	hooks := map[HookType]string{
		HookPrePush:      bin + " sync push",
		HookPostMerge:    bin + " sync pull\n" + bin + " remap",
		HookPostCheckout: bin + " sync pull",
		HookPostCommit:   bin + " store --manual",
	}

	for hookType, command := range hooks {
		if err := InstallHook(gitDir, hookType, command); err != nil {
			return fmt.Errorf("failed to install %s hook: %w", hookType, err)
		}
	}

	return nil
}

// resolveShiftlogBinary returns the absolute path to the running shiftlog binary.
func resolveShiftlogBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
