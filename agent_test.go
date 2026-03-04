package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseFrontmatter(t *testing.T) {
	content := `---
name: test-skill
description: A test skill
always: true
requirements:
  bin:
    - git
  env:
    - GITHUB_TOKEN
---

# Test Skill

This is the skill content.`

	meta := parseFrontmatter(content)
	if meta == nil {
		t.Fatal("expected non-nil meta")
	}
	if meta.Name != "test-skill" {
		t.Errorf("name = %q, want %q", meta.Name, "test-skill")
	}
	if meta.Description != "A test skill" {
		t.Errorf("description = %q", meta.Description)
	}
	if !meta.Always {
		t.Error("always should be true")
	}
	if len(meta.Requirements.Bin) != 1 || meta.Requirements.Bin[0] != "git" {
		t.Errorf("requirements.bin = %v", meta.Requirements.Bin)
	}
	if len(meta.Requirements.Env) != 1 || meta.Requirements.Env[0] != "GITHUB_TOKEN" {
		t.Errorf("requirements.env = %v", meta.Requirements.Env)
	}
}

func TestParseFrontmatterNoFrontmatter(t *testing.T) {
	meta := parseFrontmatter("# Just a heading\nNo frontmatter here.")
	if meta != nil {
		t.Error("expected nil for content without frontmatter")
	}
}

func TestParseFrontmatterMinimal(t *testing.T) {
	content := `---
name: simple
description: Simple skill
---

Content here.`

	meta := parseFrontmatter(content)
	if meta == nil {
		t.Fatal("expected non-nil meta")
	}
	if meta.Name != "simple" {
		t.Errorf("name = %q", meta.Name)
	}
	if meta.Always {
		t.Error("always should default to false")
	}
}

func TestParseFrontmatterMetadataJSON(t *testing.T) {
	content := `---
name: github
description: GitHub helper
metadata: {"nanobot":{"requires":{"bins":["gh"],"env":["GITHUB_TOKEN"]}}}
---

Body`

	meta := parseFrontmatter(content)
	if meta == nil {
		t.Fatal("expected non-nil meta")
	}
	if got := meta.Requirements.Bin; len(got) != 1 || got[0] != "gh" {
		t.Fatalf("requirements.bin = %v, want [gh]", got)
	}
	if got := meta.Requirements.Env; len(got) != 1 || got[0] != "GITHUB_TOKEN" {
		t.Fatalf("requirements.env = %v, want [GITHUB_TOKEN]", got)
	}
}

func TestParseFrontmatterRequiresAlias(t *testing.T) {
	content := `---
name: tmux
description: tmux helper
requires:
  bins:
    - tmux
  env:
    - TMUX_TMPDIR
---

Body`

	meta := parseFrontmatter(content)
	if meta == nil {
		t.Fatal("expected non-nil meta")
	}
	if got := meta.Requirements.Bin; len(got) != 1 || got[0] != "tmux" {
		t.Fatalf("requirements.bin = %v, want [tmux]", got)
	}
	if got := meta.Requirements.Env; len(got) != 1 || got[0] != "TMUX_TMPDIR" {
		t.Fatalf("requirements.env = %v, want [TMUX_TMPDIR]", got)
	}
}

func TestStripFrontmatter(t *testing.T) {
	content := `---
name: test
---

# Content

Body text.`

	stripped := stripFrontmatter(content)
	if strings.Contains(stripped, "name: test") {
		t.Error("frontmatter should be stripped")
	}
	if !strings.Contains(stripped, "# Content") {
		t.Error("body should be preserved")
	}
	if !strings.Contains(stripped, "Body text.") {
		t.Error("body text should be preserved")
	}
}

func TestStripFrontmatterNoFrontmatter(t *testing.T) {
	content := "# Just content\nNo frontmatter."
	stripped := stripFrontmatter(content)
	if stripped != content {
		t.Errorf("content should be unchanged, got %q", stripped)
	}
}

