package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

// --- OpenAI-compatible API types ---

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []ChatMessage `json:"messages"`
	Tools           []ToolDef     `json:"tools,omitempty"`
	ToolChoice      string        `json:"tool_choice,omitempty"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	Usage   LLMUsage     `json:"usage"`
}

type chatChoice struct {
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type LLMUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type LLMResponse struct {
	Content      string
	ToolCalls    []ToolCall
	Usage        LLMUsage
	FinishReason string
}

// LLMClient calls an OpenAI-compatible chat completions endpoint.
type LLMClient struct {
	apiKey  string
	apiBase string
	model   string
	client  *http.Client
	// providers is optional. When present, chat() tries them in order.
	providers []llmProvider
	// disabledProviders tracks provider identities disabled for this process
	// after a permanent API error (for example model_not_found).
	disabledMu        sync.RWMutex
	disabledProviders map[string]string
}

func newLLMClient(apiKey, apiBase, model string) *LLMClient {
	return &LLMClient{
		apiKey:  apiKey,
		apiBase: apiBase,
		model:   model,
		client:  &http.Client{Timeout: 5 * time.Minute},
	}
}

func newLLMClientFromConfig(cfg *Config) *LLMClient {
	if cfg == nil {
		return newLLMClient("", "", "")
	}
	client := newLLMClient(cfg.cerebrasAPIKey(), cfg.APIBase, cfg.Model)
	client.providers = buildLLMProviders(cfg)
	return client
}

type llmProvider struct {
	name    string
	apiKey  string
	apiBase string
	model   string
}

type llmAPIError struct {
	StatusCode int
	Body       string
}

func (e *llmAPIError) Error() string {
	return fmt.Sprintf("LLM API error %d: %s", e.StatusCode, e.Body)
}

const (
	cerebrasAPIBase   = "https://api.cerebras.ai/v1"
	groqAPIBase       = "https://api.groq.com/openai/v1"
	openRouterAPIBase = "https://openrouter.ai/api/v1"
)

func buildLLMProviders(cfg *Config) []llmProvider {
	model := strings.TrimSpace(cfg.Model)
	if isManagedGPTOSSModel(model) {
		return compactLLMProviders([]llmProvider{
			{name: "groq", apiKey: cfg.Groq.APIKey, apiBase: groqAPIBase, model: "openai/gpt-oss-120b"},
			{name: "cerebras", apiKey: cfg.cerebrasAPIKey(), apiBase: cerebrasAPIBase, model: "gpt-oss-120b"},
			{name: "openrouter", apiKey: cfg.OpenRouter.APIKey, apiBase: openRouterAPIBase, model: "openai/gpt-oss-120b:free"},
		})
	}
	return compactLLMProviders([]llmProvider{
		{name: "default", apiKey: cfg.cerebrasAPIKey(), apiBase: cfg.APIBase, model: cfg.Model},
	})
}

func compactLLMProviders(providers []llmProvider) []llmProvider {
	out := make([]llmProvider, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		p.apiKey = strings.TrimSpace(p.apiKey)
		p.apiBase = strings.TrimSpace(p.apiBase)
		p.model = strings.TrimSpace(p.model)
		if p.apiKey == "" || p.apiBase == "" {
			continue
		}
		identity := p.apiBase + "|" + p.model + "|" + p.apiKey
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		out = append(out, p)
	}
	return out
}

func isManagedGPTOSSModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	name = strings.TrimPrefix(name, "openrouter/")
	name = strings.TrimPrefix(name, "openai/")
	return strings.HasPrefix(name, "gpt-oss-120b")
}

func (c *LLMClient) providerChain() []llmProvider {
	if len(c.providers) > 0 {
		return c.providers
	}
	return compactLLMProviders([]llmProvider{
		{name: "default", apiKey: c.apiKey, apiBase: c.apiBase, model: c.model},
	})
}

func (c *LLMClient) requestModel() string {
	return c.requestModelFor(llmProvider{
		apiKey:  c.apiKey,
		apiBase: c.apiBase,
		model:   c.model,
	})
}

func (c *LLMClient) requestModelFor(provider llmProvider) string {
	model := strings.TrimSpace(provider.model)
	if model == "" {
		return model
	}
	// Accept nanobot/litellm-style "openrouter/<model>" when calling OpenRouter
	// directly through its OpenAI-compatible API.
	isOpenRouter := strings.Contains(strings.ToLower(provider.apiBase), "openrouter") || strings.HasPrefix(provider.apiKey, "sk-or-")
	if isOpenRouter && strings.HasPrefix(model, "openrouter/") {
		return strings.TrimPrefix(model, "openrouter/")
	}
	return model
}

func (c *LLMClient) chat(ctx context.Context, messages []ChatMessage, tools []ToolDef, maxTokens int, temperature float64, reasoningEffort string) (*LLMResponse, error) {
	providers := c.providerChain()
	if len(providers) == 0 {
		return nil, fmt.Errorf("no configured LLM providers")
	}
	var lastErr error
	attempted := false
	for _, provider := range providers {
		if disabled, reason := c.providerDisabled(provider); disabled {
			slog.Info("Skipping disabled LLM provider", "provider", provider.name, "reason", reason)
			continue
		}
		attempted = true
		resp, err := c.chatWithProvider(ctx, provider, messages, tools, maxTokens, temperature, reasoningEffort)
		if err == nil {
			return resp, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		lastErr = err
		if shouldDisableProvider(err) {
			c.disableProvider(provider, err.Error())
			slog.Warn("LLM provider disabled due permanent error", "provider", provider.name, "err", err)
			continue
		}
		slog.Warn("LLM provider failed, trying next provider", "provider", provider.name, "err", err)
	}
	if !attempted {
		return nil, fmt.Errorf("no available LLM providers (all disabled)")
	}
	return nil, fmt.Errorf("all configured LLM providers failed: %w", lastErr)
}

func (c *LLMClient) providerDisabled(provider llmProvider) (bool, string) {
	key := providerIdentity(provider)
	c.disabledMu.RLock()
	defer c.disabledMu.RUnlock()
	if c.disabledProviders == nil {
		return false, ""
	}
	reason, ok := c.disabledProviders[key]
	return ok, reason
}

func (c *LLMClient) disableProvider(provider llmProvider, reason string) {
	key := providerIdentity(provider)
	c.disabledMu.Lock()
	defer c.disabledMu.Unlock()
	if c.disabledProviders == nil {
		c.disabledProviders = make(map[string]string, len(c.providers))
	}
	c.disabledProviders[key] = reason
}

func providerIdentity(provider llmProvider) string {
	return strings.TrimSpace(provider.name) + "|" + strings.TrimSpace(provider.apiBase) + "|" + strings.TrimSpace(provider.model)
}

func shouldDisableProvider(err error) bool {
	var apiErr *llmAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusNotFound:
		body := strings.ToLower(apiErr.Body)
		return strings.Contains(body, "model_not_found") || strings.Contains(body, "does not exist")
	}
	return false
}

func (c *LLMClient) chatWithProvider(ctx context.Context, provider llmProvider, messages []ChatMessage, tools []ToolDef, maxTokens int, temperature float64, reasoningEffort string) (*LLMResponse, error) {
	req := chatRequest{
		Model:    c.requestModelFor(provider),
		Messages: sanitizeMessages(messages),
	}
	if len(tools) > 0 {
		req.Tools = tools
		req.ToolChoice = "auto"
	}
	if maxTokens > 0 {
		req.MaxTokens = maxTokens
	}
	if temperature >= 0 {
		req.Temperature = &temperature
	}
	if reasoningEffort != "" {
		req.ReasoningEffort = reasoningEffort
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(provider.apiBase, "/") + "/chat/completions"
	for attempt := range 3 {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+provider.apiKey)

		resp, err := c.client.Do(httpReq)
		if err != nil {
			if attempt < 2 {
				slog.Warn("LLM request failed, retrying", "provider", provider.name, "attempt", attempt+1, "err", err)
				if !sleepWithContext(ctx, time.Duration(1<<uint(attempt))*time.Second) {
					return nil, fmt.Errorf("LLM request canceled: %w", ctx.Err())
				}
				continue
			}
			return nil, fmt.Errorf("LLM request failed: %w", err)
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			if attempt < 2 {
				slog.Warn("LLM response read failed, retrying", "provider", provider.name, "attempt", attempt+1, "err", readErr)
				if !sleepWithContext(ctx, time.Duration(1<<uint(attempt))*time.Second) {
					return nil, fmt.Errorf("LLM request canceled: %w", ctx.Err())
				}
				continue
			}
			return nil, fmt.Errorf("read response body: %w", readErr)
		}

		if resp.StatusCode >= 500 && attempt < 2 {
			slog.Warn("LLM server error, retrying", "provider", provider.name, "status", resp.StatusCode, "attempt", attempt+1)
			if !sleepWithContext(ctx, time.Duration(1<<uint(attempt))*time.Second) {
				return nil, fmt.Errorf("LLM request canceled: %w", ctx.Err())
			}
			continue
		}
		if resp.StatusCode != 200 {
			return nil, &llmAPIError{StatusCode: resp.StatusCode, Body: string(respBody)}
		}

		var chatResp chatResponse
		if err := json.Unmarshal(respBody, &chatResp); err != nil {
			return nil, fmt.Errorf("parse response: %w", err)
		}
		if len(chatResp.Choices) == 0 {
			return nil, fmt.Errorf("no choices in LLM response")
		}

		choice := chatResp.Choices[0]
		content, _ := choice.Message.Content.(string)
		return &LLMResponse{
			Content:      content,
			ToolCalls:    choice.Message.ToolCalls,
			Usage:        chatResp.Usage,
			FinishReason: choice.FinishReason,
		}, nil
	}
	return nil, fmt.Errorf("LLM request failed after retries")
}

// sanitizeMessages replaces empty text content that causes provider 400 errors.
// Empty content can appear when tool results return nothing. Most providers
// reject empty-string content.
func sanitizeMessages(messages []ChatMessage) []ChatMessage {
	result := make([]ChatMessage, len(messages))
	for i, msg := range messages {
		content, isStr := msg.Content.(string)
		if isStr && content == "" {
			clean := msg
			if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
				clean.Content = nil
			} else {
				clean.Content = "(empty)"
			}
			result[i] = clean
			continue
		}
		result[i] = msg
	}
	return result
}

// repairJSON attempts basic fixes on malformed JSON from weaker models:
// trailing commas before } or ], and single-quoted strings.
func repairJSON(s string) string {
	// Remove trailing commas before closing braces/brackets
	s = strings.ReplaceAll(s, ",}", "}")
	s = strings.ReplaceAll(s, ",]", "]")
	return s
}

// transcribeAudio sends audio to Groq's Whisper API and returns the transcription.
func transcribeAudio(groqAPIKey string, audioData []byte, filename string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(audioData); err != nil {
		return "", err
	}
	if err := w.WriteField("model", "whisper-large-v3-turbo"); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", "https://api.groq.com/openai/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+groqAPIKey)

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcription request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read transcription response: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("transcription error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse transcription: %w", err)
	}
	return result.Text, nil
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
