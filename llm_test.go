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
)

func TestChatMessageSerialization(t *testing.T) {
	msg := ChatMessage{
		Role:    "assistant",
		Content: "hello",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var decoded ChatMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Role != "assistant" {
		t.Errorf("role = %q, want %q", decoded.Role, "assistant")
	}
}

func TestToolCallSerialization(t *testing.T) {
	tc := ToolCall{
		ID:   "call_123",
		Type: "function",
		Function: FunctionCall{
			Name:      "read_file",
			Arguments: `{"path": "test.txt"}`,
		},
	}

	data, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}

	var decoded ToolCall
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.ID != "call_123" {
		t.Errorf("id = %q, want %q", decoded.ID, "call_123")
	}
	if decoded.Function.Name != "read_file" {
		t.Errorf("function.name = %q, want %q", decoded.Function.Name, "read_file")
	}
}

func TestToolDefStructure(t *testing.T) {
	def := ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        "test",
			Description: "A test function",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"arg": map[string]any{"type": "string"},
				},
			},
		},
	}

	data, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}

	// Verify it produces valid JSON
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	if raw["type"] != "function" {
		t.Errorf("type = %v", raw["type"])
	}
}

func TestLLMResponseContent(t *testing.T) {
	// Simulate a non-tool-call response
	resp := LLMResponse{
		Content:   "Hello, how can I help?",
		ToolCalls: nil,
		Usage:     LLMUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
	}

	if resp.Content == "" {
		t.Error("content should not be empty")
	}
	if len(resp.ToolCalls) != 0 {
		t.Error("tool_calls should be empty")
	}
	if resp.Usage.TotalTokens != 120 {
		t.Errorf("total_tokens = %d, want 120", resp.Usage.TotalTokens)
	}
}

// mockLLMServer creates a test HTTP server that mimics the OpenAI chat completions API.
// The handler func receives the decoded request and returns the response to send.
func mockLLMServer(t *testing.T, handler func(req chatRequest) (int, chatResponse)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
			http.Error(w, "bad request", 400)
			return
		}
		status, resp := handler(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestLLMClientChatSuccess(t *testing.T) {
	srv := mockLLMServer(t, func(req chatRequest) (int, chatResponse) {
		return 200, chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "Hello!"},
				FinishReason: "stop",
			}},
			Usage: LLMUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}
	})
	defer srv.Close()

	client := newLLMClient("test-key", srv.URL, "test-model")
	resp, err := client.chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil, 100, 0.7, "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello!" {
		t.Errorf("content = %q, want %q", resp.Content, "Hello!")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", resp.FinishReason, "stop")
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("total_tokens = %d, want 15", resp.Usage.TotalTokens)
	}
}

func TestLLMClientChatWithToolCalls(t *testing.T) {
	srv := mockLLMServer(t, func(req chatRequest) (int, chatResponse) {
		return 200, chatResponse{
			Choices: []chatChoice{{
				Message: ChatMessage{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID:   "call_1",
						Type: "function",
						Function: FunctionCall{
							Name:      "read_file",
							Arguments: `{"path":"test.txt"}`,
						},
					}},
				},
				FinishReason: "tool_calls",
			}},
		}
	})
	defer srv.Close()

	client := newLLMClient("test-key", srv.URL, "test-model")
	resp, err := client.chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "read test.txt"},
	}, []ToolDef{{
		Type:     "function",
		Function: FunctionDef{Name: "read_file", Description: "Read a file"},
	}}, 100, 0.7, "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("tool name = %q, want %q", resp.ToolCalls[0].Function.Name, "read_file")
	}
}

func TestLLMClientChatRetryOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"server error"}`))
			return
		}
		resp := chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "recovered"},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newLLMClient("test-key", srv.URL, "test-model")
	resp, err := client.chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil, 100, 0.7, "")

	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if resp.Content != "recovered" {
		t.Errorf("content = %q, want %q", resp.Content, "recovered")
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls (2 retries + 1 success), got %d", calls.Load())
	}
}

func TestLLMClientChat4xxNoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	client := newLLMClient("test-key", srv.URL, "test-model")
	_, err := client.chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil, 100, 0.7, "")

	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call (no retry on 4xx), got %d", calls.Load())
	}
}

func TestLLMClientChatErrorFinishReason(t *testing.T) {
	srv := mockLLMServer(t, func(req chatRequest) (int, chatResponse) {
		return 200, chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "partial output"},
				FinishReason: "error",
			}},
		}
	})
	defer srv.Close()

	client := newLLMClient("test-key", srv.URL, "test-model")
	resp, err := client.chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil, 100, 0.7, "")

	// The LLM client itself doesn't filter finish_reason — that's the agent loop's job.
	// But it should return the finish_reason so the caller can check.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.FinishReason != "error" {
		t.Errorf("finish_reason = %q, want %q", resp.FinishReason, "error")
	}
}

func TestLLMClientChatNoChoices(t *testing.T) {
	srv := mockLLMServer(t, func(req chatRequest) (int, chatResponse) {
		return 200, chatResponse{Choices: nil}
	})
	defer srv.Close()

	client := newLLMClient("test-key", srv.URL, "test-model")
	_, err := client.chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil, 100, 0.7, "")

	if err == nil {
		t.Fatal("expected error for empty choices")
	}
}

func TestLLMClientChatContextCancelled(t *testing.T) {
	srv := mockLLMServer(t, func(req chatRequest) (int, chatResponse) {
		return 200, chatResponse{
			Choices: []chatChoice{{
				Message: ChatMessage{Role: "assistant", Content: "ok"},
			}},
		}
	})
	defer srv.Close()

	client := newLLMClient("test-key", srv.URL, "test-model")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := client.chat(ctx, []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil, 100, 0.7, "")

	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestLLMClientChatRequestFormat(t *testing.T) {
	var captured chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&captured)

		// Check auth header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer my-key" {
			t.Errorf("Authorization = %q, want %q", auth, "Bearer my-key")
		}

		resp := chatResponse{
			Choices: []chatChoice{{
				Message: ChatMessage{Role: "assistant", Content: "ok"},
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newLLMClient("my-key", srv.URL, "my-model")
	client.chat(context.Background(), []ChatMessage{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "hi"},
	}, []ToolDef{{
		Type:     "function",
		Function: FunctionDef{Name: "test", Description: "test"},
	}}, 2048, 0.5, "")

	if captured.Model != "my-model" {
		t.Errorf("model = %q, want %q", captured.Model, "my-model")
	}
	if len(captured.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(captured.Messages))
	}
	if len(captured.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(captured.Tools))
	}
	if captured.MaxTokens != 2048 {
		t.Errorf("max_tokens = %d, want 2048", captured.MaxTokens)
	}
	if captured.Temperature == nil || *captured.Temperature != 0.5 {
		t.Error("temperature should be 0.5")
	}
}

func TestRequestModelOpenRouterPrefixStripped(t *testing.T) {
	client := newLLMClient("sk-or-v1-test", "https://openrouter.ai/api/v1", "openrouter/gpt-oss-120b")
	if got := client.requestModel(); got != "gpt-oss-120b" {
		t.Fatalf("requestModel() = %q, want %q", got, "gpt-oss-120b")
	}
}

func TestRequestModelNonOpenRouterUnchanged(t *testing.T) {
	client := newLLMClient("sk-test", "https://api.example.com/v1", "openrouter/gpt-oss-120b")
	if got := client.requestModel(); got != "openrouter/gpt-oss-120b" {
		t.Fatalf("requestModel() = %q, want unchanged model", got)
	}
}

func TestBuildLLMProvidersManagedOrder(t *testing.T) {
	cfg := defaultConfig()
	cfg.Model = "gpt-oss-120b"
	cfg.Groq.APIKey = "gsk-test"
	cfg.Cerebras.APIKey = "csk-test"
	cfg.OpenRouter.APIKey = "sk-or-test"

	providers := buildLLMProviders(cfg)
	if len(providers) != 3 {
		t.Fatalf("provider count = %d, want 3", len(providers))
	}
	if providers[0].name != "groq" {
		t.Fatalf("providers[0] = %q, want groq", providers[0].name)
	}
	if providers[0].model != "openai/gpt-oss-120b" {
		t.Fatalf("groq model = %q, want %q", providers[0].model, "openai/gpt-oss-120b")
	}
	if providers[0].apiBase != "https://api.groq.com/openai/v1" {
		t.Fatalf("groq apiBase = %q, want %q", providers[0].apiBase, "https://api.groq.com/openai/v1")
	}
	if providers[1].name != "cerebras" {
		t.Fatalf("providers[1] = %q, want cerebras", providers[1].name)
	}
	if providers[1].model != "gpt-oss-120b" {
		t.Fatalf("cerebras model = %q, want %q", providers[1].model, "gpt-oss-120b")
	}
	if providers[1].apiBase != "https://api.cerebras.ai/v1" {
		t.Fatalf("cerebras apiBase = %q, want %q", providers[1].apiBase, "https://api.cerebras.ai/v1")
	}
	if providers[2].name != "openrouter" {
		t.Fatalf("providers[2] = %q, want openrouter", providers[2].name)
	}
	if providers[2].model != "openai/gpt-oss-120b:free" {
		t.Fatalf("openrouter model = %q, want %q", providers[2].model, "openai/gpt-oss-120b:free")
	}
	if providers[2].apiBase != "https://openrouter.ai/api/v1" {
		t.Fatalf("openrouter apiBase = %q, want %q", providers[2].apiBase, "https://openrouter.ai/api/v1")
	}
}

func TestBuildLLMProvidersDefaultMode(t *testing.T) {
	cfg := defaultConfig()
	cfg.Model = "custom-model"
	cfg.APIBase = "https://api.example.com/v1"
	cfg.APIKey = "sk-custom"

	providers := buildLLMProviders(cfg)
	if len(providers) != 1 {
		t.Fatalf("provider count = %d, want 1", len(providers))
	}
	if providers[0].name != "default" {
		t.Fatalf("provider name = %q, want default", providers[0].name)
	}
	if providers[0].apiBase != "https://api.example.com/v1" {
		t.Fatalf("provider apiBase = %q", providers[0].apiBase)
	}
}

func TestLLMClientChatProviderFallback(t *testing.T) {
	var groqCalls atomic.Int32
	groqSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		groqCalls.Add(1)
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"rate limit"}`))
	}))
	defer groqSrv.Close()

	var cerebrasCalls atomic.Int32
	cerebrasSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cerebrasCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "fallback worked"},
				FinishReason: "stop",
			}},
		})
	}))
	defer cerebrasSrv.Close()

	client := newLLMClient("unused", "http://unused", "ignored")
	client.providers = []llmProvider{
		{name: "groq", apiKey: "gsk-test", apiBase: groqSrv.URL, model: "gpt-oss-120b"},
		{name: "cerebras", apiKey: "csk-test", apiBase: cerebrasSrv.URL, model: "gpt-oss-120b"},
	}

	resp, err := client.chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "hello"},
	}, nil, 256, 0.1, "")
	if err != nil {
		t.Fatalf("chat() error = %v", err)
	}
	if resp.Content != "fallback worked" {
		t.Fatalf("content = %q, want %q", resp.Content, "fallback worked")
	}
	if groqCalls.Load() != 1 {
		t.Fatalf("groq calls = %d, want 1", groqCalls.Load())
	}
	if cerebrasCalls.Load() != 1 {
		t.Fatalf("cerebras calls = %d, want 1", cerebrasCalls.Load())
	}
}