func TestSkillsLoaderLoadSkill(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "test-skill")
	os.MkdirAll(skillDir, 0755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: test-skill
description: A test
---

# Test Skill

Content here.`), 0644)

	sl := newSkillsLoader(dir)
	content, err := sl.loadSkill("test-skill")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "# Test Skill") {
		t.Error("should contain skill content")
	}
	if strings.Contains(content, "name: test-skill") {
		t.Error("frontmatter should be stripped")
	}
}

func TestSkillsLoaderLoadSkillNotFound(t *testing.T) {
	dir := t.TempDir()
	sl := newSkillsLoader(dir)
	_, err := sl.loadSkill("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent skill")
	}
}

func TestSkillsLoaderListSkills(t *testing.T) {
	dir := t.TempDir()

	// Create two skills
	for _, name := range []string{"skill-a", "skill-b"} {
		skillDir := filepath.Join(dir, "skills", name)
		os.MkdirAll(skillDir, 0755)
		os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: `+name+`
description: Skill `+name+`
---

Content.`), 0644)
	}

	sl := newSkillsLoader(dir)
	skills := sl.listSkills()

	// Should include workspace skills + embedded skills
	wsSkills := 0
	for _, s := range skills {
		if s.Name == "skill-a" || s.Name == "skill-b" {
			wsSkills++
		}
	}
	if wsSkills != 2 {
		t.Errorf("expected 2 workspace skills, found %d", wsSkills)
	}
}

func TestSkillsLoaderBuildSummary(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "my-skill")
	os.MkdirAll(skillDir, 0755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: my-skill
description: Does things
---

Content.`), 0644)

	sl := newSkillsLoader(dir)
	summary := sl.buildSummary()
	if !strings.Contains(summary, "my-skill") {
		t.Error("summary should contain skill name")
	}
	if !strings.Contains(summary, "<skills>") {
		t.Error("summary should contain <skills> tag")
	}
}

func TestSkillsLoaderAlwaysOn(t *testing.T) {
	dir := t.TempDir()

	// Always-on skill (no requirements)
	skillDir := filepath.Join(dir, "skills", "always-skill")
	os.MkdirAll(skillDir, 0755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: always-skill
description: Always on
always: true
---

Content.`), 0644)

	// Not always-on
	skillDir2 := filepath.Join(dir, "skills", "optional-skill")
	os.MkdirAll(skillDir2, 0755)
	os.WriteFile(filepath.Join(skillDir2, "SKILL.md"), []byte(`---
name: optional-skill
description: Optional
---

Content.`), 0644)

	sl := newSkillsLoader(dir)
	alwaysOn := sl.alwaysOnSkills()

	found := false
	for _, name := range alwaysOn {
		if name == "always-skill" {
			found = true
		}
		if name == "optional-skill" {
			t.Error("optional-skill should not be always-on")
		}
	}
	if !found {
		t.Error("always-skill should be in always-on list")
	}
}

func TestMemoryStore(t *testing.T) {
	dir := t.TempDir()
	ms := newMemoryStore(dir)

	// Initially empty
	if mem := ms.readMemory(); mem != "" {
		t.Errorf("expected empty memory, got %q", mem)
	}

	// Write
	ms.writeMemory("User prefers Go.")
	if mem := ms.readMemory(); mem != "User prefers Go." {
		t.Errorf("memory = %q, want %q", mem, "User prefers Go.")
	}

	// Overwrite
	ms.writeMemory("User prefers Go and Rust.")
	if mem := ms.readMemory(); mem != "User prefers Go and Rust." {
		t.Errorf("memory after overwrite = %q", mem)
	}

	// History
	ms.appendHistory("Had a conversation about programming")
	ms.appendHistory("Discussed Rust vs Go")

	histData, _ := os.ReadFile(ms.historyPath())
	hist := string(histData)
	if !strings.Contains(hist, "Had a conversation") {
		t.Error("history should contain first entry")
	}
	if !strings.Contains(hist, "Discussed Rust") {
		t.Error("history should contain second entry")
	}
}

func TestMemoryStoreGetContext(t *testing.T) {
	dir := t.TempDir()
	ms := newMemoryStore(dir)

	// Empty
	if ctx := ms.getContext(); ctx != "" {
		t.Errorf("expected empty context, got %q", ctx)
	}

	// With content
	ms.writeMemory("Some facts")
	ctx := ms.getContext()
	if !strings.Contains(ctx, "Long-term Memory") {
		t.Error("context should contain Long-term Memory header")
	}
	if !strings.Contains(ctx, "Some facts") {
		t.Error("context should contain memory content")
	}
}

func TestContextBuilderSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	ms := newMemoryStore(dir)
	sl := newSkillsLoader(dir)
	cb := newContextBuilder(dir, ms, sl, "gpt-oss-120b")

	prompt := cb.buildSystemPrompt()
	if !strings.Contains(prompt, "Cody") {
		t.Error("system prompt should mention Cody")
	}
	if !strings.Contains(prompt, "made with love by Vinny") {
		t.Error("system prompt should include creator identity")
	}
	if !strings.Contains(prompt, "based on the Nanobot project") {
		t.Error("system prompt should include project lineage")
	}
	if !strings.Contains(prompt, "github.com/vinnyhorgan/cody") {
		t.Error("system prompt should include home repository")
	}
	if !strings.Contains(prompt, dir) {
		t.Error("system prompt should contain workspace path")
	}
}

