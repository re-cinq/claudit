# Shift Log

Save, view, and search coding agent conversation history, persisted as Git Notes.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/re-cinq/shift-log/master/scripts/install.sh | bash
```

```bash
# ...In a Git repo
shiftlog init --agent=<agent>
```

Where `<agent>` is `claude` (default), `codex`, `copilot`, `gemini`, or `opencode`.

Now work with your coding agent as you would normally. Whenever you or the agent commit, the conversation since the last commit will be attached to that commit as a Git Note.

## Supported Agents

| Agent       | Init command                    | How it hooks in                       |
| ----------- | ------------------------------- | ------------------------------------- |
| Claude Code | `shiftlog init` (default)        | `.claude/settings.json` hooks         |
| Codex CLI   | `shiftlog init --agent=codex`    | Post-commit git hook                  |
| Copilot CLI | `shiftlog init --agent=copilot`  | `.github/hooks/shiftlog.json` hook     |
| Gemini CLI  | `shiftlog init --agent=gemini`   | `.gemini/settings.json` hooks         |
| OpenCode    | `shiftlog init --agent=opencode` | `.opencode/plugins/shiftlog.js` plugin |

## Usage

**See what conversations you have:**

```bash
shiftlog list
```

**Search through past conversations:**

```bash
shiftlog search "authentication"          # Text search
shiftlog search --agent claude --branch main  # Filter by metadata
shiftlog search "jwt" --regex --context 2     # Regex with context lines
```

**Get a quick summary of a conversation:**

```bash
shiftlog summarise        # Summarise HEAD conversation
shiftlog tldr HEAD~1      # Alias, works the same way
shiftlog tldr --focus="security"  # Prioritise security-related changes
```

**Resume a past session:**

```bash
shiftlog resume abc123    # By commit SHA
shiftlog resume HEAD~3    # By git ref
```

**View in your browser:**

```bash
shiftlog serve
```

**Pull down conversations from a repo you cloned:**

```bash
shiftlog sync pull
```

## shiftlog vs Entire

|                     | [Entire](https://entire.io) | shiftlog                                                    |
| ------------------- | --------------------------- | ---------------------------------------------------------- |
| **Funding**         | $60M seed round             | Claude Code Max plan ($200/mo)                             |
| **Staffing**        | 12 engineers                | An imbecile spec-driving while not really paying attention |
| **Agents**          | Claude Code, Gemini CLI     | Claude Code, Codex CLI, Copilot CLI, Gemini CLI, OpenCode  |
| **Storage**         | Custom checkpoints format   | Standard Git Notes                                         |
| **Resume sessions** | No                          | Yes                                                        |
| **Web viewer**      | No                          | Yes                                                        |
| **Rebase support**  | No                          | Yes                                                        |
| **Open source**     | Yes                         | Yes                                                        |

## Why?

In order to understand _how_ and _why_ a commit was made, we need to see the conversation that led to it.

## How It Works

Shiftlog uses [Git Notes](https://git-scm.com/docs/git-notes) to attach conversations to commits, stored under `refs/notes/shiftlog` to keep `git log` clean. When you run `shiftlog init`, it sets up hooks so:

1. When the coding agent makes a commit, the conversation is saved automatically
2. When you make a commit during an agent session, it's saved too
3. When you push/pull, conversations sync with the remote

No extra steps needed during your normal workflow.

To view notes directly with git: `git log --notes=shiftlog`

## Commands

| Command                   | Description                             |
| ------------------------- | --------------------------------------- |
| `shiftlog init`            | Initialize shiftlog in the current repo  |
| `shiftlog list`            | List commits with stored conversations  |
| `shiftlog search [query]`  | Search through stored conversations     |
| `shiftlog show [ref]`      | Show conversation history for a commit  |
| `shiftlog summarise [ref]` | Summarise a conversation using your coding agent |
| `shiftlog resume <commit>` | Resume a coding agent session from a commit |
| `shiftlog serve`           | Start the web visualization server      |
| `shiftlog doctor`          | Diagnose shiftlog configuration issues   |
| `shiftlog debug`           | Toggle debug logging                    |
| `shiftlog sync push/pull`  | Sync conversation notes with remote     |
| `shiftlog remap`           | Remap orphaned notes to rebased commits |

## Requirements

- Git
- One of the supported coding agents (Claude Code, Codex CLI, Copilot CLI, Gemini CLI, or OpenCode)

## Multi-Developer Sync

When multiple developers use shiftlog on the same repository, each person's conversation notes are synced automatically via git push/pull hooks. Notes from different developers are merged seamlessly because they typically annotate different commits.

If the remote notes ref has diverged (e.g. two developers pushed notes without pulling first), `shiftlog sync push` will reject the push and advise you to pull first:

```bash
shiftlog sync pull   # Fetches and merges remote notes
shiftlog sync push   # Now succeeds
```

In the rare case where two developers annotate the exact same commit SHA, both notes are preserved by concatenation — no data is lost.

## Git Worktrees

Shiftlog is worktree-safe. If you use `git worktree` to work on multiple branches simultaneously, each worktree sees only the conversations for commits on its own branch. Hooks are shared across worktrees (as git requires), but `shiftlog list` and `shiftlog show` are scoped to the current HEAD.

## Local Rebase

Conversation notes automatically follow commits when you rebase. During `shiftlog init`, the `notes.rewriteRef` git config is set to `refs/notes/shiftlog`, which tells git to remap notes to the new commit SHAs during rebase. No manual steps are needed.

If you initialized before this config was added, run `shiftlog init` again or set it manually:

```bash
git config notes.rewriteRef refs/notes/shiftlog
```

You can verify the config is set with `shiftlog doctor`.

## GitHub Rebase Merge

When a PR is merged via GitHub's "Rebase and merge" strategy, the commits get new SHAs on the target branch. Since this happens server-side, git's `notes.rewriteRef` does not apply and notes remain keyed to the original (now-orphaned) commit SHAs.

Shiftlog handles this automatically. After you pull a rebase-merged PR, the post-merge hook runs `shiftlog remap`, which:

1. Finds notes on commits that are no longer on any branch (orphaned)
2. Uses `git patch-id` to match each orphaned commit to its rebased counterpart by diff content
3. Copies the note to the new commit SHA (the original note is also kept)

For this to work, the local feature branch must be deleted or pruned so the old commits become orphaned. If your GitHub repo is configured to auto-delete branches after merge, this happens naturally when you `git pull` with `fetch.prune=true` (or `git fetch --prune`). Otherwise, delete the branch manually first:

```bash
git branch -D feature-branch
shiftlog remap
```

You can also run `shiftlog remap` at any time — it reports how many notes were remapped and how many remain unmatched. Unmatched notes are left in place, not deleted.

**Note:** Remap works with GitHub's "Rebase and merge" strategy. It does not support "Squash and merge", which combines all commits into one new commit with no 1:1 mapping to copy notes from.

## License

[AI Native Application License (AINAL)](https://github.com/re-cinq/ai-native-application-license) — see [LICENSE](LICENSE).
