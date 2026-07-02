package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("End-to-End Store Flow", func() {
	var local, remote *testutil.GitRepo

	BeforeEach(func() {
		var err error
		local, remote, err = testutil.NewGitRepoWithRemote()
		Expect(err).NotTo(HaveOccurred())

		// Set binary path so git hooks can find shiftlog
		local.SetBinaryPath(testutil.BinaryPath())
	})

	AfterEach(func() {
		if local != nil {
			local.Cleanup()
		}
		if remote != nil {
			remote.Cleanup()
		}
	})

	It("full flow: init repo, simulate Claude hook, verify note stored, push/pull", func() {
		// Step 1: Initialize shiftlog
		stdout, _, err := testutil.RunShiftlogInDir(local.Path, "init")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring("Shiftlog is now configured"))

		// Verify Claude settings were created
		Expect(local.FileExists(".claude/settings.local.json")).To(BeTrue())

		// Step 2: Create initial commit
		Expect(local.WriteFile("main.go", "package main\n\nfunc main() {}\n")).To(Succeed())
		Expect(local.Commit("Add main.go")).To(Succeed())

		head, err := local.GetHead()
		Expect(err).NotTo(HaveOccurred())

		// Push to remote (without notes yet)
		Expect(local.Run("git", "push", "-u", "origin", "master")).To(Succeed())

		// Step 3: Simulate Claude hook on a commit
		transcriptPath := filepath.Join(local.Path, "transcript.jsonl")
		Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

		hookInput := testutil.SampleHookInput("e2e-session", transcriptPath, "git commit -m 'Add feature'")
		_, stderr, err := testutil.RunShiftlogInDirWithStdin(local.Path, hookInput, "store")
		Expect(err).NotTo(HaveOccurred())
		Expect(stderr).To(ContainSubstring("stored conversation"))

		// Step 4: Verify note was created
		Expect(local.HasNote("refs/notes/shiftlog", head)).To(BeTrue())

		noteContent, err := local.GetNote("refs/notes/shiftlog", head)
		Expect(err).NotTo(HaveOccurred())

		var stored map[string]interface{}
		Expect(json.Unmarshal([]byte(noteContent), &stored)).To(Succeed())
		Expect(stored["session_id"]).To(Equal("e2e-session"))

		// Step 5: Push notes
		stdout, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring("Pushed"))

		// Step 6: Clone and pull notes
		clone, err := testutil.NewGitRepo()
		Expect(err).NotTo(HaveOccurred())
		defer clone.Cleanup()

		Expect(clone.Run("git", "remote", "add", "origin", remote.Path)).To(Succeed())
		Expect(clone.Run("git", "fetch", "origin")).To(Succeed())
		Expect(clone.Run("git", "checkout", "-b", "master", "origin/master")).To(Succeed())

		// Clone should not have notes yet
		Expect(clone.HasNote("refs/notes/shiftlog", head)).To(BeFalse())

		// Pull notes
		stdout, _, err = testutil.RunShiftlogInDir(clone.Path, "sync", "pull")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring("Fetched"))

		// Verify note is now available
		Expect(clone.HasNote("refs/notes/shiftlog", head)).To(BeTrue())

		clonedNote, err := clone.GetNote("refs/notes/shiftlog", head)
		Expect(err).NotTo(HaveOccurred())

		var clonedStored map[string]interface{}
		Expect(json.Unmarshal([]byte(clonedNote), &clonedStored)).To(Succeed())
		Expect(clonedStored["session_id"]).To(Equal("e2e-session"))
	})
})
