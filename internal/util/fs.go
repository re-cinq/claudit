package util

import (
	"os"
	"os/exec"
	"strings"
)

// EnsureDir creates a directory and all parent directories if they don't exist.
// Uses standard permissions (0755).
func EnsureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

// GetProjectRoot returns the git repository root, or the current working directory
// if not inside a git repository. This is useful for determining the project root
// regardless of git context.
func GetProjectRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(output)), nil
	}

	// Fall back to current directory if not in a git repo
	return os.Getwd()
}