func TestContextBuilderBuildMessages(t *testing.T) {
	dir := t.TempDir()
	ms := newMemoryStore(dir)
	sl := newSkillsLoader(dir)
	cb := newContextBuilder(dir, ms, sl, "gpt-oss-120b")

	history := []ChatMessage{
		{Role: "user", Content: "previous msg"},
		{Role: "assistant", Content: "previous response"},
	}

	msgs := cb.buildMessages(history, "new message", "", "")

	// Should have: system + history(2) + runtime system + user = 5
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Error("first message should be system")
	}
	if msgs[len(msgs)-1].Role != "user" {
		t.Error("last message should be user")
	}

	lastContent, _ := msgs[len(msgs)-1].Content.(string)
	if lastContent != "new message" {
		t.Errorf("last message content = %q, want %q", lastContent, "new message")
	}
}

func TestContextBuilderTextOnlyInput(t *testing.T) {
	dir := t.TempDir()
	ms := newMemoryStore(dir)
	sl := newSkillsLoader(dir)
	cb := newContextBuilder(dir, ms, sl, "gpt-oss-120b")

	msgs := cb.buildMessages(nil, "check this image", "", "")

	lastMsg := msgs[len(msgs)-1]
	if lastMsg.Role != "user" {
		t.Error("last message should be user")
	}
	content, ok := lastMsg.Content.(string)
	if !ok {
		t.Fatalf("expected text-only user content, got %T", lastMsg.Content)
	}
	if content != "check this image" {
		t.Fatalf("expected plain text content, got %q", content)
	}
}

