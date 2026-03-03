package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionAddMessage(t *testing.T) {
	s := &Session{Key: "test", CreatedAt: time.Now(), UpdatedAt: time.Now()}

	s.addMessage("user", "hello", nil, "", "")
	s.addMessage("assistant", "hi there", nil, "", "")

	if len(s.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(s.Messages))
	}
	if s.Messages[0].Role != "user" {
		t.Errorf("message 0 role = %q, want %q", s.Messages[0].Role, "user")
	}
	if s.Messages[0].Ts == "" {
		t.Error("message should have timestamp")
	}
}

func TestSessionGetHistory(t *testing.T) {
	s := &Session{Key: "test", CreatedAt: time.Now(), UpdatedAt: time.Now()}

	// Add messages
	s.addMessage("user", "msg1", nil, "", "")
	s.addMessage("assistant", "resp1", nil, "", "")
	s.addMessage("user", "msg2", nil, "", "")
	s.addMessage("assistant", "resp2", nil, "", "")

	history := s.getHistory(0)
	if len(history) != 4 {
		t.Errorf("expected 4 history messages, got %d", len(history))
	}

	// With limit
	history = s.getHistory(2)
	if len(history) != 2 {
		t.Errorf("expected 2 history messages with limit, got %d", len(history))
	}

	// After consolidation
	s.mu.Lock()
	s.LastConsolidated = 2
	s.mu.Unlock()

	history = s.getHistory(0)
	if len(history) != 2 {
		t.Errorf("expected 2 unconsolidated messages, got %d", len(history))
	}
}

func TestSessionGetHistoryAlignToUser(t *testing.T) {
	s := &Session{Key: "test", CreatedAt: time.Now(), UpdatedAt: time.Now()}

	// Start with non-user messages (orphaned tool results)
	s.addMessage("tool", "result", nil, "tc1", "exec")
	s.addMessage("user", "hello", nil, "", "")
	s.addMessage("assistant", "hi", nil, "", "")

	history := s.getHistory(0)
	if len(history) != 2 {
		t.Errorf("expected 2 messages (aligned to user), got %d", len(history))
	}
	if history[0].Role != "user" {
		t.Errorf("first message should be user, got %q", history[0].Role)
	}
}

func TestSessionUnconsolidatedCount(t *testing.T) {
	s := &Session{Key: "test", CreatedAt: time.Now(), UpdatedAt: time.Now()}

	s.addMessage("user", "a", nil, "", "")
	s.addMessage("assistant", "b", nil, "", "")

	if s.unconsolidatedCount() != 2 {
		t.Errorf("expected 2 unconsolidated, got %d", s.unconsolidatedCount())
	}

	s.mu.Lock()
	s.LastConsolidated = 1
	s.mu.Unlock()

	if s.unconsolidatedCount() != 1 {
		t.Errorf("expected 1 unconsolidated, got %d", s.unconsolidatedCount())
	}
}

func TestSessionManagerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mgr := newSessionManager(dir)

	// Create and populate session
	s := mgr.getOrCreate("test-session")
	s.addMessage("user", "hello", nil, "", "")
	s.addMessage("assistant", "hi", nil, "", "")

	// Save
	if err := mgr.save(s); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	// Verify file exists
	path := filepath.Join(dir, "sessions", "test-session.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session file not created: %v", err)
	}

	// Load into fresh manager
	mgr2 := newSessionManager(dir)
	s2 := mgr2.getOrCreate("test-session")

	if len(s2.Messages) != 2 {
		t.Fatalf("expected 2 messages after reload, got %d", len(s2.Messages))
	}
	content, _ := s2.Messages[0].Content.(string)
	if content != "hello" {
		t.Errorf("message 0 content = %q, want %q", content, "hello")
	}
}

func TestSessionManagerConsolidationPersist(t *testing.T) {
	dir := t.TempDir()
	mgr := newSessionManager(dir)

	s := mgr.getOrCreate("consolidate-test")
	s.addMessage("user", "a", nil, "", "")
	s.addMessage("assistant", "b", nil, "", "")
	s.mu.Lock()
	s.LastConsolidated = 1
	s.mu.Unlock()

	mgr.save(s)

	mgr2 := newSessionManager(dir)
	s2 := mgr2.getOrCreate("consolidate-test")

	if s2.LastConsolidated != 1 {
		t.Errorf("last_consolidated = %d, want 1", s2.LastConsolidated)
	}
}

func TestSessionManagerToolCallsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mgr := newSessionManager(dir)

	s := mgr.getOrCreate("tools-test")
	toolCalls := []ToolCall{{
		ID:   "tc_001",
		Type: "function",
		Function: FunctionCall{
			Name:      "read_file",
			Arguments: `{"path": "test.txt"}`,
		},
	}}

	s.addMessage("user", "read test.txt", nil, "", "")
	s.addMessage("assistant", nil, toolCalls, "", "")
	s.addMessage("tool", "file contents here", nil, "tc_001", "read_file")

	mgr.save(s)

	mgr2 := newSessionManager(dir)
	s2 := mgr2.getOrCreate("tools-test")

	if len(s2.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(s2.Messages))
	}

	// The tool call ID should be preserved
	if s2.Messages[2].ToolCallID != "tc_001" {
		t.Errorf("tool_call_id = %q, want %q", s2.Messages[2].ToolCallID, "tc_001")
	}
}

