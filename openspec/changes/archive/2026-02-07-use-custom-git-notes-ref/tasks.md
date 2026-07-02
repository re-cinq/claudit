## 1. Notes Ref Constant
- [x] 1.1 Ensure `NotesRef` constant is set to `refs/notes/claude-conversations` in notes package
- [x] 1.2 Verify all git notes operations (add, show, list) pass `--ref` with the constant
- [x] 1.3 Verify push/fetch use the custom ref

## 2. Remove Default Ref Usage
- [ ] 2.1 Remove any notes stored on `refs/notes/commits` in the dev repo (manual cleanup)
- [x] 2.2 Remove configuration infrastructure if any was added (config file, init prompt, flags)

## 3. Testing
- [x] 3.1 Acceptance test: notes are stored on `refs/notes/claude-conversations`, not default ref
- [x] 3.2 Acceptance test: `git log` does NOT show claudit notes by default
- [x] 3.3 Acceptance test: push/fetch sync the custom ref

## 4. Documentation
- [x] 4.1 Document the custom ref in README (how to view notes manually if desired)