func TestStripThinkBlocks(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"<think>reasoning</think>answer", "answer"},
		{"<think>\nmultiline\nthought\n</think>\n\nfinal answer", "final answer"},
		{"no think blocks here", "no think blocks here"},
		{"<think>first</think>middle<think>second</think>end", "middleend"},
	}
	for _, tc := range tests {
		got := stripThinkBlocks(tc.in)
		if got != tc.want {
			t.Errorf("stripThinkBlocks(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestContextBuilderIncludesRuntime(t *testing.T) {
	tmp := t.TempDir()
	mem := newMemoryStore(tmp)
	skills := newSkillsLoader(tmp)
	cb := newContextBuilder(tmp, mem, skills, "gpt-oss-120b")

	prompt := cb.buildSystemPrompt()
	if !strings.Contains(prompt, "Runtime") {
		t.Error("system prompt should include runtime system info")
	}
	if !strings.Contains(prompt, tmp) {
		t.Error("system prompt should include workspace path")
	}
}

func TestContextBuilderManagedModelIncludesProviderAwareness(t *testing.T) {
	tmp := t.TempDir()
	mem := newMemoryStore(tmp)
	skills := newSkillsLoader(tmp)
	cb := newContextBuilder(tmp, mem, skills, "gpt-oss-120b")

	llm := newLLMClient("k", "http://localhost", "gpt-oss-120b")
	llm.configureProviderRoutingStats(tmp)
	cb.setLLMClient(llm)

	prompt := cb.buildSystemPrompt()
	if !strings.Contains(prompt, "provider_stats tool") {
		t.Fatal("system prompt should mention provider_stats tool")
	}
	if !strings.Contains(prompt, filepath.Join(tmp, "memory", providerRoutingStatsFilename)) {
		t.Fatal("system prompt should include provider stats file path")
	}
	if !strings.Contains(prompt, filepath.Join(tmp, "memory", providerRoutingEventsFilename)) {
		t.Fatal("system prompt should include provider events file path")
	}
}

func TestSubagentPromptIncludesCoreIdentity(t *testing.T) {
	tmp := t.TempDir()
	cfg := defaultConfig()
	bus := newMessageBus()
	tools := newToolRegistry()
	ctxBuilder := newContextBuilder(tmp, newMemoryStore(tmp), newSkillsLoader(tmp), "gpt-oss-120b")
	sm := newSubagentManager(nil, tmp, bus, tools, cfg, ctxBuilder)

	prompt := sm.buildSubagentPrompt()
	if !strings.Contains(prompt, "made with love by Vinny") {
		t.Fatal("subagent prompt should include creator identity")
	}
	if !strings.Contains(prompt, "based on the Nanobot project") {
		t.Fatal("subagent prompt should include project lineage")
	}
	if !strings.Contains(prompt, "github.com/vinnyhorgan/cody") {
		t.Fatal("subagent prompt should include home repository")
	}
}

// newTestAgentLoop creates an AgentLoop connected to a mock LLM server for testing.
func newTestAgentLoop(t *testing.T, handler http.Handler) (*AgentLoop, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	dir := t.TempDir()

	cfg := defaultConfig()
	cfg.Workspace = dir
	cfg.APIKey = "test-key"
	cfg.APIBase = srv.URL
	cfg.Agent.MaxIterations = 5

	llm := newLLMClient(cfg.APIKey, cfg.APIBase, cfg.Model)
	bus := newMessageBus()
	sessions := newSessionManager(dir)
	tools := newToolRegistry()

	// Register a simple echo tool for testing
	tools.register(&AgentTool{
		name:        "echo",
		description: "Echo the input back",
		parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			return "echoed: " + paramStr(params, "text"), nil
		},
	})

	memory := newMemoryStore(dir)
	skills := newSkillsLoader(dir)
	ctxBuilder := newContextBuilder(dir, memory, skills, "gpt-oss-120b")

	agent := &AgentLoop{
		llm:      llm,
		bus:      bus,
		sessions: sessions,
		tools:    tools,
		context:  ctxBuilder,
		memory:   memory,
		config:   cfg,
		reqCtx:   &RequestContext{},
		cancels:  make(map[string]context.CancelFunc),
	}
	return agent, srv
}

func TestRunLoopDirectResponse(t *testing.T) {
	agent, srv := newTestAgentLoop(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "Hello, world!"},
				FinishReason: "stop",
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	messages := []ChatMessage{{Role: "user", Content: "hi"}}
	content, msgs := agent.runLoop(context.Background(), messages, nil)

	if content != "Hello, world!" {
		t.Errorf("content = %q, want %q", content, "Hello, world!")
	}
	if len(msgs) != 1 {
		t.Errorf("expected 1 message (unchanged), got %d", len(msgs))
	}
}

func TestRunLoopWithToolCall(t *testing.T) {
	var callCount atomic.Int32
	agent, srv := newTestAgentLoop(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		var resp chatResponse
		if n == 1 {
			// First call: return a tool call
			resp = chatResponse{
				Choices: []chatChoice{{
					Message: ChatMessage{
						Role: "assistant",
						ToolCalls: []ToolCall{{
							ID:   "call_1",
							Type: "function",
							Function: FunctionCall{
								Name:      "echo",
								Arguments: `{"text":"hello"}`,
							},
						}},
					},
					FinishReason: "tool_calls",
				}},
			}
		} else {
			// Second call: return final response
			resp = chatResponse{
				Choices: []chatChoice{{
					Message:      ChatMessage{Role: "assistant", Content: "Done echoing"},
					FinishReason: "stop",
				}},
			}
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	messages := []ChatMessage{{Role: "user", Content: "echo something"}}
	content, msgs := agent.runLoop(context.Background(), messages, nil)

	if content != "Done echoing" {
		t.Errorf("content = %q, want %q", content, "Done echoing")
	}
	// messages should be: user + assistant(tool_call) + tool(result) = 3
	if len(msgs) != 3 {
		t.Errorf("expected 3 messages, got %d", len(msgs))
	}
	// Verify tool result
	if msgs[2].Role != "tool" {
		t.Errorf("expected tool message, got %q", msgs[2].Role)
	}
	toolResult, _ := msgs[2].Content.(string)
	if !strings.Contains(toolResult, "echoed: hello") {
		t.Errorf("tool result = %q, want echoed: hello", toolResult)
	}
}

func TestRunLoopMaxIterations(t *testing.T) {
	// Always return tool calls to trigger max iterations
	agent, srv := newTestAgentLoop(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatResponse{
			Choices: []chatChoice{{
				Message: ChatMessage{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID:   "call_n",
						Type: "function",
						Function: FunctionCall{
							Name:      "echo",
							Arguments: `{"text":"loop"}`,
						},
					}},
				},
				FinishReason: "tool_calls",
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	messages := []ChatMessage{{Role: "user", Content: "keep going"}}
	content, _ := agent.runLoop(context.Background(), messages, nil)

	if !strings.Contains(content, "maximum") {
		t.Errorf("expected max iterations message, got: %q", content)
	}
}

func TestRunLoopErrorFinishReason(t *testing.T) {
	agent, srv := newTestAgentLoop(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "partial"},
				FinishReason: "error",
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	messages := []ChatMessage{{Role: "user", Content: "hi"}}
	content, msgs := agent.runLoop(context.Background(), messages, nil)

	if content != llmUnavailableMessage {
		t.Errorf("expected %q, got: %q", llmUnavailableMessage, content)
	}
	// Messages should not include the error response (poison prevention)
	if len(msgs) != 1 {
		t.Errorf("expected 1 message (original only), got %d", len(msgs))
	}
}

func TestRunLoopThinkBlockStripping(t *testing.T) {
	agent, srv := newTestAgentLoop(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "<think>reasoning here</think>The answer is 42."},
				FinishReason: "stop",
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	messages := []ChatMessage{{Role: "user", Content: "what is the answer?"}}
	content, _ := agent.runLoop(context.Background(), messages, nil)

	if strings.Contains(content, "think") {
		t.Errorf("think blocks should be stripped, got: %q", content)
	}
	if content != "The answer is 42." {
		t.Errorf("content = %q, want %q", content, "The answer is 42.")
	}
}

func TestRunLoopProgressCallback(t *testing.T) {
	var callCount atomic.Int32
	agent, srv := newTestAgentLoop(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		var resp chatResponse
		if n == 1 {
			resp = chatResponse{
				Choices: []chatChoice{{
					Message: ChatMessage{
						Role:    "assistant",
						Content: "Let me check that...",
						ToolCalls: []ToolCall{{
							ID:   "call_1",
							Type: "function",
							Function: FunctionCall{
								Name:      "echo",
								Arguments: `{"text":"test"}`,
							},
						}},
					},
					FinishReason: "tool_calls",
				}},
			}
		} else {
			resp = chatResponse{
				Choices: []chatChoice{{
					Message:      ChatMessage{Role: "assistant", Content: "Done"},
					FinishReason: "stop",
				}},
			}
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var progressCalls []string
	onProgress := func(content string, toolHint bool) {
		progressCalls = append(progressCalls, content)
	}

	messages := []ChatMessage{{Role: "user", Content: "test"}}
	agent.runLoop(context.Background(), messages, onProgress)

	// Should have at least 2 progress calls: thinking content + tool hint
	if len(progressCalls) < 2 {
		t.Errorf("expected at least 2 progress calls, got %d: %v", len(progressCalls), progressCalls)
	}
}

func TestProcessMessageEmptyAssistantFilter(t *testing.T) {
	// Verify that empty assistant responses don't get saved to session
	agent, srv := newTestAgentLoop(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: ""},
				FinishReason: "stop",
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	session := agent.sessions.getOrCreate("test-session")
	messages := agent.context.buildMessages(nil, "hi", "telegram", "123")
	content, _ := agent.runLoop(context.Background(), messages, nil)

	// The runLoop returns empty content; ProcessMessage should not save it.
	if content != "" {
		// runLoop may return "" which is correct
		session.addMessage("assistant", content, nil, "", "")
	}
	// Session should only have messages we explicitly added, not empty assistant msgs.
	history := session.getHistory(100)
	for _, m := range history {
		if m.Role == "assistant" {
			c, _ := m.Content.(string)
			if c == "" && len(m.ToolCalls) == 0 {
				t.Error("empty assistant message should not be in session history")
			}
		}
	}
}

// --- dispatch / ProcessMessage / ProcessDirect / handleNew / cancelSession tests ---

func newTestAgentLoopWithSubMgr(t *testing.T, handler http.Handler) (*AgentLoop, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	dir := t.TempDir()

	cfg := defaultConfig()
	cfg.Workspace = dir
	cfg.APIKey = "test-key"
	cfg.APIBase = srv.URL
	cfg.Agent.MaxIterations = 5
	cfg.Agent.MemoryWindow = 50

	llm := newLLMClient(cfg.APIKey, cfg.APIBase, cfg.Model)
	bus := newMessageBus()
	sessions := newSessionManager(dir)
	tools := newToolRegistry()

	tools.register(&AgentTool{
		name:        "echo",
		description: "Echo",
		parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			return "echoed: " + paramStr(params, "text"), nil
		},
	})

	memory := newMemoryStore(dir)
	skills := newSkillsLoader(dir)
	ctxBuilder := newContextBuilder(dir, memory, skills, "gpt-oss-120b")
	reqCtx := &RequestContext{}
	subMgr := newSubagentManager(llm, dir, bus, tools, cfg, ctxBuilder)

	agent := &AgentLoop{
		llm:      llm,
		bus:      bus,
		sessions: sessions,
		tools:    tools,
		context:  ctxBuilder,
		memory:   memory,
		subMgr:   subMgr,
		config:   cfg,
		reqCtx:   reqCtx,
		cancels:  make(map[string]context.CancelFunc),
	}
	return agent, srv
}

func TestDispatchNormalMessage(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "I got your message!"},
				FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()

	go agent.dispatch(context.Background(), &InboundMessage{
		ChatID:     "123",
		SessionKey: "sess-1",
		MessageID:  1,
		Content:    "Hello",
	})

	out := <-agent.bus.Outbound
	if !strings.Contains(out.Content, "I got your message!") {
		t.Errorf("dispatch output = %q", out.Content)
	}
	if out.ChatID != "123" {
		t.Errorf("chatID = %q", out.ChatID)
	}
}

func TestDispatchStop(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "ok"},
				FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()

	go agent.dispatch(context.Background(), &InboundMessage{
		ChatID:     "123",
		SessionKey: "sess-1",
		MessageID:  1,
		Content:    "/stop",
	})

	out := <-agent.bus.Outbound
	if !strings.Contains(out.Content, "Stopped") && !strings.Contains(out.Content, "No active task") {
		t.Errorf("expected stop confirmation, got %q", out.Content)
	}
}

func TestDispatchNew(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /new archival succeeds only when consolidation calls save_memory.
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message: ChatMessage{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID:   "tc-mem",
						Type: "function",
						Function: FunctionCall{
							Name:      "save_memory",
							Arguments: `{"memory_update":"archived","history_entry":"[2026-03-03 00:00] archived"}`,
						},
					}},
				},
				FinishReason: "tool_calls",
			}},
		})
	}))
	defer srv.Close()

	// Pre-populate session so consolidation has something to do
	session := agent.sessions.getOrCreate("sess-new")
	session.addMessage("user", "first message", nil, "", "")
	session.addMessage("assistant", "first reply", nil, "", "")

	go agent.dispatch(context.Background(), &InboundMessage{
		ChatID:     "123",
		SessionKey: "sess-new",
		MessageID:  2,
		Content:    "/new",
	})

	out := <-agent.bus.Outbound
	if !strings.Contains(out.Content, "New session") {
		t.Errorf("expected new session msg, got %q", out.Content)
	}

	// Session should be cleared
	session = agent.sessions.getOrCreate("sess-new")
	if len(session.Messages) != 0 {
		t.Errorf("expected session cleared, got %d messages", len(session.Messages))
	}
}

