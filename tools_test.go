package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParamStr(t *testing.T) {
	params := map[string]any{
		"name": "test",
		"num":  42.0,
	}
	if got := paramStr(params, "name"); got != "test" {
		t.Errorf("paramStr(name) = %q, want %q", got, "test")
	}
	if got := paramStr(params, "missing"); got != "" {
		t.Errorf("paramStr(missing) = %q, want empty", got)
	}
	// Non-string value
	if got := paramStr(params, "num"); got != "" {
		t.Errorf("paramStr(num) = %q, want empty", got)
	}
}

func TestParamInt(t *testing.T) {
	params := map[string]any{
		"count": 42.0,
		"exact": 10,
		"str":   "not a number",
	}
	if got := paramInt(params, "count"); got != 42 {
		t.Errorf("paramInt(count) = %d, want 42", got)
	}
	if got := paramInt(params, "exact"); got != 10 {
		t.Errorf("paramInt(exact) = %d, want 10", got)
	}
	if got := paramInt(params, "missing"); got != 0 {
		t.Errorf("paramInt(missing) = %d, want 0", got)
	}
}

func TestParamBool(t *testing.T) {
	params := map[string]any{
		"flag": true,
		"off":  false,
	}
	if !paramBool(params, "flag") {
		t.Error("paramBool(flag) should be true")
	}
	if paramBool(params, "off") {
		t.Error("paramBool(off) should be false")
	}
	if paramBool(params, "missing") {
		t.Error("paramBool(missing) should be false")
	}
}

func TestResolvePath(t *testing.T) {
	ws := "/home/user/workspace"
	tests := []struct {
		path string
		want string
	}{
		{"file.txt", "/home/user/workspace/file.txt"},
		{"sub/dir/file.txt", "/home/user/workspace/sub/dir/file.txt"},
		{"/absolute/path", "/absolute/path"},
		{"../escape", "/home/user/escape"},
		{"./current", "/home/user/workspace/current"},
	}
	for _, tt := range tests {
		got, err := resolvePath(ws, tt.path, "")
		if err != nil {
			t.Errorf("resolvePath(%q, %q) unexpected error: %v", ws, tt.path, err)
			continue
		}
		if got != tt.want {
			t.Errorf("resolvePath(%q, %q) = %q, want %q", ws, tt.path, got, tt.want)
		}
	}
}

func TestResolvePathAllowedDir(t *testing.T) {
	ws := t.TempDir()

	// Create a file inside the workspace so resolvePath can evaluate it.
	os.WriteFile(filepath.Join(ws, "file.txt"), []byte("x"), 0644)

	// Within allowed dir — should succeed
	got, err := resolvePath(ws, "file.txt", ws)
	if err != nil {
		t.Errorf("expected success, got: %v", err)
	}
	if got != filepath.Join(ws, "file.txt") {
		t.Errorf("got %q", got)
	}

	// Outside allowed dir — should fail
	_, err = resolvePath(ws, "/etc/passwd", ws)
	if err == nil {
		t.Error("expected error for path outside allowed dir")
	}

	// Traversal outside — should fail
	_, err = resolvePath(ws, "../escape", ws)
	if err == nil {
		t.Error("expected error for traversal outside allowed dir")
	}

	// Empty allowedDir — no restriction (no symlink eval)
	_, err = resolvePath(ws, "/etc/passwd", "")
	if err != nil {
		t.Errorf("expected success with empty allowedDir, got: %v", err)
	}
}

func TestResolvePathSymlinkEscape(t *testing.T) {
	ws := t.TempDir()

	// Create a symlink inside the workspace that points outside it.
	outsideDir := t.TempDir()
	os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret"), 0644)
	symlink := filepath.Join(ws, "escape")
	os.Symlink(outsideDir, symlink)

	// With allowedDir set, following the symlink should be blocked.
	_, err := resolvePath(ws, "escape/secret.txt", ws)
	if err == nil {
		t.Error("expected error for symlink escaping allowed directory")
	}

	// Without allowedDir, no restriction applies.
	_, err = resolvePath(ws, "escape/secret.txt", "")
	if err != nil {
		t.Errorf("expected success without allowedDir, got: %v", err)
	}

	// New file in an allowed dir should work (parent exists).
	_, err = resolvePath(ws, "newfile.txt", ws)
	if err != nil {
		t.Errorf("expected success for new file in allowed dir, got: %v", err)
	}
}