func TestLLMClientChatAllProvidersFail(t *testing.T) {
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer failSrv.Close()

	client := newLLMClient("unused", "http://unused", "ignored")
	client.providers = []llmProvider{
		{name: "groq", apiKey: "gsk-test", apiBase: failSrv.URL, model: "gpt-oss-120b"},
		{name: "cerebras", apiKey: "csk-test", apiBase: failSrv.URL, model: "gpt-oss-120b"},
	}

	_, err := client.chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "hello"},
	}, nil, 256, 0.1, "")
	if err == nil {
		t.Fatal("expected error when all providers fail")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "all configured LLM providers failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLLMClientDisablesProviderAfterModelNotFound(t *testing.T) {
	var groqCalls atomic.Int32
	groqSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		groqCalls.Add(1)
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"message":"The model ` + "`gpt-oss-120b`" + ` does not exist or you do not have access to it.","type":"invalid_request_error","code":"model_not_found"}}`))
	}))
	defer groqSrv.Close()

	var cerebrasCalls atomic.Int32
	cerebrasSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cerebrasCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "ok"},
				FinishReason: "stop",
			}},
		})
	}))
	defer cerebrasSrv.Close()

	client := newLLMClient("unused", "http://unused", "ignored")
	client.providers = []llmProvider{
		{name: "groq", apiKey: "gsk-test", apiBase: groqSrv.URL, model: "gpt-oss-120b"},
		{name: "cerebras", apiKey: "csk-test", apiBase: cerebrasSrv.URL, model: "gpt-oss-120b"},
	}

	// First call should hit Groq once, then fallback to Cerebras.
	if _, err := client.chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil, 256, 0.1, ""); err != nil {
		t.Fatalf("first chat() error = %v", err)
	}
	if groqCalls.Load() != 1 {
		t.Fatalf("groq calls after first request = %d, want 1", groqCalls.Load())
	}
	if cerebrasCalls.Load() != 1 {
		t.Fatalf("cerebras calls after first request = %d, want 1", cerebrasCalls.Load())
	}

	// Second call should skip Groq entirely because it was disabled.
	if _, err := client.chat(context.Background(), []ChatMessage{{Role: "user", Content: "again"}}, nil, 256, 0.1, ""); err != nil {
		t.Fatalf("second chat() error = %v", err)
	}
	if groqCalls.Load() != 1 {
		t.Fatalf("groq calls after second request = %d, want still 1", groqCalls.Load())
	}
	if cerebrasCalls.Load() != 2 {
		t.Fatalf("cerebras calls after second request = %d, want 2", cerebrasCalls.Load())
	}
}

func TestLLMClientProviderRoutingStatsPersistence(t *testing.T) {
	var groqCalls atomic.Int32
	groqSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		groqCalls.Add(1)
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"rate limit"}`))
	}))
	defer groqSrv.Close()

	var cerebrasCalls atomic.Int32
	cerebrasSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cerebrasCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{
				Message:      ChatMessage{Role: "assistant", Content: "ok"},
				FinishReason: "stop",
			}},
			Usage: LLMUsage{PromptTokens: 20, CompletionTokens: 4, TotalTokens: 24},
		})
	}))
	defer cerebrasSrv.Close()

	tmp := t.TempDir()
	client := newLLMClient("unused", "http://unused", "gpt-oss-120b")
	client.providers = []llmProvider{
		{name: "groq", apiKey: "gsk-test", apiBase: groqSrv.URL, model: "openai/gpt-oss-120b"},
		{name: "cerebras", apiKey: "csk-test", apiBase: cerebrasSrv.URL, model: "gpt-oss-120b"},
	}
	client.configureProviderRoutingStats(tmp)

	if _, err := client.chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil, 256, 0.1, ""); err != nil {
		t.Fatalf("chat() error = %v", err)
	}

	report := client.providerRoutingReport(10)
	if report.Summary.RequestsTotal != 1 {
		t.Fatalf("requests_total = %d, want 1", report.Summary.RequestsTotal)
	}
	if report.Summary.RequestsSucceeded != 1 {
		t.Fatalf("requests_succeeded = %d, want 1", report.Summary.RequestsSucceeded)
	}
	if report.Summary.ProviderAttemptsTotal != 2 {
		t.Fatalf("provider_attempts_total = %d, want 2", report.Summary.ProviderAttemptsTotal)
	}
	if report.Summary.Providers["groq"].Failures != 1 {
		t.Fatalf("groq failures = %d, want 1", report.Summary.Providers["groq"].Failures)
	}
	if report.Summary.Providers["cerebras"].Successes != 1 {
		t.Fatalf("cerebras successes = %d, want 1", report.Summary.Providers["cerebras"].Successes)
	}
	if len(report.RecentEvents) != 2 {
		t.Fatalf("recent events = %d, want 2", len(report.RecentEvents))
	}

	statsPath := filepath.Join(tmp, "memory", providerRoutingStatsFilename)
	eventsPath := filepath.Join(tmp, "memory", providerRoutingEventsFilename)

	statsData, err := os.ReadFile(statsPath)
	if err != nil {
		t.Fatalf("read stats file: %v", err)
	}
	var summary ProviderRoutingStats
	if err := json.Unmarshal(statsData, &summary); err != nil {
		t.Fatalf("parse stats file: %v", err)
	}
	if summary.RequestsTotal != 1 {
		t.Fatalf("persisted requests_total = %d, want 1", summary.RequestsTotal)
	}

	eventData, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(eventData)), "\n")
	if len(lines) != 2 {
		t.Fatalf("event line count = %d, want 2", len(lines))
	}
	if groqCalls.Load() != 1 || cerebrasCalls.Load() != 1 {
		t.Fatalf("provider calls groq=%d cerebras=%d, want 1/1", groqCalls.Load(), cerebrasCalls.Load())
	}
}