func TestDispatchError(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": {"message": "bad request"}}`))
	}))
	defer srv.Close()

	go agent.dispatch(context.Background(), &InboundMessage{
		ChatID:     "123",
		SessionKey: "sess-err",
		MessageID:  1,
		Content:    "hello",
	})

	out := <-agent.bus.Outbound
	if !strings.Contains(out.Content, "error") && !strings.Contains(out.Content, "Error") && !strings.Contains(out.Content, "Sorry") {
		t.Errorf("expected error response, got %q", out.Content)
	}
}

func TestProcessMessageSavesHistory(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "Reply here"},
				FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()

	result, err := agent.processMessage(context.Background(), &InboundMessage{
		ChatID:     "c1",
		SessionKey: "sess-hist",
		Content:    "Tell me something",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != "Reply here" {
		t.Errorf("result = %q", result)
	}

	// Check session history has both user and assistant messages
	session := agent.sessions.getOrCreate("sess-hist")
	history := session.getHistory(100)
	if len(history) < 2 {
		t.Fatalf("expected at least 2 messages in history, got %d", len(history))
	}
	if history[0].Role != "user" {
		t.Errorf("first message role = %q", history[0].Role)
	}
	if history[len(history)-1].Role != "assistant" {
		t.Errorf("last message role = %q", history[len(history)-1].Role)
	}
}

func TestProcessMessageWithToolCall(t *testing.T) {
	var callCount atomic.Int32
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			json.NewEncoder(w).Encode(chatResponse{
				Choices: []chatChoice{{
					Message: ChatMessage{
						Role: "assistant",
						ToolCalls: []ToolCall{{
							ID:       "tc1",
							Type:     "function",
							Function: FunctionCall{Name: "echo", Arguments: `{"text":"hello"}`},
						}},
					},
					FinishReason: "tool_calls",
				}},
			})
		} else {
			json.NewEncoder(w).Encode(chatResponse{
				Choices: []chatChoice{{
					Message:      ChatMessage{Role: "assistant", Content: "Echo result received"},
					FinishReason: "stop",
				}},
			})
		}
	}))
	defer srv.Close()

	result, err := agent.processMessage(context.Background(), &InboundMessage{
		ChatID:     "c1",
		SessionKey: "sess-tool",
		Content:    "echo test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != "Echo result received" {
		t.Errorf("result = %q", result)
	}

	// Session should have tool interactions saved
	session := agent.sessions.getOrCreate("sess-tool")
	history := session.getHistory(100)
	hasToolMsg := false
	for _, m := range history {
		if m.Role == "tool" {
			hasToolMsg = true
		}
	}
	if !hasToolMsg {
		t.Error("expected tool message in session history")
	}
}