func TestMessageBus(t *testing.T) {
	bus := newMessageBus()

	go func() {
		bus.Inbound <- &InboundMessage{Content: "hello"}
	}()

	select {
	case msg := <-bus.Inbound:
		if msg.Content != "hello" {
			t.Errorf("content = %q, want %q", msg.Content, "hello")
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for message")
	}
}

func TestSessionChatMessageConversion(t *testing.T) {
	sm := SessionMessage{
		Role:    "assistant",
		Content: "hello",
	}
	cm := sm.toChatMessage()
	if cm.Role != "assistant" {
		t.Errorf("role = %q, want %q", cm.Role, "assistant")
	}

	content, _ := cm.Content.(string)
	if content != "hello" {
		t.Errorf("content = %q, want %q", content, "hello")
	}
}

func TestSafeFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"telegram:12345", "telegram_12345"},
		{"simple", "simple"},
		{"a/b\\c:d", "a_b_c_d"},
	}
	for _, tt := range tests {
		got := safeFilename(tt.input)
		if got != tt.want {
			t.Errorf("safeFilename(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTimestamp(t *testing.T) {
	ts := timestamp()
	if ts == "" {
		t.Error("timestamp should not be empty")
	}
	// Should parse as RFC3339
	_, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Errorf("timestamp %q not valid RFC3339: %v", ts, err)
	}
}

func TestSessionJSONLFormat(t *testing.T) {
	dir := t.TempDir()
	mgr := newSessionManager(dir)

	s := mgr.getOrCreate("format-test")
	s.addMessage("user", "hello", nil, "", "")
	mgr.save(s)

	// Read raw JSONL
	data, err := os.ReadFile(filepath.Join(dir, "sessions", "format-test.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	lines := 0
	for _, line := range splitNonEmpty(string(data)) {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Errorf("invalid JSON line: %v", err)
		}
		lines++
	}

	// 1 meta line + 1 message line
	if lines != 2 {
		t.Errorf("expected 2 JSONL lines, got %d", lines)
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range splitLines(s) {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func TestSessionClear(t *testing.T) {
	s := &Session{Key: "test", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	s.addMessage("user", "hi", nil, "", "")
	s.addMessage("assistant", "hello", nil, "", "")
	s.LastConsolidated = 1

	s.clear()

	if len(s.Messages) != 0 {
		t.Errorf("expected 0 messages after clear, got %d", len(s.Messages))
	}
	if s.LastConsolidated != 0 {
		t.Errorf("expected LastConsolidated=0, got %d", s.LastConsolidated)
	}
}

func TestSessionManagerLoadNonexistent(t *testing.T) {
	dir := t.TempDir()
	sm := newSessionManager(dir)
	session := sm.getOrCreate("brand-new-key")
	if session.Key != "brand-new-key" {
		t.Errorf("key = %q", session.Key)
	}
	if len(session.Messages) != 0 {
		t.Error("new session should have no messages")
	}
}

func TestSessionManagerSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	sm1 := newSessionManager(dir)
	session := sm1.getOrCreate("persist-key")
	session.addMessage("user", "remember me", nil, "", "")
	sm1.save(session)

	// New manager, same workspace
	sm2 := newSessionManager(dir)
	reloaded := sm2.getOrCreate("persist-key")
	if len(reloaded.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(reloaded.Messages))
	}
	msg := reloaded.Messages[0].toChatMessage()
	c, _ := msg.Content.(string)
	if c != "remember me" {
		t.Errorf("content = %q", c)
	}
}

func TestSessionManagerCacheHit(t *testing.T) {
	dir := t.TempDir()
	sm := newSessionManager(dir)
	s1 := sm.getOrCreate("cached")
	s2 := sm.getOrCreate("cached")
	if s1 != s2 {
		t.Error("expected same session pointer from cache")
	}
}

func TestSessionLoadMalformedJSONL(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	os.MkdirAll(sessDir, 0755)
	// Write a JSONL file with one valid and one malformed line
	f := filepath.Join(sessDir, "bad-session.jsonl")
	os.WriteFile(f, []byte(`{"role":"user","content":"hello"}
not-json
{"role":"assistant","content":"hi"}
`), 0644)

	sm := newSessionManager(dir)
	session := sm.getOrCreate("bad-session")
	// Should load 2 valid messages, skip the malformed one
	if len(session.Messages) != 2 {
		t.Errorf("expected 2 messages (skipping malformed), got %d", len(session.Messages))
	}
}
