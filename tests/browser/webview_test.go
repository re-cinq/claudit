package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/re-cinq/shift-log/internal/git"
	"github.com/re-cinq/shift-log/internal/storage"
	"github.com/re-cinq/shift-log/internal/web"
)

// testRepo is a temporary git repository for browser tests.
type testRepo struct {
	path string
	t    *testing.T
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	dir, err := os.MkdirTemp("", "shiftlog-browser-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	r := &testRepo{path: dir, t: t}
	r.git("init", "-b", "master")
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "Test User")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.path
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *testRepo) writeFile(name, content string) {
	r.t.Helper()
	p := filepath.Join(r.path, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) commit(message string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "--no-gpg-sign", "-m", message, "--allow-empty")
	return r.git("rev-parse", "HEAD")
}

func (r *testRepo) addConversation(commitSHA, sessionID string, transcriptData []byte, messageCount int) {
	r.t.Helper()
	stored, err := storage.NewStoredConversation(sessionID, r.path, "master", messageCount, transcriptData)
	if err != nil {
		r.t.Fatal(err)
	}
	data, err := stored.Marshal()
	if err != nil {
		r.t.Fatal(err)
	}
	r.git("notes", "--ref", git.NotesRef, "add", "-f", "-m", string(data), commitSHA)
}

func marshalTranscript(entries []map[string]interface{}) []byte {
	var lines []string
	for _, e := range entries {
		data, _ := json.Marshal(e)
		lines = append(lines, string(data))
	}
	return []byte(strings.Join(lines, "\n"))
}

// allocCtx is a shared Chrome allocator context created once in TestMain.
// All tests create browser tabs from this context, avoiding repeated cold starts.
var allocCtx context.Context

func TestMain(m *testing.M) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WSURLReadTimeout(45*time.Second),
	)

	ctx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	allocCtx = ctx

	code := m.Run()
	cancel()
	os.Exit(code)
}

// chdir changes CWD to dir for git functions that operate on CWD.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
}

// newBrowserContext creates a new browser tab from the shared Chrome allocator.
func newBrowserContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()

	ctx, cancel := chromedp.NewContext(allocCtx)
	t.Cleanup(cancel)
	return ctx, cancel
}

// setupTestServer creates a test repo with sample data and starts an httptest server.
// Returns the server URL, repo, and the SHAs of created commits.
func setupTestServer(t *testing.T) (string, *testRepo, []string) {
	t.Helper()

	repo := newTestRepo(t)
	chdir(t, repo.path)

	// Commit 1: no conversation
	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("Initial commit")

	// Commit 2: with conversation (user + assistant text messages)
	repo.writeFile("b.txt", "b")
	sha2 := repo.commit("Add feature B")
	transcript2 := marshalTranscript([]map[string]interface{}{
		{
			"uuid": "user-1", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Hello, can you help me fix the bug?"},
				},
			},
		},
		{
			"uuid": "assistant-1", "parentUuid": "user-1", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Of course! I'll look into the bug now."},
				},
			},
		},
	})
	repo.addConversation(sha2, "session-1", transcript2, 2)

	// Commit 3: with extended conversation including tool use and thinking
	repo.writeFile("c.txt", "c")
	sha3 := repo.commit("Fix bug in module C")
	transcript3 := marshalTranscript([]map[string]interface{}{
		{
			"uuid": "user-10", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Please create a test file"},
				},
			},
		},
		{
			"uuid": "assistant-10", "parentUuid": "user-10", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{
						"type":     "thinking",
						"thinking": "I need to create a test file.\nLet me think about the structure.\nThis should have proper assertions.\nAnd edge cases too.",
					},
					{"type": "text", "text": "I'll create the test file for you."},
					{
						"type":  "tool_use",
						"id":    "tool-1",
						"name":  "Bash",
						"input": map[string]interface{}{"command": "echo 'test content' > test.txt"},
					},
				},
			},
		},
		{
			"uuid": "user-11", "parentUuid": "assistant-10", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type":       "tool_result",
						"tool_use_id": "tool-1",
						"content":    "test content",
					},
				},
			},
		},
		{
			"uuid": "assistant-11", "parentUuid": "user-11", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Done! The test file has been created."},
				},
			},
		},
	})
	repo.addConversation(sha3, "session-2", transcript3, 4)

	srv := web.NewServer(0, repo.path)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return ts.URL, repo, []string{sha1, sha2, sha3}
}