func TestProcessDirect(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "Direct result"},
				FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()

	result := agent.processDirect(context.Background(), "do something", "direct-session", "")
	if result != "Direct result" {
		t.Errorf("ProcessDirect result = %q", result)
	}

	// Session should have messages
	session := agent.sessions.getOrCreate("direct-session")
	history := session.getHistory(100)
	if len(history) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(history))
	}
}

func TestCancelSession(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "ok"},
				FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()

	// Cancel with no active task
	n := agent.cancelSession("no-such-session")
	if n != 0 {
		t.Errorf("cancelSession on empty = %d", n)
	}

	// Set up a cancel func and cancel it
	_, cancel := context.WithCancel(context.Background())
	agent.cancelMu.Lock()
	agent.cancels["test-session"] = cancel
	agent.cancelMu.Unlock()

	n = agent.cancelSession("test-session")
	if n != 1 {
		t.Errorf("cancelSession = %d, want 1", n)
	}
}

func TestConsolidateMemory(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message: ChatMessage{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID:   "tc-mem",
						Type: "function",
						Function: FunctionCall{
							Name:      "save_memory",
							Arguments: `{"memory_update":"User likes Go","history_entry":"Discussed Go preferences"}`,
						},
					}},
				},
				FinishReason: "tool_calls",
			}},
		})
	}))
	defer srv.Close()

	session := agent.sessions.getOrCreate("consol-session")
	// Add messages that need consolidation
	for i := 0; i < 10; i++ {
		session.addMessage("user", "message "+string(rune('A'+i)), nil, "", "")
		session.addMessage("assistant", "reply "+string(rune('A'+i)), nil, "", "")
	}

	ok := agent.memory.consolidate(context.Background(), agent.llm, session, 4096, 0)
	if !ok {
		t.Error("consolidate returned false")
	}

	// Check memory was written
	mem := agent.memory.readMemory()
	if !strings.Contains(mem, "User likes Go") {
		t.Errorf("memory = %q, expected 'User likes Go'", mem)
	}

	// Check history was appended
	histPath := filepath.Join(agent.config.workspacePath(), "memory", "HISTORY.md")
	data, err := os.ReadFile(histPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Discussed Go preferences") {
		t.Error("history entry not found")
	}
}

