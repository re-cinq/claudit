package testutil

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
)

// GitRepo represents a temporary git repository for testing
type GitRepo struct {
	Path     string
	ExtraEnv []string // Extra environment variables for commands
}

// NewGitRepo creates a new temporary git repository
func NewGitRepo() (*GitRepo, error) {
	dir, err := os.MkdirTemp("", "shiftlog-test-repo-*")
	if err != nil {
		return nil, err
	}

	repo := &GitRepo{Path: dir}

	// Initialize git repo with default branch 'master'
	if err := repo.Run("git", "init", "-b", "master"); err != nil {
		repo.Cleanup()
		return nil, err
	}

	// Configure git user for commits
	if err := repo.Run("git", "config", "user.email", "test@example.com"); err != nil {
		repo.Cleanup()
		return nil, err
	}
	if err := repo.Run("git", "config", "user.name", "Test User"); err != nil {
		repo.Cleanup()
		return nil, err
	}

	// Disable GPG signing
	if err := repo.Run("git", "config", "commit.gpgsign", "false"); err != nil {
		repo.Cleanup()
		return nil, err
	}

	return repo, nil
}

// NewGitRepoWithRemote creates a git repo with a local bare remote
func NewGitRepoWithRemote() (*GitRepo, *GitRepo, error) {
	// Create the "remote" (bare repo)
	remoteDir, err := os.MkdirTemp("", "shiftlog-test-remote-*")
	if err != nil {
		return nil, nil, err
	}

	remote := &GitRepo{Path: remoteDir}
	if err := remote.Run("git", "init", "--bare"); err != nil {
		remote.Cleanup()
		return nil, nil, err
	}

	// Create the local repo
	local, err := NewGitRepo()
	if err != nil {
		remote.Cleanup()
		return nil, nil, err
	}

	// Add remote
	if err := local.Run("git", "remote", "add", "origin", remoteDir); err != nil {
		local.Cleanup()
		remote.Cleanup()
		return nil, nil, err
	}

	return local, remote, nil
}

// Cleanup removes the temporary repository
func (r *GitRepo) Cleanup() {
	_ = os.RemoveAll(r.Path)
}

// Run executes a command in the repository directory
func (r *GitRepo) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = r.Path
	if len(r.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), r.ExtraEnv...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return &runError{err: err, stderr: stderr.String()}
		}
		return err
	}
	return nil
}

type runError struct {
	err    error
	stderr string
}

func (e *runError) Error() string {
	return e.err.Error() + ": " + e.stderr
}

// RunOutput executes a command and returns its output
func (r *GitRepo) RunOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = r.Path
	if len(r.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), r.ExtraEnv...)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	return stdout.String(), err
}

// SetBinaryPath adds the shiftlog binary directory to PATH for git hooks
func (r *GitRepo) SetBinaryPath(binPath string) {
	r.ExtraEnv = append(r.ExtraEnv, "PATH="+filepath.Dir(binPath)+":"+os.Getenv("PATH"))
}

// WriteFile creates a file in the repository
func (r *GitRepo) WriteFile(name, content string) error {
	path := filepath.Join(r.Path, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

// ReadFile reads a file from the repository
func (r *GitRepo) ReadFile(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(r.Path, name))
	return string(data), err
}

// FileExists checks if a file exists in the repository
func (r *GitRepo) FileExists(name string) bool {
	_, err := os.Stat(filepath.Join(r.Path, name))
	return err == nil
}

// RemoveFile removes a file from the repository
func (r *GitRepo) RemoveFile(name string) error {
	return os.Remove(filepath.Join(r.Path, name))
}

// Commit creates a commit with the given message
func (r *GitRepo) Commit(message string) error {
	if err := r.Run("git", "add", "-A"); err != nil {
		return err
	}
	return r.Run("git", "commit", "--no-gpg-sign", "-m", message, "--allow-empty")
}

// GetHead returns the SHA of HEAD
func (r *GitRepo) GetHead() (string, error) {
	output, err := r.RunOutput("git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	// Trim newline
	if len(output) > 0 && output[len(output)-1] == '\n' {
		output = output[:len(output)-1]
	}
	return output, nil
}

// GetNote retrieves a git note for a commit
func (r *GitRepo) GetNote(ref, commit string) (string, error) {
	return r.RunOutput("git", "notes", "--ref", ref, "show", commit)
}

// HasNote checks if a commit has a note
func (r *GitRepo) HasNote(ref, commit string) bool {
	err := r.Run("git", "notes", "--ref", ref, "show", commit)
	return err == nil
}

// NewGitRepoAsBare creates a new bare git repository (for use as a remote)
func NewGitRepoAsBare() (*GitRepo, error) {
	dir, err := os.MkdirTemp("", "shiftlog-test-bare-*")
	if err != nil {
		return nil, err
	}

	repo := &GitRepo{Path: dir}
	if err := repo.Run("git", "init", "--bare"); err != nil {
		repo.Cleanup()
		return nil, err
	}

	return repo, nil
}

// AddRemote adds a new remote to the repository
func (r *GitRepo) AddRemote(name, path string) error {
	return r.Run("git", "remote", "add", name, path)
}

// AddNote adds a raw git note to a commit under the given ref
func (r *GitRepo) AddNote(ref, commit, content string) error {
	return r.Run("git", "notes", "--ref", ref, "add", "-f", "-m", content, commit)
}