func TestShouldDisableProvider(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "model not found",
			err:  &llmAPIError{StatusCode: 404, Body: `{"error":{"code":"model_not_found"}}`},
			want: true,
		},
		{
			name: "unauthorized",
			err:  &llmAPIError{StatusCode: 401, Body: `{"error":"bad key"}`},
			want: true,
		},
		{
			name: "forbidden",
			err:  &llmAPIError{StatusCode: 403, Body: `{"error":"forbidden"}`},
			want: true,
		},
		{
			name: "rate limited",
			err:  &llmAPIError{StatusCode: 429, Body: `{"error":"rate limit"}`},
			want: false,
		},
		{
			name: "non api error",
			err:  context.DeadlineExceeded,
			want: false,
		},
	}

	for _, tt := range tests {
		if got := shouldDisableProvider(tt.err); got != tt.want {
			t.Fatalf("%s: shouldDisableProvider() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestIsManagedGPTOSSModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{model: "gpt-oss-120b", want: true},
		{model: "openrouter/gpt-oss-120b", want: true},
		{model: "openai/gpt-oss-120b:free", want: true},
		{model: "gpt-4o", want: false},
	}
	for _, tt := range tests {
		if got := isManagedGPTOSSModel(tt.model); got != tt.want {
			t.Fatalf("isManagedGPTOSSModel(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}