func TestReadFileTool(t *testing.T) {
	dir := t.TempDir()
	tool := makeReadFileTool(dir, "")

	// Create test file
	content := "line one\nline two\nline three\n"
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0644)

	ctx := context.Background()

	// Read entire file — returns raw content without line numbers
	result, err := tool.execute(ctx, map[string]any{"path": "test.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if result != content {
		t.Errorf("expected raw content, got: %s", result)
	}

	// File not found
	_, err = tool.execute(ctx, map[string]any{"path": "nonexistent.txt"})
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestWriteFileTool(t *testing.T) {
	dir := t.TempDir()
	tool := makeWriteFileTool(dir, "")
	ctx := context.Background()

	result, err := tool.execute(ctx, map[string]any{"path": "output.txt", "content": "hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "11 bytes") {
		t.Errorf("unexpected result: %s", result)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "output.txt"))
	if string(data) != "hello world" {
		t.Errorf("file content = %q, want %q", string(data), "hello world")
	}

	// Nested directory creation
	_, err = tool.execute(ctx, map[string]any{"path": "sub/dir/file.txt", "content": "nested"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "sub", "dir", "file.txt"))
	if string(data) != "nested" {
		t.Errorf("nested file content = %q", string(data))
	}
}

func TestEditFileTool(t *testing.T) {
	dir := t.TempDir()
	tool := makeEditFileTool(dir, "")
	ctx := context.Background()

	// Create initial file
	os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("hello world"), 0644)

	// Successful edit
	result, err := tool.execute(ctx, map[string]any{
		"path":     "edit.txt",
		"old_text": "world",
		"new_text": "Go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Successfully edited") {
		t.Errorf("unexpected result: %s", result)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "edit.txt"))
	if string(data) != "hello Go" {
		t.Errorf("file content = %q, want %q", string(data), "hello Go")
	}

	// Not found
	_, err = tool.execute(ctx, map[string]any{
		"path":     "edit.txt",
		"old_text": "nonexistent",
		"new_text": "X",
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}

	// Duplicate match
	os.WriteFile(filepath.Join(dir, "dup.txt"), []byte("aaa"), 0644)
	_, err = tool.execute(ctx, map[string]any{
		"path":     "dup.txt",
		"old_text": "a",
		"new_text": "b",
	})
	if err == nil || !strings.Contains(err.Error(), "3 times") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

func TestListDirTool(t *testing.T) {
	dir := t.TempDir()
	tool := makeListDirTool(dir, "")
	ctx := context.Background()

	// Create structure
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)
	os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, "subdir", "file2.txt"), []byte("b"), 0644)

	result, err := tool.execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "📄 file1.txt") {
		t.Error("should list file1.txt with 📄 prefix")
	}
	if !strings.Contains(result, "📁 subdir") {
		t.Error("should list subdir with 📁 prefix")
	}
	// Flat listing: nested files not visible
	if strings.Contains(result, "file2.txt") {
		t.Error("flat listing should not show nested files")
	}

	// Hidden files included (matching nanobot)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0644)
	result, _ = tool.execute(ctx, map[string]any{})
	if !strings.Contains(result, ".hidden") {
		t.Error("should include hidden files")
	}
}

func TestExecTool(t *testing.T) {
	dir := t.TempDir()
	tool := makeExecTool(dir, "", 10, "")
	ctx := context.Background()
	result, err := tool.execute(ctx, map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected 'hello', got: %s", result)
	}

	// Working directory
	result, err = tool.execute(ctx, map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, dir) {
		t.Errorf("expected working dir %s, got: %s", dir, result)
	}

	// Error command
	result, err = tool.execute(ctx, map[string]any{"command": "exit 1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Exit code: 1") {
		t.Errorf("expected 'Exit code: 1', got: %s", result)
	}
}

func TestExecToolDangerousCommands(t *testing.T) {
	dir := t.TempDir()
	tool := makeExecTool(dir, "", 10, "")
	ctx := context.Background()

	dangerous := []string{
		"rm -rf /",
		"rm -rf / --no-preserve-root",
		"mkfs.ext4 /dev/sda",
		":() { :|:& }; :",
	}

	for _, cmd := range dangerous {
		_, err := tool.execute(ctx, map[string]any{"command": cmd})
		if err == nil {
			t.Errorf("expected blocked for: %s", cmd)
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("expected 'blocked' error for %q, got: %v", cmd, err)
		}
	}
}

func TestToolRegistry(t *testing.T) {
	reg := newToolRegistry()

	tool := &AgentTool{
		name:        "test_tool",
		description: "A test tool",
		parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			return "ok", nil
		},
	}

	reg.register(tool)

	// Execute registered tool
	result := reg.execute(context.Background(), "test_tool", "{}")
	if result != "ok" {
		t.Errorf("result = %q, want %q", result, "ok")
	}

	// Execute unknown tool
	result = reg.execute(context.Background(), "unknown", "{}")
	if !strings.Contains(result, "unknown tool") {
		t.Errorf("expected 'unknown tool' error, got: %s", result)
	}

	// Invalid JSON
	result = reg.execute(context.Background(), "test_tool", "not json")
	if !strings.Contains(result, "invalid parameters") {
		t.Errorf("expected 'invalid parameters' error, got: %s", result)
	}

	// Schemas
	schemas := reg.schemas()
	if len(schemas) != 1 {
		t.Errorf("expected 1 schema, got %d", len(schemas))
	}
	if schemas[0].Function.Name != "test_tool" {
		t.Errorf("schema name = %q, want %q", schemas[0].Function.Name, "test_tool")
	}
}

