package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Git Hooks Auto-Sync", func() {
	var local, remote *testutil.GitRepo

	BeforeEach(func() {
		var err error
		local, remote, err = testutil.NewGitRepoWithRemote()
		Expect(err).NotTo(HaveOccurred())

		// Create initial commit and push
		Expect(local.WriteFile("README.md", "# Test")).To(Succeed())
		Expect(local.Commit("Initial commit")).To(Succeed())
		Expect(local.Run("git", "push", "-u", "origin", "master")).To(Succeed())

		// Initialize shiftlog (installs hooks)
		_, _, err = testutil.RunShiftlogInDir(local.Path, "init")
		Expect(err).NotTo(HaveOccurred())

		// Make sure hooks can find shiftlog binary
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

	Describe("pre-push hook", func() {
		It("automatically syncs notes when user runs git push", func() {
			// Store a conversation
			transcriptPath := filepath.Join(local.Path, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("session-hook-test", transcriptPath, "git commit -m 'test'")
			_, _, err := testutil.RunShiftlogInDirWithStdin(local.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Verify local has the note
			Expect(local.HasNote("refs/notes/shiftlog", head)).To(BeTrue())

			// Remote should NOT have the note yet
			Expect(remote.HasNote("refs/notes/shiftlog", head)).To(BeFalse())

			// Make a change and push (triggers pre-push hook)
			Expect(local.WriteFile("new-file.txt", "content")).To(Succeed())
			Expect(local.Commit("Add new file")).To(Succeed())
			Expect(local.Run("git", "push", "origin", "master")).To(Succeed())

			// Now remote SHOULD have the note (hook pushed it)
			Expect(remote.HasNote("refs/notes/shiftlog", head)).To(BeTrue())
		})
	})

	Describe("post-merge hook", func() {
		It("automatically fetches notes after git pull", func() {
			// First, store and push a conversation from local
			transcriptPath := filepath.Join(local.Path, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("session-post-merge", transcriptPath, "git commit -m 'test'")
			_, _, err := testutil.RunShiftlogInDirWithStdin(local.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Push notes manually
			_, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())

			// Commit .claude to track it in git (so clones can merge cleanly)
			Expect(local.Commit("Add .claude settings")).To(Succeed())
			Expect(local.Run("git", "push", "origin", "master")).To(Succeed())

			// Create a clone without notes, but with hooks
			clone, err := testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())
			defer clone.Cleanup()

			Expect(clone.Run("git", "remote", "add", "origin", remote.Path)).To(Succeed())
			Expect(clone.Run("git", "fetch", "origin")).To(Succeed())
			Expect(clone.Run("git", "checkout", "-b", "master", "origin/master")).To(Succeed())

			// Initialize shiftlog on clone (installs hooks) - this won't conflict
			// because we already have .claude committed
			_, _, err = testutil.RunShiftlogInDir(clone.Path, "init")
			Expect(err).NotTo(HaveOccurred())
			clone.SetBinaryPath(testutil.BinaryPath())

			// Clone should not have notes yet
			Expect(clone.HasNote("refs/notes/shiftlog", head)).To(BeFalse())

			// Push a new commit from local so clone has something to pull
			Expect(local.WriteFile("another.txt", "content")).To(Succeed())
			Expect(local.Commit("Another commit")).To(Succeed())
			Expect(local.Run("git", "push", "origin", "master")).To(Succeed())

			// Pull from clone (triggers post-merge hook which runs sync pull)
			Expect(clone.Run("git", "pull", "origin", "master")).To(Succeed())

			// Now clone should have the note (hook fetched it)
			Expect(clone.HasNote("refs/notes/shiftlog", head)).To(BeTrue())
		})
	})
})

var _ = Describe("Sync Command", func() {
	var local, remote *testutil.GitRepo

	BeforeEach(func() {
		var err error
		local, remote, err = testutil.NewGitRepoWithRemote()
		Expect(err).NotTo(HaveOccurred())

		// Create initial commit and push
		Expect(local.WriteFile("README.md", "# Test")).To(Succeed())
		Expect(local.Commit("Initial commit")).To(Succeed())
		Expect(local.Run("git", "push", "-u", "origin", "master")).To(Succeed())
	})

	AfterEach(func() {
		if local != nil {
			local.Cleanup()
		}
		if remote != nil {
			remote.Cleanup()
		}
	})

	Describe("shiftlog sync with --remote flag", func() {
		var upstream *testutil.GitRepo

		BeforeEach(func() {
			var err error
			upstream, err = testutil.NewGitRepoAsBare()
			Expect(err).NotTo(HaveOccurred())

			// Add upstream as a second remote
			Expect(local.AddRemote("upstream", upstream.Path)).To(Succeed())

			// Push master to upstream so it has the branch
			Expect(local.Run("git", "push", "upstream", "master")).To(Succeed())
		})

		AfterEach(func() {
			if upstream != nil {
				upstream.Cleanup()
			}
		})

		It("pushes notes to non-origin remote with --remote flag", func() {
			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Store a conversation
			transcriptPath := filepath.Join(local.Path, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("session-upstream", transcriptPath, "git commit -m 'test'")
			_, _, err = testutil.RunShiftlogInDirWithStdin(local.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			// Push notes to upstream (not origin)
			stdout, _, err := testutil.RunShiftlogInDir(local.Path, "sync", "push", "--remote=upstream")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Pushed"))

			// Verify upstream has the notes ref
			Expect(upstream.HasNote("refs/notes/shiftlog", head)).To(BeTrue())

			// Verify origin does NOT have the notes ref (we didn't push there)
			Expect(remote.HasNote("refs/notes/shiftlog", head)).To(BeFalse())
		})

		It("pulls notes from non-origin remote with --remote flag", func() {
			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Store and push notes to upstream
			transcriptPath := filepath.Join(local.Path, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("session-upstream-pull", transcriptPath, "git commit -m 'test'")
			_, _, err = testutil.RunShiftlogInDirWithStdin(local.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())
			_, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push", "--remote=upstream")
			Expect(err).NotTo(HaveOccurred())

			// Create a new clone that only has upstream as remote
			clone, err := testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())
			defer clone.Cleanup()

			Expect(clone.AddRemote("upstream", upstream.Path)).To(Succeed())
			Expect(clone.Run("git", "fetch", "upstream")).To(Succeed())
			Expect(clone.Run("git", "checkout", "-b", "master", "upstream/master")).To(Succeed())

			// Clone should not have notes yet
			Expect(clone.HasNote("refs/notes/shiftlog", head)).To(BeFalse())

			// Pull notes from upstream
			stdout, _, err := testutil.RunShiftlogInDir(clone.Path, "sync", "pull", "--remote=upstream")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Fetched"))

			// Now clone should have the note
			Expect(clone.HasNote("refs/notes/shiftlog", head)).To(BeTrue())
		})
	})

	Describe("shiftlog sync push", func() {
		It("pushes notes to remote", func() {
			// Create a note on the commit
			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Store a conversation
			transcriptPath := filepath.Join(local.Path, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("session-123", transcriptPath, "git commit -m 'test'")
			_, _, err = testutil.RunShiftlogInDirWithStdin(local.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			// Push notes
			stdout, _, err := testutil.RunShiftlogInDir(local.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Pushed"))

			// Verify remote has the notes ref
			output, err := remote.RunOutput("git", "notes", "--ref", "refs/notes/shiftlog", "list")
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring(head))
		})
	})

	Describe("shiftlog sync pull", func() {
		It("fetches notes from remote", func() {
			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Store and push a note from local
			transcriptPath := filepath.Join(local.Path, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("session-123", transcriptPath, "git commit -m 'test'")
			_, _, err = testutil.RunShiftlogInDirWithStdin(local.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())
			_, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())

			// Create a clone without notes
			clone, err := testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())
			defer clone.Cleanup()

			Expect(clone.Run("git", "remote", "add", "origin", remote.Path)).To(Succeed())
			Expect(clone.Run("git", "fetch", "origin")).To(Succeed())
			Expect(clone.Run("git", "checkout", "-b", "master", "origin/master")).To(Succeed())

			// Clone should not have notes yet
			Expect(clone.HasNote("refs/notes/shiftlog", head)).To(BeFalse())

			// Pull notes
			stdout, _, err := testutil.RunShiftlogInDir(clone.Path, "sync", "pull")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Fetched"))

			// Now clone should have the note
			Expect(clone.HasNote("refs/notes/shiftlog", head)).To(BeTrue())
		})
	})

	Describe("diverged notes merge", func() {
		It("merges notes from two repos that annotated different commits", func() {
			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Dev1 (local) adds a note on initial commit and pushes
			local.AddNote("refs/notes/shiftlog", head, "dev1-note-on-initial")
			_, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())

			// Dev1 makes a second commit with a note
			Expect(local.WriteFile("second.txt", "content")).To(Succeed())
			Expect(local.Commit("second commit")).To(Succeed())
			secondHead, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())
			local.AddNote("refs/notes/shiftlog", secondHead, "dev1-note-on-second")
			_, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())

			// Dev2 (clone) clones the repo and adds its own note on a third commit
			clone, err := testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())
			defer clone.Cleanup()

			Expect(clone.Run("git", "remote", "add", "origin", remote.Path)).To(Succeed())
			Expect(clone.Run("git", "fetch", "origin")).To(Succeed())
			Expect(clone.Run("git", "checkout", "-b", "master", "origin/master")).To(Succeed())

			Expect(clone.WriteFile("third.txt", "content")).To(Succeed())
			Expect(clone.Commit("third commit")).To(Succeed())
			thirdHead, err := clone.GetHead()
			Expect(err).NotTo(HaveOccurred())
			clone.AddNote("refs/notes/shiftlog", thirdHead, "dev2-note-on-third")

			// Dev2 pulls — should merge dev1's notes in
			stdout, _, err := testutil.RunShiftlogInDir(clone.Path, "sync", "pull")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("merged"))

			// All three notes should be present
			Expect(clone.HasNote("refs/notes/shiftlog", head)).To(BeTrue())
			Expect(clone.HasNote("refs/notes/shiftlog", secondHead)).To(BeTrue())
			Expect(clone.HasNote("refs/notes/shiftlog", thirdHead)).To(BeTrue())
		})

		It("concatenates conflicting notes on the same commit SHA", func() {
			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Dev1 adds a note and pushes
			local.AddNote("refs/notes/shiftlog", head, "dev1-note")
			_, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())

			// Dev2 clones and adds a different note on the SAME commit
			clone, err := testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())
			defer clone.Cleanup()

			Expect(clone.Run("git", "remote", "add", "origin", remote.Path)).To(Succeed())
			Expect(clone.Run("git", "fetch", "origin")).To(Succeed())
			Expect(clone.Run("git", "checkout", "-b", "master", "origin/master")).To(Succeed())

			clone.AddNote("refs/notes/shiftlog", head, "dev2-note")

			// Dev2 pulls — should merge via cat_sort_uniq
			_, _, err = testutil.RunShiftlogInDir(clone.Path, "sync", "pull")
			Expect(err).NotTo(HaveOccurred())

			// Both notes should be present (concatenated)
			note, err := clone.GetNote("refs/notes/shiftlog", head)
			Expect(err).NotTo(HaveOccurred())
			Expect(note).To(ContainSubstring("dev1-note"))
			Expect(note).To(ContainSubstring("dev2-note"))
		})
	})

	Describe("non-fast-forward push", func() {
		It("rejects push when remote has diverged and advises pull", func() {
			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Dev1 adds a note and pushes
			local.AddNote("refs/notes/shiftlog", head, "dev1-note")
			_, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())

			// Dev2 clones and adds a conflicting note
			clone, err := testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())
			defer clone.Cleanup()

			Expect(clone.Run("git", "remote", "add", "origin", remote.Path)).To(Succeed())
			Expect(clone.Run("git", "fetch", "origin")).To(Succeed())
			Expect(clone.Run("git", "checkout", "-b", "master", "origin/master")).To(Succeed())

			clone.AddNote("refs/notes/shiftlog", head, "dev2-note")

			// Dev2 tries to push — should fail
			stdout, _, err := testutil.RunShiftlogInDir(clone.Path, "sync", "push")
			Expect(err).To(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Push rejected"))
			Expect(stdout).To(ContainSubstring("shiftlog sync pull"))

			// Dev2 pulls first, then push succeeds
			_, _, err = testutil.RunShiftlogInDir(clone.Path, "sync", "pull")
			Expect(err).NotTo(HaveOccurred())

			_, _, err = testutil.RunShiftlogInDir(clone.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("notes round-trip", func() {
		It("preserves conversation through push/pull", func() {
			// Store conversation locally
			transcriptPath := filepath.Join(local.Path, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("session-roundtrip", transcriptPath, "git commit -m 'test'")
			_, _, err := testutil.RunShiftlogInDirWithStdin(local.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			head, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Get original note content
			originalNote, err := local.GetNote("refs/notes/shiftlog", head)
			Expect(err).NotTo(HaveOccurred())

			// Push to remote
			_, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())

			// Create clone and pull
			clone, err := testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())
			defer clone.Cleanup()

			Expect(clone.Run("git", "remote", "add", "origin", remote.Path)).To(Succeed())
			Expect(clone.Run("git", "fetch", "origin")).To(Succeed())
			Expect(clone.Run("git", "checkout", "-b", "master", "origin/master")).To(Succeed())

			_, _, err = testutil.RunShiftlogInDir(clone.Path, "sync", "pull")
			Expect(err).NotTo(HaveOccurred())

			// Compare notes
			clonedNote, err := clone.GetNote("refs/notes/shiftlog", head)
			Expect(err).NotTo(HaveOccurred())

			var original, cloned map[string]interface{}
			Expect(json.Unmarshal([]byte(originalNote), &original)).To(Succeed())
			Expect(json.Unmarshal([]byte(clonedNote), &cloned)).To(Succeed())

			Expect(cloned["session_id"]).To(Equal(original["session_id"]))
			Expect(cloned["checksum"]).To(Equal(original["checksum"]))
			Expect(cloned["transcript"]).To(Equal(original["transcript"]))
		})
	})
})