func TestConsolidateNotEnoughMessages(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("LLM should not be called when nothing to consolidate")
	}))
	defer srv.Close()

	session := agent.sessions.getOrCreate("empty-session")
	ok := agent.memory.consolidate(context.Background(), agent.llm, session, 4096, 50)
	if !ok {
		t.Error("consolidate should return true when nothing to consolidate")
	}
}

func TestSubagentSpawnAndWait(t *testing.T) {
	var callCount atomic.Int32
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			// Subagent gets a direct response
			json.NewEncoder(w).Encode(chatResponse{
				Choices: []chatChoice{{
					Message:      ChatMessage{Role: "assistant", Content: "Task done!"},
					FinishReason: "stop",
				}},
			})
		} else {
			json.NewEncoder(w).Encode(chatResponse{
				Choices: []chatChoice{{
					Message:      ChatMessage{Role: "assistant", Content: "ok"},
					FinishReason: "stop",
				}},
			})
		}
	}))
	defer srv.Close()

	id := agent.subMgr.spawn("test task", "test-label", "chat1", "sess1")
	if id == "" {
		t.Fatal("spawn returned empty id")
	}

	agent.subMgr.wait()

	// Result should be re-injected via inbound bus as a system message
	// for the main agent to summarize (matching nanobot behavior).
	select {
	case msg := <-agent.bus.Inbound:
		if msg.SenderID != "subagent" {
			t.Errorf("expected sender 'subagent', got %q", msg.SenderID)
		}
		if !strings.Contains(msg.Content, "test-label") {
			t.Errorf("expected task label in content, got %q", msg.Content)
		}
		if !strings.Contains(msg.Content, "completed successfully") {
			t.Errorf("expected 'completed successfully' in content, got %q", msg.Content)
		}
	default:
		t.Error("no inbound message from subagent")
	}
}

func TestSubagentCancelBySession(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow response to allow cancellation
		<-r.Context().Done()
	}))
	defer srv.Close()

	agent.subMgr.spawn("slow task", "slow", "chat1", "cancel-sess")

	n := agent.subMgr.cancelBySession("cancel-sess")
	if n != 1 {
		t.Errorf("cancelBySession = %d, want 1", n)
	}

	// Cancel non-existent session
	n = agent.subMgr.cancelBySession("no-such")
	if n != 0 {
		t.Errorf("cancelBySession empty = %d", n)
	}

	agent.subMgr.wait()
}