func TestToolRegistrySchemaValidation(t *testing.T) {
	reg := newToolRegistry()
	reg.register(&AgentTool{
		name:        "validate_me",
		description: "validate params",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []any{"add", "list"},
				},
				"count": map[string]any{
					"type":    "integer",
					"minimum": 1,
				},
			},
			"required": []string{"action"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			return "ok", nil
		},
	})

	result := reg.execute(context.Background(), "validate_me", `{}`)
	if !strings.Contains(result, "missing required action") {
		t.Fatalf("expected missing required error, got: %s", result)
	}

	result = reg.execute(context.Background(), "validate_me", `{"action":"bogus"}`)
	if !strings.Contains(result, "must be one of") {
		t.Fatalf("expected enum validation error, got: %s", result)
	}

	result = reg.execute(context.Background(), "validate_me", `{"action":"add","count":0}`)
	if !strings.Contains(result, "must be >=") {
		t.Fatalf("expected numeric minimum validation error, got: %s", result)
	}
}

func TestToolRegistryExcluding(t *testing.T) {
	reg := newToolRegistry()

	for _, name := range []string{"a", "b", "c", "spawn"} {
		n := name
		reg.register(&AgentTool{
			name:        n,
			description: "tool " + n,
			parameters:  map[string]any{"type": "object"},
			execute: func(ctx context.Context, params map[string]any) (string, error) {
				return n, nil
			},
		})
	}

	schemas := reg.schemasExcluding("spawn", "b")
	names := make(map[string]bool)
	for _, s := range schemas {
		names[s.Function.Name] = true
	}

	if names["spawn"] {
		t.Error("spawn should be excluded")
	}
	if names["b"] {
		t.Error("b should be excluded")
	}
	if !names["a"] || !names["c"] {
		t.Error("a and c should be included")
	}
}

func TestToolResultTruncation(t *testing.T) {
	reg := newToolRegistry()
	reg.register(&AgentTool{
		name:       "big",
		parameters: map[string]any{"type": "object"},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			return strings.Repeat("x", 60000), nil
		},
	})

	result := reg.execute(context.Background(), "big", "{}")
	if len(result) > 51000 {
		t.Errorf("result should be truncated, got %d chars", len(result))
	}
	if !strings.Contains(result, "truncated") {
		t.Error("should contain truncation message")
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"<p>Hello</p>", "Hello"},
		{"<div><b>Bold</b> text</div>", "Bold text"},
		{"plain text", "plain text"},
		{"  spaces  everywhere  ", "spaces everywhere"},
	}
	for _, tt := range tests {
		got := stripHTML(tt.input)
		if got != tt.want {
			t.Errorf("stripHTML(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMarshalJSONResult(t *testing.T) {
	ok := marshalJSONResult(map[string]any{"ok": true})
	if ok != `{"ok":true}` {
		t.Fatalf("marshalJSONResult success = %q", ok)
	}

	fallback := marshalJSONResult(map[string]any{"bad": func() {}})
	if fallback != `{"error":"failed to marshal JSON response"}` {
		t.Fatalf("marshalJSONResult fallback = %q", fallback)
	}
}

func TestValidateToolParamsNonObjectSchema(t *testing.T) {
	errs := validateToolParams(map[string]any{"type": "string"}, map[string]any{})
	if len(errs) != 1 || !strings.Contains(errs[0], "parameter should be string") {
		t.Fatalf("unexpected validation errors: %v", errs)
	}
}

func TestNumericValue(t *testing.T) {
	n, ok := numericValue(42, true)
	if !ok || n != 42 {
		t.Fatalf("numericValue int = (%v, %v), want (42, true)", n, ok)
	}

	_, ok = numericValue(42.5, true)
	if ok {
		t.Fatal("numericValue should reject non-integer when integer required")
	}

	n, ok = numericValue(float32(3.5), false)
	if !ok || n != 3.5 {
		t.Fatalf("numericValue float32 = (%v, %v), want (3.5, true)", n, ok)
	}

	_, ok = numericValue("42", false)
	if ok {
		t.Fatal("numericValue should reject non-numeric types")
	}
}

func TestSchemaStringSlice(t *testing.T) {
	fromStrings := schemaStringSlice([]string{"a", "b"})
	if len(fromStrings) != 2 || fromStrings[0] != "a" || fromStrings[1] != "b" {
		t.Fatalf("schemaStringSlice []string = %v", fromStrings)
	}

	fromAny := schemaStringSlice([]any{"a", 1, "b"})
	if len(fromAny) != 2 || fromAny[0] != "a" || fromAny[1] != "b" {
		t.Fatalf("schemaStringSlice []any = %v", fromAny)
	}

	if schemaStringSlice(123) != nil {
		t.Fatal("schemaStringSlice should return nil for unsupported input")
	}
}

func TestDangerousPatterns(t *testing.T) {
	safe := []string{
		"ls -la",
		"rm file.txt",
		"echo hello",
		"git status",
	}
	for _, cmd := range safe {
		if dangerousPatterns.MatchString(cmd) {
			t.Errorf("safe command %q matched dangerous pattern", cmd)
		}
	}

	dangerous := []string{
		"rm -rf /",
		"rm -r tmp",
		"rm -rf / --no-preserve-root",
		"shutdown now",
		"reboot",
		"poweroff",
		"dd if=/dev/zero of=/dev/sda",
		"del /f important.txt",
		"rmdir /s C:\\",
	}
	for _, cmd := range dangerous {
		if !dangerousPatterns.MatchString(cmd) {
			t.Errorf("dangerous command %q should match pattern", cmd)
		}
	}
}

func TestEditFileCloseMatch(t *testing.T) {
	dir := t.TempDir()
	tool := makeEditFileTool(dir, "")

	// Create file with known content
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("Hello World\nFoo Bar\n"), 0644)

	// Try an edit with a near-miss
	result, err := tool.execute(context.Background(), map[string]any{
		"path":     path,
		"old_text": "Helo World", // typo
		"new_text": "Goodbye",
	})
	if err == nil {
		t.Fatal("expected error for near-miss edit")
	}
	_ = result
	errMsg := err.Error()
	if !strings.Contains(errMsg, "not found") {
		t.Errorf("error should mention not found, got: %s", errMsg)
	}
	// Should include a close match hint since "Helo World" is similar to "Hello World"
	if !strings.Contains(errMsg, "similar") {
		t.Errorf("error should include similarity hint, got: %s", errMsg)
	}
}

func TestToolRegistryErrorHints(t *testing.T) {
	reg := newToolRegistry()
	reg.register(&AgentTool{
		name:        "test_tool",
		description: "test",
		parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			return "", fmt.Errorf("something went wrong")
		},
	})

	// Unknown tool should list available tools
	result := reg.execute(context.Background(), "nonexistent", "{}")
	if !strings.Contains(result, "unknown tool") {
		t.Error("should report unknown tool")
	}
	if !strings.Contains(result, "test_tool") {
		t.Error("should list available tools")
	}

	// Tool error should include retry hint
	result = reg.execute(context.Background(), "test_tool", "{}")
	if !strings.Contains(result, "Analyze the error") {
		t.Error("should include retry hint on error")
	}

	// Invalid JSON should include retry hint
	result = reg.execute(context.Background(), "test_tool", "not json")
	if !strings.Contains(result, "Analyze the error") {
		t.Error("should include retry hint on invalid params")
	}
}

