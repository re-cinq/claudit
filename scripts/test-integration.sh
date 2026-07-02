#!/bin/bash
# Manual integration test for shiftlog with Claude Code
#
# This script helps verify that shiftlog is properly integrated with Claude Code.
# Run this after installing shiftlog and initializing it in a repository.
#
# Usage: ./scripts/test-integration.sh

set -e

echo "=== Claudit Integration Test ==="
echo

# Check prerequisites
echo "1. Checking prerequisites..."

if ! command -v shiftlog &> /dev/null; then
    echo "   ERROR: 'shiftlog' not found in PATH"
    echo "   Install with: go install github.com/re-cinq/shiftlog@latest"
    exit 1
fi
echo "   ✓ shiftlog is in PATH"

if ! git rev-parse --git-dir &> /dev/null; then
    echo "   ERROR: Not in a git repository"
    exit 1
fi
echo "   ✓ In a git repository"

# Check if initialized
if [ ! -f ".claude/settings.local.json" ]; then
    echo "   WARNING: .claude/settings.local.json not found"
    echo "   Run 'shiftlog init' first"
else
    echo "   ✓ Claude settings file exists"
fi

echo

# Verify hook format
echo "2. Checking hook configuration..."
if [ -f ".claude/settings.local.json" ]; then
    if grep -q '"hooks"' .claude/settings.local.json && \
       grep -q '"PostToolUse"' .claude/settings.local.json; then
        echo "   ✓ Hook format is correct (nested hooks.PostToolUse)"
    else
        echo "   ERROR: Hook format may be incorrect"
        echo "   Expected format: {\"hooks\": {\"PostToolUse\": [...]}}"
        echo "   Actual content:"
        cat .claude/settings.local.json
        exit 1
    fi
fi

echo

# Simulate a hook call
echo "3. Testing hook invocation..."
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

# Create a sample transcript
cat > "$TMPDIR/transcript.jsonl" << 'EOF'
{"uuid":"test-1","type":"user","message":{"role":"user","content":[{"type":"text","text":"Test message"}]}}
EOF

# Create a test commit (if we don't have one)
CURRENT_HEAD=$(git rev-parse HEAD 2>/dev/null || echo "")
if [ -z "$CURRENT_HEAD" ]; then
    echo "   Creating initial commit for testing..."
    echo "test" > "$TMPDIR/testfile"
    git add "$TMPDIR/testfile" 2>/dev/null || true
    git commit --allow-empty -m "Test commit for shiftlog integration" > /dev/null
    CURRENT_HEAD=$(git rev-parse HEAD)
    echo "   Created test commit: ${CURRENT_HEAD:0:8}"
fi

# Simulate hook input
HOOK_INPUT=$(cat << EOF
{
  "session_id": "integration-test-$(date +%s)",
  "transcript_path": "$TMPDIR/transcript.jsonl",
  "tool_name": "Bash",
  "tool_input": {
    "command": "git commit -m 'test'"
  }
}
EOF
)

echo "   Simulating hook call..."
if echo "$HOOK_INPUT" | shiftlog store 2>&1 | grep -q "stored conversation"; then
    echo "   ✓ Hook execution successful"
else
    echo "   ERROR: Hook execution failed or produced no output"
    echo "   Try running manually: echo '<hook_input>' | shiftlog store"
    exit 1
fi

echo

# Check if note was created
echo "4. Verifying git note was created..."
if git notes --ref=refs/notes/shiftlog show HEAD &> /dev/null; then
    echo "   ✓ Git note created successfully"
    echo "   Note content preview:"
    git notes --ref=refs/notes/shiftlog show HEAD | head -c 200
    echo "..."
else
    echo "   ERROR: Git note was not created"
    exit 1
fi

echo
echo
echo "=== Integration test passed! ==="
echo
echo "Claudit is properly configured. When Claude Code makes commits,"
echo "conversations will be automatically stored as git notes."
echo
echo "To verify with real Claude Code usage:"
echo "  1. Start Claude Code in this directory"
echo "  2. Ask Claude to make a commit"
echo "  3. Run 'shiftlog list' to see stored conversations"