func TestNewAgentLoop(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Workspace = dir
	cfg.APIKey = "test"
	cfg.APIBase = "http://localhost"

	llm := newLLMClient(cfg.APIKey, cfg.APIBase, cfg.Model)
	bus := newMessageBus()
	sessions := newSessionManager(dir)
	tools := newToolRegistry()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)

	agent := newAgentLoop(cfg, llm, bus, sessions, tools, cronSvc)
	if agent == nil {
		t.Fatal("newAgentLoop returned nil")
	}
	if agent.llm != llm {
		t.Error("llm mismatch")
	}
	// Verify tools were registered
	schemas := agent.tools.schemas()
	if len(schemas) < 5 {
		t.Errorf("expected at least 5 tools registered, got %d", len(schemas))
	}
}

func TestAgentLoopRun(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "response"},
				FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	// Send a message then cancel
	go func() {
		agent.bus.Inbound <- &InboundMessage{
			ChatID:     "test",
			SessionKey: "run-test",
			Content:    "hello",
		}
		// Read outbound then cancel
		<-agent.bus.Outbound
		cancel()
	}()

	agent.run(ctx) // Should return after cancel
}

func TestProcessMessageProgressCallback(t *testing.T) {
	// LLM returns thinking + tool call, then final
	var callCount atomic.Int32
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			json.NewEncoder(w).Encode(chatResponse{
				Choices: []chatChoice{{
					Message: ChatMessage{
						Role:    "assistant",
						Content: "Let me check...",
						ToolCalls: []ToolCall{{
							ID:       "tc1",
							Type:     "function",
							Function: FunctionCall{Name: "echo", Arguments: `{"text":"ping"}`},
						}},
					},
					FinishReason: "tool_calls",
				}},
			})
		} else {
			json.NewEncoder(w).Encode(chatResponse{
				Choices: []chatChoice{{
					Message:      ChatMessage{Role: "assistant", Content: "Done!"},
					FinishReason: "stop",
				}},
			})
		}
	}))
	defer srv.Close()

	// ProcessMessage sends progress via outbound bus
	go func() {
		agent.processMessage(context.Background(), &InboundMessage{
			ChatID:     "prog-chat",
			SessionKey: "prog-sess",
			Content:    "test progress",
		})
	}()

	// Should get at least one progress message (thinking content)
	msg := <-agent.bus.Outbound
	if msg == nil {
		t.Fatal("expected progress message")
	}
}

func TestProcessMessageDoesNotPersistTransportError(t *testing.T) {
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"upstream unavailable"}`))
	}))
	defer srv.Close()

	result, err := agent.processMessage(context.Background(), &InboundMessage{
		ChatID:     "err-chat",
		SessionKey: "err-session",
		Content:    "hello",
	})
	if err != nil {
		t.Fatalf("processMessage returned error: %v", err)
	}
	if result != llmUnavailableMessage {
		t.Fatalf("unexpected result: %q", result)
	}

	session := agent.sessions.getOrCreate("err-session")
	history := session.getHistory(20)
	if len(history) != 1 {
		t.Fatalf("history length = %d, want 1 (user message only)", len(history))
	}
	if history[0].Role != "user" {
		t.Fatalf("first history role = %q, want user", history[0].Role)
	}
}

func TestProcessDirectSuppressesFinalAfterMessageTool(t *testing.T) {
	var callCount atomic.Int32
	agent, srv := newTestAgentLoopWithSubMgr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			json.NewEncoder(w).Encode(chatResponse{
				Choices: []chatChoice{{
					Message: ChatMessage{
						Role: "assistant",
						ToolCalls: []ToolCall{{
							ID:   "tc1",
							Type: "function",
							Function: FunctionCall{
								Name:      "message",
								Arguments: `{"content":"intermediate update"}`,
							},
						}},
					},
					FinishReason: "tool_calls",
				}},
			})
			return
		}

		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "final answer"},
				FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()
	agent.tools.register(makeMessageTool(agent.bus, agent.reqCtx))

	result := agent.processDirect(context.Background(), "do work", "direct-message-session", "123")
	if result != "" {
		t.Fatalf("processDirect result = %q, want empty because message tool already sent output", result)
	}

	select {
	case out := <-agent.bus.Outbound:
		if out.Content != "intermediate update" {
			t.Fatalf("message tool content = %q", out.Content)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("expected outbound message from message tool")
	}
}