func TestFindCloseMatch(t *testing.T) {
	content := "Hello World\nFoo Bar\nBaz Qux"

	// Exact match — no hint needed (but this function is only called when count=0)
	// So test with near-misses
	hint := findCloseMatch(content, "Helo World")
	if hint == "" {
		t.Error("expected close match hint for 'Helo World'")
	}
	if !strings.Contains(hint, "similar") {
		t.Error("hint should mention similarity")
	}

	// Completely different — no hint
	hint = findCloseMatch(content, "ZZZZZZZZZZZ")
	if hint != "" {
		t.Errorf("expected no hint for completely different text, got: %s", hint)
	}
}

func TestSimilarity(t *testing.T) {
	// Identical strings
	if s := similarity("hello", "hello"); s != 1.0 {
		t.Errorf("identical strings should have similarity 1.0, got %f", s)
	}
	// Empty strings
	if s := similarity("", "hello"); s != 0.0 {
		t.Errorf("empty vs non-empty should be 0.0, got %f", s)
	}
	// Similar strings
	s := similarity("Hello World", "Helo World")
	if s < 0.8 {
		t.Errorf("similar strings should have high similarity, got %f", s)
	}
	// Very different strings
	s = similarity("abc", "xyz")
	if s > 0.1 {
		t.Errorf("different strings should have low similarity, got %f", s)
	}
}