func TestPageTitle(t *testing.T) {
	url, _, _ := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	var title string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.Title(&title),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	if title != "Shiftlog - Conversation History" {
		t.Errorf("title: want %q, got %q", "Shiftlog - Conversation History", title)
	}
}

func TestCommitListRenders(t *testing.T) {
	url, _, shas := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	var commitListHTML string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		// Wait for commits to load (commit list should have commit items)
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		chromedp.OuterHTML(`#commit-list`, &commitListHTML, chromedp.ByID),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	// Check that SHAs are rendered (short form)
	for _, sha := range shas {
		short := sha[:7]
		if !strings.Contains(commitListHTML, short) {
			t.Errorf("commit list should contain short SHA %s", short)
		}
	}

	// Check commit messages are present
	for _, msg := range []string{"Initial commit", "Add feature B", "Fix bug in module C"} {
		if !strings.Contains(commitListHTML, msg) {
			t.Errorf("commit list should contain message %q", msg)
		}
	}

	// Check dates are rendered (the commit-meta class holds date info)
	if !strings.Contains(commitListHTML, "commit-meta") {
		t.Error("commit list should contain commit-meta elements for dates")
	}
}

func TestConversationBadge(t *testing.T) {
	url, _, _ := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	var badges []*string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		// Get text of all badges
		chromedp.EvaluateAsDevTools(
			`Array.from(document.querySelectorAll('.badge')).map(b => b.textContent.trim())`,
			&badges,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	// We have 2 commits with conversations (sha2 with 2 msgs, sha3 with 4 msgs)
	if len(badges) != 2 {
		t.Fatalf("expected 2 badges, got %d", len(badges))
	}

	foundCounts := make(map[string]bool)
	for _, b := range badges {
		if b != nil {
			foundCounts[*b] = true
		}
	}
	if !foundCounts["2 msgs"] {
		t.Error("expected badge with '2 msgs'")
	}
	if !foundCounts["4 msgs"] {
		t.Error("expected badge with '4 msgs'")
	}
}

func TestClickCommitShowsConversation(t *testing.T) {
	url, _, shas := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	sha2Short := shas[1][:7]
	selector := fmt.Sprintf(`.commit-item[data-sha="%s"]`, shas[1])

	var conversationHTML string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
		// Wait for conversation to load — look for a message div
		chromedp.WaitVisible(`.message`, chromedp.ByQuery),
		chromedp.OuterHTML(`#conversation-content`, &conversationHTML, chromedp.ByID),
	)
	if err != nil {
		t.Fatalf("chromedp (clicking commit %s): %v", sha2Short, err)
	}

	if !strings.Contains(conversationHTML, "Hello, can you help me fix the bug?") {
		t.Error("conversation should contain user message text")
	}
	if !strings.Contains(conversationHTML, "Of course!") {
		t.Error("conversation should contain assistant message text")
	}
}

