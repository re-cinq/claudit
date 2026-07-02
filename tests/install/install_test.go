package install_test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findProjectRoot walks up from cwd looking for go.mod.
func findProjectRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

// hasGitHubRelease returns true if the repo has a published release with a
// shiftlog-named asset (i.e. published after the claudit → shiftlog rename).
func hasGitHubRelease() bool {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/re-cinq/shift-log/releases/latest")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(resp.Body)
	return strings.Contains(string(body), `"shiftlog_`)
}

func TestInstallScript(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available, skipping install script test")
	}

	if !hasGitHubRelease() {
		t.Skip("no published GitHub release found for re-cinq/shift-log, skipping install script test")
	}

	root := findProjectRoot()
	scriptPath := filepath.Join(root, "scripts", "install.sh")

	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("install script not found at %s", scriptPath)
	}

	// Run a clean Debian container that:
	// 1. Installs curl (needed by install script)
	// 2. Runs the install script
	// 3. Verifies shiftlog is installed and reports a version
	cmd := exec.Command("docker", "run", "--rm",
		"-v", scriptPath+":/tmp/install.sh:ro",
		"debian:bookworm-slim",
		"bash", "-c",
		"apt-get update -qq && apt-get install -y -qq curl ca-certificates > /dev/null 2>&1 && bash /tmp/install.sh && shiftlog --version",
	)

	out, err := cmd.CombinedOutput()
	output := string(out)
	t.Logf("container output:\n%s", output)

	if err != nil {
		t.Fatalf("install script failed: %v", err)
	}

	if !strings.Contains(output, "shiftlog version") {
		t.Errorf("expected output to contain 'shiftlog version', got:\n%s", output)
	}

	// The installed binary should report a real version, not "dev"
	lines := strings.Split(strings.TrimSpace(output), "\n")
	lastLine := lines[len(lines)-1]
	if strings.Contains(lastLine, "dev") {
		t.Errorf("expected a release version, but got: %s", lastLine)
	}
}