func TestExecToolWorkingDir(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub")
	os.MkdirAll(subdir, 0755)
	os.WriteFile(filepath.Join(subdir, "marker.txt"), []byte("found"), 0644)

	tool := makeExecTool(dir, "", 10, "")
	result, err := tool.execute(context.Background(), map[string]any{
		"command":     "cat marker.txt",
		"working_dir": "sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "found") {
		t.Errorf("exec with working_dir should find file in subdir, got: %s", result)
	}
}

func TestWebFetchToolHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><article><p>Hello from test server</p></article></body></html>`))
	}))
	defer srv.Close()

	tool := makeWebFetchTool()
	result, err := tool.execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Hello from test server") {
		t.Errorf("expected fetched content, got: %s", result)
	}
}

func TestWebFetchToolPlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("Plain text response"))
	}))
	defer srv.Close()

	tool := makeWebFetchTool()
	result, err := tool.execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Plain text") {
		t.Errorf("expected plain text content, got: %s", result)
	}
}

func TestWebFetchToolNoURL(t *testing.T) {
	tool := makeWebFetchTool()
	_, err := tool.execute(context.Background(), map[string]any{})
	if err == nil {
		t.Error("expected error for missing url")
	}
}

func TestWebFetchToolTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Generate content > 50000 chars
		w.Write([]byte("<html><body><article><p>"))
		for i := 0; i < 60000; i++ {
			w.Write([]byte("x"))
		}
		w.Write([]byte("</p></article></body></html>"))
	}))
	defer srv.Close()

	tool := makeWebFetchTool()
	result, err := tool.execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "truncated") {
		t.Error("expected truncation marker for large content")
	}
}

func TestWebSearchToolNoQuery(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "test-key")
	tool := makeWebSearchTool()
	_, err := tool.execute(context.Background(), map[string]any{})
	if err == nil {
		t.Error("expected error for missing query")
	}
}

func TestWebSearchToolNoAPIKey(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "")
	tool := makeWebSearchTool()
	_, err := tool.execute(context.Background(), map[string]any{"query": "test"})
	if err == nil {
		t.Error("expected error for missing BRAVE_API_KEY")
	}
}

// --- registerDefaultTools ---

func TestRegisterDefaultTools(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Workspace = dir
	cfg.Tools.AllowedDir = dir
	bus := newMessageBus()
	reqCtx := &RequestContext{}
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)
	llm := newLLMClient("k", "http://localhost", "m")
	tools := newToolRegistry()
	ctxB := newContextBuilder(dir, newMemoryStore(dir), newSkillsLoader(dir), "gpt-oss-120b")
	subMgr := newSubagentManager(llm, dir, bus, tools, cfg, ctxB)

	registerDefaultTools(tools, cfg, dir, bus, reqCtx, cronSvc, subMgr)

	schemas := tools.schemas()
	if len(schemas) < 9 {
		t.Errorf("expected at least 9 tools, got %d", len(schemas))
	}

	names := make(map[string]bool)
	for _, s := range schemas {
		names[s.Function.Name] = true
	}
	for _, n := range []string{"read_file", "write_file", "edit_file", "list_dir", "exec", "web_search", "web_fetch", "message", "cron", "spawn"} {
		if !names[n] {
			t.Errorf("missing tool: %s", n)
		}
	}
}

// --- makeMessageTool ---

func TestMessageTool(t *testing.T) {
	bus := newMessageBus()
	reqCtx := &RequestContext{ChatID: "chat-42", MessageID: 99}
	tool := makeMessageTool(bus, reqCtx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err := tool.execute(context.Background(), map[string]any{"content": "hello user"})
		if err != nil {
			t.Errorf("message tool error: %v", err)
		}
		if !strings.Contains(result, "sent") {
			t.Errorf("result = %q", result)
		}
	}()

	msg := <-bus.Outbound
	if msg.Content != "hello user" {
		t.Errorf("outbound content = %q", msg.Content)
	}
	if msg.ChatID != "chat-42" {
		t.Errorf("chatID = %q", msg.ChatID)
	}
	if msg.ReplyToMessageID != 99 {
		t.Errorf("replyToMessageID = %d, want 99", msg.ReplyToMessageID)
	}
	wg.Wait()
	if !reqCtx.MessageSent {
		t.Error("MessageSent should be true")
	}
}

func TestMessageToolEmptyContent(t *testing.T) {
	bus := newMessageBus()
	reqCtx := &RequestContext{}
	tool := makeMessageTool(bus, reqCtx)

	_, err := tool.execute(context.Background(), map[string]any{"content": ""})
	if err == nil {
		t.Error("expected error for empty content")
	}
}

func TestMessageToolCustomChatIDDoesNotMarkSentInTurn(t *testing.T) {
	bus := newMessageBus()
	reqCtx := &RequestContext{ChatID: "chat-42", MessageID: 99}
	tool := makeMessageTool(bus, reqCtx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := tool.execute(context.Background(), map[string]any{
			"content": "hello elsewhere",
			"chat_id": "chat-100",
		})
		if err != nil {
			t.Errorf("message tool error: %v", err)
		}
	}()

	msg := <-bus.Outbound
	if msg.ChatID != "chat-100" {
		t.Fatalf("chatID = %q, want chat-100", msg.ChatID)
	}

	wg.Wait()
	if reqCtx.MessageSent {
		t.Fatal("MessageSent should remain false for non-current chat targets")
	}
}

// --- makeCronTool ---

func TestCronToolList(t *testing.T) {
	dir := t.TempDir()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)
	tool := makeCronTool(cronSvc, &RequestContext{ChatID: "test-chat"})

	result, err := tool.execute(context.Background(), map[string]any{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if result != "No scheduled jobs." {
		t.Errorf("list empty = %q", result)
	}
}

func TestCronToolAddListRemove(t *testing.T) {
	dir := t.TempDir()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)
	tool := makeCronTool(cronSvc, &RequestContext{ChatID: "test-chat"})

	// Add
	result, err := tool.execute(context.Background(), map[string]any{
		"action":        "add",
		"name":          "Test Job",
		"every_seconds": float64(3600),
		"message":       "do something",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Job created") {
		t.Errorf("add result = %q", result)
	}

	// List
	result, err = tool.execute(context.Background(), map[string]any{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Test Job") {
		t.Errorf("list result = %q", result)
	}

	// Get job ID
	jobs := cronSvc.listJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	jobID := jobs[0].ID

	// Disable
	result, err = tool.execute(context.Background(), map[string]any{"action": "disable", "job_id": jobID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "disabled") {
		t.Errorf("disable result = %q", result)
	}

	// Enable
	result, err = tool.execute(context.Background(), map[string]any{"action": "enable", "job_id": jobID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "enabled") {
		t.Errorf("enable result = %q", result)
	}

	// Remove
	result, err = tool.execute(context.Background(), map[string]any{"action": "remove", "job_id": jobID})
	if err != nil {
		t.Fatal(err)
	}
	if result != "Job removed." {
		t.Errorf("remove result = %q", result)
	}

	// Remove non-existent
	_, err = tool.execute(context.Background(), map[string]any{"action": "remove", "job_id": "no-such"})
	if err == nil {
		t.Error("expected error for non-existent job")
	}
}

func TestCronToolAddMissingFields(t *testing.T) {
	dir := t.TempDir()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)
	tool := makeCronTool(cronSvc, &RequestContext{ChatID: "test-chat"})

	_, err := tool.execute(context.Background(), map[string]any{"action": "add", "name": "x"})
	if err == nil {
		t.Error("expected error for missing schedule/message")
	}
	// Also test missing schedule but with message
	_, err = tool.execute(context.Background(), map[string]any{"action": "add", "name": "x", "message": "hi"})
	if err == nil {
		t.Error("expected error for missing schedule params")
	}
}

func TestCronToolUnknownAction(t *testing.T) {
	dir := t.TempDir()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)
	tool := makeCronTool(cronSvc, &RequestContext{ChatID: "test-chat"})

	_, err := tool.execute(context.Background(), map[string]any{"action": "bogus"})
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

func TestCronToolAddWithTimezone(t *testing.T) {
	dir := t.TempDir()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)
	tool := makeCronTool(cronSvc, &RequestContext{ChatID: "test-chat"})

	// Valid timezone
	_, err := tool.execute(context.Background(), map[string]any{
		"action":    "add",
		"name":      "TZ Job",
		"cron_expr": "0 9 * * *",
		"message":   "morning",
		"tz":        "America/New_York",
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Invalid timezone
	_, err = tool.execute(context.Background(), map[string]any{
		"action":    "add",
		"name":      "Bad TZ",
		"cron_expr": "0 9 * * *",
		"message":   "morning",
		"tz":        "Invalid/Timezone",
	})
	if err == nil {
		t.Error("expected error for invalid timezone")
	}
}

func TestCronToolEnableDisableNotFound(t *testing.T) {
	dir := t.TempDir()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)
	tool := makeCronTool(cronSvc, &RequestContext{ChatID: "test-chat"})

	_, err := tool.execute(context.Background(), map[string]any{"action": "enable", "job_id": "nope"})
	if err == nil {
		t.Error("expected error for enable non-existent")
	}
	_, err = tool.execute(context.Background(), map[string]any{"action": "disable", "job_id": "nope"})
	if err == nil {
		t.Error("expected error for disable non-existent")
	}
}

// --- makeSpawnTool ---

func TestSpawnTool(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Workspace = dir
	cfg.APIKey = "k"
	cfg.APIBase = "http://localhost"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	cfg.APIBase = srv.URL

	llm := newLLMClient(cfg.APIKey, cfg.APIBase, cfg.Model)
	bus := newMessageBus()
	tools := newToolRegistry()
	ctxB := newContextBuilder(dir, newMemoryStore(dir), newSkillsLoader(dir), "gpt-oss-120b")
	subMgr := newSubagentManager(llm, dir, bus, tools, cfg, ctxB)
	reqCtx := &RequestContext{ChatID: "chat1", SessionKey: "sess1"}

	tool := makeSpawnTool(subMgr, reqCtx)
	result, err := tool.execute(context.Background(), map[string]any{"task": "run some test"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "started") {
		t.Errorf("result = %q", result)
	}

	subMgr.wait()
	msg := <-bus.Inbound
	if !strings.Contains(msg.Content, "completed successfully") {
		t.Errorf("inbound = %q", msg.Content)
	}
}

func TestSpawnToolWithLabel(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Workspace = dir
	cfg.APIKey = "k"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	cfg.APIBase = srv.URL

	llm := newLLMClient(cfg.APIKey, cfg.APIBase, cfg.Model)
	bus := newMessageBus()
	tools := newToolRegistry()
	ctxB := newContextBuilder(dir, newMemoryStore(dir), newSkillsLoader(dir), "gpt-oss-120b")
	subMgr := newSubagentManager(llm, dir, bus, tools, cfg, ctxB)
	reqCtx := &RequestContext{ChatID: "c", SessionKey: "s"}

	tool := makeSpawnTool(subMgr, reqCtx)
	result, _ := tool.execute(context.Background(), map[string]any{"task": "do x", "label": "my-label"})
	if !strings.Contains(result, "my-label") {
		t.Errorf("label not in result: %q", result)
	}
	subMgr.wait()
	<-bus.Inbound
}

// --- Write file edge cases ---

func TestWriteFileNewFile(t *testing.T) {
	dir := t.TempDir()
	tool := makeWriteFileTool(dir, dir)

	// Create a new file in the existing workspace dir
	result, err := tool.execute(context.Background(), map[string]any{
		"path":    filepath.Join(dir, "newfile.txt"),
		"content": "hello world",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Wrote") {
		t.Errorf("result = %q", result)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "newfile.txt"))
	if string(data) != "hello world" {
		t.Errorf("file content = %q", data)
	}
}

func TestWriteFileOverwrite(t *testing.T) {
	dir := t.TempDir()
	tool := makeWriteFileTool(dir, dir)
	target := filepath.Join(dir, "existing.txt")
	os.WriteFile(target, []byte("old"), 0644)

	result, err := tool.execute(context.Background(), map[string]any{
		"path":    target,
		"content": "new content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Wrote") {
		t.Errorf("result = %q", result)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "new content" {
		t.Errorf("content = %q", string(data))
	}
}

// --- list_dir recursive ---

func TestListDirRecursive(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0755)
	os.WriteFile(filepath.Join(dir, "top.txt"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "a", "mid.txt"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "a", "b", "deep.txt"), []byte("x"), 0644)

	tool := makeListDirTool(dir, dir)
	result, err := tool.execute(context.Background(), map[string]any{
		"path": dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Flat listing: should show top-level entries only
	if !strings.Contains(result, "📄 top.txt") {
		t.Errorf("should include top.txt: %s", result)
	}
	if !strings.Contains(result, "📁 a") {
		t.Errorf("should include directory a: %s", result)
	}
	// Should NOT show nested files (flat listing)
	if strings.Contains(result, "deep.txt") {
		t.Errorf("flat listing should not show nested files: %s", result)
	}
}

// --- web_search with key ---

func TestWebSearchWithAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "test query" {
			t.Errorf("query = %q", r.URL.Query().Get("q"))
		}
		w.Write([]byte(`{"web":{"results":[{"title":"Result 1","url":"https://example.com","description":"A test result"}]}}`))
	}))
	defer srv.Close()

	// Override env for this test
	old := os.Getenv("BRAVE_API_KEY")
	os.Setenv("BRAVE_API_KEY", "test-key")
	defer os.Setenv("BRAVE_API_KEY", old)

	tool := makeWebSearchTool()
	// We can't easily override the URL, but we test the tool exists and validates
	if tool.name != "web_search" {
		t.Errorf("name = %q", tool.name)
	}
}

// --- web_fetch max_length ---

func TestWebFetchPlainText(t *testing.T) {
	body := strings.Repeat("Hello world. ", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	tool := makeWebFetchTool()
	result, err := tool.execute(context.Background(), map[string]any{
		"url": srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) == 0 {
		t.Error("expected non-empty result")
	}
}

// --- CronService start/tick ---

func TestCronServiceStartStop(t *testing.T) {
	dir := t.TempDir()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cronSvc.start(ctx)

	if !cronSvc.running {
		t.Error("expected running=true")
	}

	cancel()
	time.Sleep(50 * time.Millisecond)

	cronSvc.mu.Lock()
	running := cronSvc.running
	cronSvc.mu.Unlock()
	if running {
		t.Error("expected running=false after cancel")
	}
}

func TestCronServiceTick(t *testing.T) {
	dir := t.TempDir()

	var executed []string
	onJob := func(message string, deliver bool, sessionKey, chatID string) string {
		executed = append(executed, message)
		return "done"
	}
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), onJob)
	cronSvc.loadStore()

	// Add a job that's already due
	cronSvc.store.Jobs = append(cronSvc.store.Jobs, &CronJob{
		ID:       "test-job",
		Name:     "Test",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "every", Raw: "every 1h"},
		Message:  "do it",
		State:    CronJobState{NextRunAt: time.Now().Add(-1 * time.Hour)},
	})

	cronSvc.tick()

	if len(executed) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(executed))
	}
	if executed[0] != "do it" {
		t.Errorf("executed = %q", executed[0])
	}

	// Check next run was updated
	job := cronSvc.store.Jobs[0]
	if job.State.LastStatus != "ok" {
		t.Errorf("status = %q", job.State.LastStatus)
	}
}

func TestCronServiceTickOneShotDisables(t *testing.T) {
	dir := t.TempDir()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), func(string, bool, string, string) string { return "" })
	cronSvc.loadStore()

	future := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	cronSvc.store.Jobs = append(cronSvc.store.Jobs, &CronJob{
		ID:       "at-job",
		Name:     "One Shot",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "at", Raw: "at " + future},
		Message:  "once",
		State:    CronJobState{NextRunAt: time.Now().Add(-1 * time.Minute)},
	})

	cronSvc.tick()

	job := cronSvc.store.Jobs[0]
	if job.Enabled {
		t.Error("one-shot job should be disabled after running")
	}
	if job.State.LastStatus != "empty" {
		t.Errorf("status = %q (empty because handler returns empty)", job.State.LastStatus)
	}
}

func TestCronServiceTickNoHandler(t *testing.T) {
	dir := t.TempDir()
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), nil)
	cronSvc.loadStore()
	cronSvc.store.Jobs = append(cronSvc.store.Jobs, &CronJob{
		ID:       "no-handler",
		Name:     "NoHandler",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "every", Raw: "every 1h"},
		Message:  "msg",
		State:    CronJobState{NextRunAt: time.Now().Add(-1 * time.Minute)},
	})

	cronSvc.tick() // Should not panic
	if cronSvc.store.Jobs[0].State.LastStatus != "empty" {
		t.Errorf("status = %q", cronSvc.store.Jobs[0].State.LastStatus)
	}
}

func TestCronServiceTickDisabledJobSkipped(t *testing.T) {
	dir := t.TempDir()
	called := false
	cronSvc := newCronService(filepath.Join(dir, "cron.json"), func(string, bool, string, string) string {
		called = true
		return "ran"
	})
	cronSvc.loadStore()
	cronSvc.store.Jobs = append(cronSvc.store.Jobs, &CronJob{
		ID:       "disabled",
		Name:     "Disabled",
		Enabled:  false,
		Schedule: CronSchedule{Kind: "every", Raw: "every 1h"},
		Message:  "msg",
		State:    CronJobState{NextRunAt: time.Now().Add(-1 * time.Minute)},
	})

	cronSvc.tick()
	if called {
		t.Error("disabled job should not execute")
	}
}

// --- HeartbeatService ---

func TestHeartbeatServiceStartDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Heartbeat.Enabled = false
	llm := newLLMClient("k", "http://localhost", "m")

	hs := newHeartbeatService(dir, llm, cfg, nil, nil)
	hs.start(context.Background()) // Should return immediately without panic
}

func TestHeartbeatServiceTick(t *testing.T) {
	dir := t.TempDir()

	// Write a heartbeat file
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("Check the weather"), 0644)

	var executeCalled bool
	onExec := func(ctx context.Context, content, key, chatID string) string {
		executeCalled = true
		return "done"
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"tc1","type":"function","function":{"name":"heartbeat_decision","arguments":"{\"action\":\"run\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	cfg := defaultConfig()
	cfg.Heartbeat.Enabled = true
	cfg.Heartbeat.IntervalMinutes = 1
	llm := newLLMClient("k", srv.URL, "m")

	hs := newHeartbeatService(dir, llm, cfg, nil, onExec)
	hs.tick(context.Background())

	if !executeCalled {
		t.Error("expected onExecute to be called when heartbeat decides to run")
	}
}

func TestHeartbeatServiceTickSkip(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("Some tasks"), 0644)

	var executeCalled bool
	onExec := func(ctx context.Context, content, key, chatID string) string {
		executeCalled = true
		return ""
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"tc1","type":"function","function":{"name":"heartbeat_decision","arguments":"{\"action\":\"skip\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	cfg := defaultConfig()
	cfg.Heartbeat.Enabled = true
	llm := newLLMClient("k", srv.URL, "m")

	hs := newHeartbeatService(dir, llm, cfg, nil, onExec)
	hs.tick(context.Background())

	if executeCalled {
		t.Error("onExecute should not be called when heartbeat skips")
	}
}

func TestHeartbeatServiceTickNoFile(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Heartbeat.Enabled = true
	llm := newLLMClient("k", "http://localhost", "m")

	hs := newHeartbeatService(dir, llm, cfg, nil, nil)
	hs.tick(context.Background()) // Should return early, no panic
}

func TestHeartbeatServiceTickEmptyFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("   "), 0644)
	cfg := defaultConfig()
	cfg.Heartbeat.Enabled = true
	llm := newLLMClient("k", "http://localhost", "m")

	hs := newHeartbeatService(dir, llm, cfg, nil, nil)
	hs.tick(context.Background()) // Should return early, no panic
}

func TestHeartbeatServiceStopNilCancel(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	llm := newLLMClient("k", "http://localhost", "m")
	hs := newHeartbeatService(dir, llm, cfg, nil, nil)
	hs.stop() // Should not panic even without start
}

func TestHeartbeatServiceMinInterval(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Heartbeat.IntervalMinutes = 0 // Should default to 30 min
	llm := newLLMClient("k", "http://localhost", "m")
	hs := newHeartbeatService(dir, llm, cfg, nil, nil)
	if hs.interval != 30*time.Minute {
		t.Errorf("interval = %v, want 30m", hs.interval)
	}
}