func TestUserMessageRendering(t *testing.T) {
	url, _, shas := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	selector := fmt.Sprintf(`.commit-item[data-sha="%s"]`, shas[1])

	var userMsgClasses, userMsgRole string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.WaitVisible(`.message.user`, chromedp.ByQuery),
		chromedp.AttributeValue(`.message.user`, "class", &userMsgClasses, nil, chromedp.ByQuery),
		chromedp.Text(`.message.user .message-role`, &userMsgRole, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !strings.Contains(userMsgClasses, "user") {
		t.Errorf("user message should have 'user' class, got %q", userMsgClasses)
	}
	if strings.TrimSpace(strings.ToLower(userMsgRole)) != "user" {
		t.Errorf("user message role label: want 'user', got %q", userMsgRole)
	}
}

func TestAssistantMessageRendering(t *testing.T) {
	url, _, shas := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	selector := fmt.Sprintf(`.commit-item[data-sha="%s"]`, shas[1])

	var assistantMsgClasses, assistantMsgRole string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.WaitVisible(`.message.assistant`, chromedp.ByQuery),
		chromedp.AttributeValue(`.message.assistant`, "class", &assistantMsgClasses, nil, chromedp.ByQuery),
		chromedp.Text(`.message.assistant .message-role`, &assistantMsgRole, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !strings.Contains(assistantMsgClasses, "assistant") {
		t.Errorf("assistant message should have 'assistant' class, got %q", assistantMsgClasses)
	}
	if strings.TrimSpace(strings.ToLower(assistantMsgRole)) != "assistant" {
		t.Errorf("assistant message role label: want 'assistant', got %q", assistantMsgRole)
	}
}

func TestToolUseRendering(t *testing.T) {
	url, _, shas := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	// Click commit 3 which has tool use
	selector := fmt.Sprintf(`.commit-item[data-sha="%s"]`, shas[2])

	var toolName string
	var toolContentVisible bool
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.WaitVisible(`.tool-use`, chromedp.ByQuery),
		// Check tool name is displayed
		chromedp.Text(`.tool-name`, &toolName, chromedp.ByQuery),
		// Check tool content is initially hidden
		chromedp.EvaluateAsDevTools(
			`document.querySelector('.tool-content').classList.contains('expanded')`,
			&toolContentVisible,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !strings.Contains(toolName, "Bash") {
		t.Errorf("tool name should contain 'Bash', got %q", toolName)
	}
	if toolContentVisible {
		t.Error("tool content should be hidden initially")
	}

	// Click tool header to expand
	var toolContentVisibleAfter bool
	err = chromedp.Run(ctx,
		chromedp.Click(`.tool-header`, chromedp.ByQuery),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.EvaluateAsDevTools(
			`document.querySelector('.tool-content').classList.contains('expanded')`,
			&toolContentVisibleAfter,
		),
	)
	if err != nil {
		t.Fatalf("chromedp click tool header: %v", err)
	}

	if !toolContentVisibleAfter {
		t.Error("tool content should be expanded after clicking header")
	}
}

func TestThinkingBlockRendering(t *testing.T) {
	url, _, shas := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	// Click commit 3 which has thinking
	selector := fmt.Sprintf(`.commit-item[data-sha="%s"]`, shas[2])

	var previewText string
	var fullVisible bool
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.WaitVisible(`.thinking-block`, chromedp.ByQuery),
		// Check preview is shown
		chromedp.Text(`.thinking-preview`, &previewText, chromedp.ByQuery),
		// Check full text is initially hidden
		chromedp.EvaluateAsDevTools(
			`window.getComputedStyle(document.querySelector('.thinking-full')).display === 'none'`,
			&fullVisible,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !strings.Contains(previewText, "I need to create a test file") {
		t.Errorf("thinking preview should contain initial text, got %q", previewText)
	}
	if !fullVisible {
		t.Error("thinking full text should be hidden initially (display: none)")
	}

	// Click thinking header to expand
	var expandedAfterClick bool
	err = chromedp.Run(ctx,
		chromedp.Click(`.thinking-header`, chromedp.ByQuery),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.EvaluateAsDevTools(
			`document.querySelector('.thinking-block').classList.contains('expanded')`,
			&expandedAfterClick,
		),
	)
	if err != nil {
		t.Fatalf("chromedp click thinking: %v", err)
	}

	if !expandedAfterClick {
		t.Error("thinking block should be expanded after clicking header")
	}
}

func TestHTMLEscaping(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	sha := repo.commit("XSS test commit")

	// Create conversation with HTML/script injection attempt
	transcript := marshalTranscript([]map[string]interface{}{
		{
			"uuid": "user-xss", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": `<script>alert("xss")</script><img src=x onerror=alert(1)>`},
				},
			},
		},
		{
			"uuid": "assistant-xss", "parentUuid": "user-xss", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": `Here is some <b>bold</b> text and a <a href="javascript:alert(1)">link</a>`},
				},
			},
		},
	})
	repo.addConversation(sha, "session-xss", transcript, 2)

	srv := web.NewServer(0, repo.path)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx, _ := newBrowserContext(t)

	selector := fmt.Sprintf(`.commit-item[data-sha="%s"]`, sha)

	var conversationHTML string
	var scriptCount int
	err := chromedp.Run(ctx,
		chromedp.Navigate(ts.URL),
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.WaitVisible(`.message`, chromedp.ByQuery),
		chromedp.OuterHTML(`#conversation-content`, &conversationHTML, chromedp.ByID),
		// Verify no script tags were injected into the DOM
		chromedp.EvaluateAsDevTools(
			`document.querySelectorAll('#conversation-content script').length`,
			&scriptCount,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if scriptCount != 0 {
		t.Errorf("expected no script elements in conversation, found %d", scriptCount)
	}

	// The HTML entities should be escaped in the rendered output
	if strings.Contains(conversationHTML, `<script>`) {
		t.Error("raw <script> tag should be escaped in conversation HTML")
	}
	if strings.Contains(conversationHTML, `<img `) {
		t.Error("raw <img> tag should be escaped in conversation HTML")
	}
	// Escaped versions should be present
	if !strings.Contains(conversationHTML, `&lt;script&gt;`) {
		t.Error("script tag should appear as escaped HTML entities")
	}
	if !strings.Contains(conversationHTML, `&lt;img`) {
		t.Error("img tag should appear as escaped HTML entities")
	}
}

func TestCodeBlockRendering(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	sha := repo.commit("Code block test")

	transcript := marshalTranscript([]map[string]interface{}{
		{
			"uuid": "user-code", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Show me some code"},
				},
			},
		},
		{
			"uuid": "assistant-code", "parentUuid": "user-code", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Here is a code block:\n```go\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```"},
				},
			},
		},
	})
	repo.addConversation(sha, "session-code", transcript, 2)

	srv := web.NewServer(0, repo.path)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx, _ := newBrowserContext(t)

	selector := fmt.Sprintf(`.commit-item[data-sha="%s"]`, sha)

	var preCount int
	var codeCount int
	err := chromedp.Run(ctx,
		chromedp.Navigate(ts.URL),
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.WaitVisible(`.message`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelectorAll('#conversation-content pre').length`,
			&preCount,
		),
		chromedp.EvaluateAsDevTools(
			`document.querySelectorAll('#conversation-content pre code').length`,
			&codeCount,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if preCount == 0 {
		t.Error("expected at least one <pre> element for code block")
	}
	if codeCount == 0 {
		t.Error("expected at least one <pre><code> element for code block")
	}
}

func TestEmptyState(t *testing.T) {
	url, _, _ := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	var emptyStateText string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.empty-state`, chromedp.ByQuery),
		chromedp.Text(`.empty-state`, &emptyStateText, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !strings.Contains(emptyStateText, "Select a commit") {
		t.Errorf("empty state should say 'Select a commit', got %q", emptyStateText)
	}
}

func TestResumeButtonDisabledUntilConversationSelected(t *testing.T) {
	url, _, shas := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	var disabledBefore bool
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`#resume-btn`, chromedp.ByID),
		// Check button is disabled initially
		chromedp.EvaluateAsDevTools(
			`document.getElementById('resume-btn').disabled`,
			&disabledBefore,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !disabledBefore {
		t.Error("resume button should be disabled before selecting a commit")
	}

	// Click a commit WITHOUT conversation (sha1 = first commit)
	selectorNoConv := fmt.Sprintf(`.commit-item[data-sha="%s"]`, shas[0])
	var disabledNoConv bool
	err = chromedp.Run(ctx,
		chromedp.WaitVisible(`.commit-item`, chromedp.ByQuery),
		chromedp.Click(selectorNoConv, chromedp.ByQuery),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.EvaluateAsDevTools(
			`document.getElementById('resume-btn').disabled`,
			&disabledNoConv,
		),
	)
	if err != nil {
		t.Fatalf("chromedp click no-conv commit: %v", err)
	}

	if !disabledNoConv {
		t.Error("resume button should be disabled when a commit without conversation is selected")
	}

	// Click a commit WITH conversation (sha2 = second commit)
	selectorWithConv := fmt.Sprintf(`.commit-item[data-sha="%s"]`, shas[1])
	var disabledWithConv bool
	err = chromedp.Run(ctx,
		chromedp.Click(selectorWithConv, chromedp.ByQuery),
		chromedp.WaitVisible(`.message`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.getElementById('resume-btn').disabled`,
			&disabledWithConv,
		),
	)
	if err != nil {
		t.Fatalf("chromedp click conv commit: %v", err)
	}

	if disabledWithConv {
		t.Error("resume button should be enabled when a commit with conversation is selected")
	}
}

// setupMultiBranchTestServer creates a repo with 2 branches and conversations, returning
// the test server URL, the repo, and a map of branch name → commit SHAs.
func setupMultiBranchTestServer(t *testing.T) (string, *testRepo, map[string][]string) {
	t.Helper()

	repo := newTestRepo(t)
	chdir(t, repo.path)

	// master: 3 commits, conversation on commit 2
	repo.writeFile("a.txt", "a")
	m1 := repo.commit("Initial commit")

	repo.writeFile("b.txt", "b")
	m2 := repo.commit("Add feature B")
	transcript := marshalTranscript([]map[string]interface{}{
		{
			"uuid": "u1", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Hello from master"},
				},
			},
		},
		{
			"uuid": "a1", "parentUuid": "u1", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Hi there!"},
				},
			},
		},
	})
	repo.addConversation(m2, "session-master", transcript, 2)

	repo.writeFile("d.txt", "d")
	m3 := repo.commit("Update docs")

	// Create feature branch from m1
	repo.git("checkout", "-b", "feature-x", m1)

	repo.writeFile("x.txt", "x1")
	f1 := repo.commit("Start feature X")
	transcriptF := marshalTranscript([]map[string]interface{}{
		{
			"uuid": "u2", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Working on feature X"},
				},
			},
		},
		{
			"uuid": "a2", "parentUuid": "u2", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Let me help with feature X."},
				},
			},
		},
	})
	repo.addConversation(f1, "session-feature", transcriptF, 2)

	repo.writeFile("x.txt", "x2")
	f2 := repo.commit("Complete feature X")

	// Switch back to master so it's the current branch
	repo.git("checkout", "master")

	srv := web.NewServer(0, repo.path)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	shas := map[string][]string{
		"master":    {m1, m2, m3},
		"feature-x": {f1, f2},
	}
	return ts.URL, repo, shas
}

func TestMultiBranchOverviewRenders(t *testing.T) {
	url, _, _ := setupMultiBranchTestServer(t)
	ctx, _ := newBrowserContext(t)

	var headerCount int
	var headerTexts []string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.col-header`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelectorAll('.col-header').length`,
			&headerCount,
		),
		chromedp.EvaluateAsDevTools(
			`Array.from(document.querySelectorAll('.col-header-name')).map(e => e.textContent.trim())`,
			&headerTexts,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if headerCount != 2 {
		t.Errorf("expected 2 column headers, got %d", headerCount)
	}

	foundMaster := false
	foundFeature := false
	for _, text := range headerTexts {
		if strings.Contains(text, "master") {
			foundMaster = true
		}
		if strings.Contains(text, "feature-x") {
			foundFeature = true
		}
	}
	if !foundMaster {
		t.Error("column headers should include 'master'")
	}
	if !foundFeature {
		t.Error("column headers should include 'feature-x'")
	}
}

func TestOverviewConversationCounts(t *testing.T) {
	url, _, _ := setupMultiBranchTestServer(t)
	ctx, _ := newBrowserContext(t)

	var countTexts []string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.col-header`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`Array.from(document.querySelectorAll('.col-header-count')).map(e => e.textContent.trim())`,
			&countTexts,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	// master has 1 conversation, feature-x has 1 conversation
	foundOne := 0
	for _, text := range countTexts {
		if strings.Contains(text, "1 conversation") {
			foundOne++
		}
	}
	if foundOne != 2 {
		t.Errorf("expected 2 branches with '1 conversation', got %d (texts: %v)", foundOne, countTexts)
	}
}

func TestOverviewCommitBoxesRender(t *testing.T) {
	url, _, shas := setupMultiBranchTestServer(t)
	ctx, _ := newBrowserContext(t)

	var boxCount int
	var boxHTML string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-box`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelectorAll('.commit-box').length`,
			&boxCount,
		),
		chromedp.OuterHTML(`.commit-grid`, &boxHTML, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	// master has 3 commits, feature-x has 2 + shared initial = 3, total unique ~5-6
	if boxCount < 5 {
		t.Errorf("expected at least 5 commit boxes, got %d", boxCount)
	}

	// Check that short SHAs from both branches appear
	for branch, commits := range shas {
		for _, sha := range commits {
			short := sha[:7]
			if !strings.Contains(boxHTML, short) {
				t.Errorf("commit grid should contain short SHA %s from branch %s", short, branch)
			}
		}
	}
}

func TestOverviewHasConvStyling(t *testing.T) {
	url, _, shas := setupMultiBranchTestServer(t)
	ctx, _ := newBrowserContext(t)

	// m2 has a conversation, m1 does not
	m2Short := shas["master"][1]
	m1Short := shas["master"][0]

	var m2HasConv, m1HasConv bool
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-box`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			fmt.Sprintf(`document.querySelector('.commit-box[data-sha="%s"]').classList.contains('has-conv')`, m2Short),
			&m2HasConv,
		),
		chromedp.EvaluateAsDevTools(
			fmt.Sprintf(`document.querySelector('.commit-box[data-sha="%s"]').classList.contains('no-conv')`, m1Short),
			&m1HasConv,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !m2HasConv {
		t.Error("commit with conversation should have 'has-conv' class")
	}
	if !m1HasConv {
		t.Error("commit without conversation should have 'no-conv' class")
	}
}

func TestClickCommitBoxDrillsIntoDetail(t *testing.T) {
	url, _, shas := setupMultiBranchTestServer(t)
	ctx, _ := newBrowserContext(t)

	m2 := shas["master"][1]
	selector := fmt.Sprintf(`.commit-box[data-sha="%s"]`, m2)

	var detailVisible bool
	var conversationHTML string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-box`, chromedp.ByQuery),
		// Click commit box with conversation
		chromedp.Click(selector, chromedp.ByQuery),
		// Wait for detail view to appear
		chromedp.WaitVisible(`#detail-container`, chromedp.ByQuery),
		chromedp.WaitVisible(`.message`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`!document.getElementById('detail-container').classList.contains('hidden')`,
			&detailVisible,
		),
		chromedp.OuterHTML(`#conversation-content`, &conversationHTML, chromedp.ByID),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !detailVisible {
		t.Error("detail container should be visible after clicking commit box")
	}
	if !strings.Contains(conversationHTML, "Hello from master") {
		t.Error("conversation should contain user message from master branch")
	}
}

func TestBranchesButtonReturnsToOverview(t *testing.T) {
	url, _, shas := setupMultiBranchTestServer(t)
	ctx, _ := newBrowserContext(t)

	m2 := shas["master"][1]
	selector := fmt.Sprintf(`.commit-box[data-sha="%s"]`, m2)

	var overviewVisible bool
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-box`, chromedp.ByQuery),
		// Drill into detail
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.WaitVisible(`#detail-container`, chromedp.ByQuery),
		// Click Branches button to go back
		chromedp.Click(`#nav-branches`, chromedp.ByID),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.EvaluateAsDevTools(
			`!document.getElementById('overview-container').classList.contains('hidden')`,
			&overviewVisible,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !overviewVisible {
		t.Error("overview should be visible after clicking Branches button")
	}
}

func TestDownArrowHintsRender(t *testing.T) {
	url, _, _ := setupMultiBranchTestServer(t)
	ctx, _ := newBrowserContext(t)

	var hintCount int
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`.commit-box`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelectorAll('.grid-cell-hint').length`,
			&hintCount,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	// With 2 branches diverging, there should be empty cells with down-arrow hints
	if hintCount == 0 {
		t.Error("expected down-arrow hints in empty grid cells where branch has commits below")
	}
}

func TestSingleBranchSkipsOverview(t *testing.T) {
	// Use the single-branch setupTestServer
	url, _, _ := setupTestServer(t)
	ctx, _ := newBrowserContext(t)

	var detailVisible, overviewHidden bool
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`#detail-container`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`!document.getElementById('detail-container').classList.contains('hidden')`,
			&detailVisible,
		),
		chromedp.EvaluateAsDevTools(
			`document.getElementById('overview-container').classList.contains('hidden')`,
			&overviewHidden,
		),
	)
	if err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	if !detailVisible {
		t.Error("single-branch repo should show detail view directly")
	}
	if !overviewHidden {
		t.Error("single-branch repo should hide overview")
	}
}
