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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

type ProviderRoutingProviderStats struct {
	Attempts         int64  `json:"attempts"`
	Successes        int64  `json:"successes"`
	Failures         int64  `json:"failures"`
	SkippedDisabled  int64  `json:"skipped_disabled"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	LastUsedAt       string `json:"last_used_at,omitempty"`
	LastRequestID    int64  `json:"last_request_id,omitempty"`
	LastError        string `json:"last_error,omitempty"`
}

type ProviderRoutingLastRequest struct {
	RequestID   int64  `json:"request_id"`
	Status      string `json:"status"`
	Provider    string `json:"provider,omitempty"`
	CompletedAt string `json:"completed_at"`
	Error       string `json:"error,omitempty"`
}

type ProviderRoutingStats struct {
	Model                 string                                  `json:"model"`
	FailoverOrder         []string                                `json:"failover_order"`
	Since                 string                                  `json:"since"`
	UpdatedAt             string                                  `json:"updated_at"`
	RequestsTotal         int64                                   `json:"requests_total"`
	RequestsSucceeded     int64                                   `json:"requests_succeeded"`
	RequestsFailed        int64                                   `json:"requests_failed"`
	ProviderAttemptsTotal int64                                   `json:"provider_attempts_total"`
	Providers             map[string]ProviderRoutingProviderStats `json:"providers"`
	LastRequestID         int64                                   `json:"last_request_id,omitempty"`
	LastRequest           *ProviderRoutingLastRequest             `json:"last_request,omitempty"`
	StatsFile             string                                  `json:"stats_file,omitempty"`
	EventsFile            string                                  `json:"events_file,omitempty"`
}

type ProviderRoutingEvent struct {
	Timestamp        string `json:"timestamp"`
	RequestID        int64  `json:"request_id"`
	Attempt          int    `json:"attempt"`
	Provider         string `json:"provider"`
	APIBase          string `json:"api_base"`
	ProviderModel    string `json:"provider_model"`
	RequestModel     string `json:"request_model"`
	Outcome          string `json:"outcome"`
	HTTPStatus       int    `json:"http_status,omitempty"`
	DurationMS       int64  `json:"duration_ms,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	TotalTokens      int    `json:"total_tokens,omitempty"`
	Error            string `json:"error,omitempty"`
}

type ProviderRoutingReport struct {
	Summary      ProviderRoutingStats   `json:"summary"`
	RecentEvents []ProviderRoutingEvent `json:"recent_events"`
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

	statsMu      sync.RWMutex
	stats        ProviderRoutingStats
	recentEvents []ProviderRoutingEvent
	statsPath    string
	eventsPath   string
	requestSeq   atomic.Int64
}

func newLLMClient(apiKey, apiBase, model string) *LLMClient {
	now := time.Now().UTC().Format(time.RFC3339)
	return &LLMClient{
		apiKey:  apiKey,
		apiBase: apiBase,
		model:   model,
		client:  &http.Client{Timeout: 5 * time.Minute},
		stats: ProviderRoutingStats{
			Model:     strings.TrimSpace(model),
			Since:     now,
			UpdatedAt: now,
			Providers: make(map[string]ProviderRoutingProviderStats),
		},
	}
}

func newLLMClientFromConfig(cfg *Config) *LLMClient {
	if cfg == nil {
		return newLLMClient("", "", "")
	}
	client := newLLMClient(cfg.cerebrasAPIKey(), cfg.APIBase, cfg.Model)
	client.providers = buildLLMProviders(cfg)
	client.configureProviderRoutingStats(cfg.workspacePath())
	client.setFailoverOrder(client.providerChain())
	client.persistProviderStats(nil)
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

	providerRoutingStatsFilename  = "provider-routing-stats.json"
	providerRoutingEventsFilename = "provider-routing-events.jsonl"
	maxRecentRoutingEvents        = 200

	providerEventSuccess         = "success"
	providerEventError           = "error"
	providerEventSkippedDisabled = "skipped_disabled"
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

func (c *LLMClient) configureProviderRoutingStats(workspace string) {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		return
	}
	memoryDir := filepath.Join(ws, "memory")
	statsPath := filepath.Join(memoryDir, providerRoutingStatsFilename)
	eventsPath := filepath.Join(memoryDir, providerRoutingEventsFilename)

	c.statsMu.Lock()
	c.statsPath = statsPath
	c.eventsPath = eventsPath
	c.stats.StatsFile = statsPath
	c.stats.EventsFile = eventsPath
	c.statsMu.Unlock()
}

func providerName(provider llmProvider) string {
	name := strings.TrimSpace(provider.name)
	if name == "" {
		return "default"
	}
	return name
}

func (c *LLMClient) setFailoverOrder(providers []llmProvider) {
	order := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		name := providerName(provider)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		order = append(order, name)
	}

	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.ensureStatsMapsLocked()
	c.stats.FailoverOrder = order
	for _, name := range order {
		if _, ok := c.stats.Providers[name]; !ok {
			c.stats.Providers[name] = ProviderRoutingProviderStats{}
		}
	}
}

func (c *LLMClient) ensureStatsMapsLocked() {
	if c.stats.Providers == nil {
		c.stats.Providers = make(map[string]ProviderRoutingProviderStats)
	}
	if c.stats.Since == "" {
		c.stats.Since = time.Now().UTC().Format(time.RFC3339)
	}
	if c.stats.UpdatedAt == "" {
		c.stats.UpdatedAt = c.stats.Since
	}
	if c.stats.Model == "" {
		c.stats.Model = strings.TrimSpace(c.model)
	}
}

func (c *LLMClient) recordRequestStart(requestID int64, providers []llmProvider) {
	c.setFailoverOrder(providers)

	now := time.Now().UTC().Format(time.RFC3339)
	c.statsMu.Lock()
	c.ensureStatsMapsLocked()
	c.stats.RequestsTotal++
	c.stats.LastRequestID = requestID
	c.stats.UpdatedAt = now
	c.statsMu.Unlock()

	c.persistProviderStats(nil)
}

func (c *LLMClient) recordRequestCompletion(requestID int64, success bool, provider string, errMsg string) {
	now := time.Now().UTC().Format(time.RFC3339)
	c.statsMu.Lock()
	c.ensureStatsMapsLocked()
	if success {
		c.stats.RequestsSucceeded++
	} else {
		c.stats.RequestsFailed++
	}
	c.stats.LastRequest = &ProviderRoutingLastRequest{
		RequestID:   requestID,
		CompletedAt: now,
	}
	if success {
		c.stats.LastRequest.Status = providerEventSuccess
		c.stats.LastRequest.Provider = provider
	} else {
		c.stats.LastRequest.Status = "failed"
		c.stats.LastRequest.Error = errMsg
	}
	c.stats.UpdatedAt = now
	c.statsMu.Unlock()

	c.persistProviderStats(nil)
}

func providerHTTPStatus(err error) int {
	var apiErr *llmAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

func (c *LLMClient) recordProviderEvent(event ProviderRoutingEvent) {
	event.Timestamp = strings.TrimSpace(event.Timestamp)
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	event.Provider = providerName(llmProvider{name: event.Provider})

	c.statsMu.Lock()
	c.ensureStatsMapsLocked()
	pstats := c.stats.Providers[event.Provider]
	switch event.Outcome {
	case providerEventSuccess:
		c.stats.ProviderAttemptsTotal++
		pstats.Attempts++
		pstats.Successes++
		pstats.PromptTokens += int64(event.PromptTokens)
		pstats.CompletionTokens += int64(event.CompletionTokens)
		pstats.TotalTokens += int64(event.TotalTokens)
		pstats.LastUsedAt = event.Timestamp
		pstats.LastRequestID = event.RequestID
		pstats.LastError = ""
	case providerEventError:
		c.stats.ProviderAttemptsTotal++
		pstats.Attempts++
		pstats.Failures++
		pstats.LastUsedAt = event.Timestamp
		pstats.LastRequestID = event.RequestID
		pstats.LastError = event.Error
	case providerEventSkippedDisabled:
		pstats.SkippedDisabled++
	}
	c.stats.Providers[event.Provider] = pstats

	c.recentEvents = append(c.recentEvents, event)
	if len(c.recentEvents) > maxRecentRoutingEvents {
		c.recentEvents = append([]ProviderRoutingEvent(nil), c.recentEvents[len(c.recentEvents)-maxRecentRoutingEvents:]...)
	}
	c.stats.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	c.statsMu.Unlock()

	c.persistProviderStats(&event)
}

func cloneProviderStats(in map[string]ProviderRoutingProviderStats) map[string]ProviderRoutingProviderStats {
	if len(in) == 0 {
		return map[string]ProviderRoutingProviderStats{}
	}
	out := make(map[string]ProviderRoutingProviderStats, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (c *LLMClient) providerRoutingReport(limit int) ProviderRoutingReport {
	c.statsMu.RLock()
	defer c.statsMu.RUnlock()

	summary := c.stats
	summary.FailoverOrder = append([]string(nil), c.stats.FailoverOrder...)
	summary.Providers = cloneProviderStats(c.stats.Providers)
	if c.stats.LastRequest != nil {
		last := *c.stats.LastRequest
		summary.LastRequest = &last
	}

	events := append([]ProviderRoutingEvent(nil), c.recentEvents...)
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}

	return ProviderRoutingReport{
		Summary:      summary,
		RecentEvents: events,
	}
}

func (c *LLMClient) persistProviderStats(event *ProviderRoutingEvent) {
	c.statsMu.RLock()
	statsPath := c.statsPath
	eventsPath := c.eventsPath
	c.statsMu.RUnlock()

	if statsPath == "" && (event == nil || eventsPath == "") {
		return
	}
	if statsPath != "" {
		if err := ensureDir(filepath.Dir(statsPath)); err != nil {
			slog.Warn("Failed to create provider stats directory", "err", err)
		} else {
			report := c.providerRoutingReport(0)
			data, err := json.MarshalIndent(report.Summary, "", "  ")
			if err != nil {
				slog.Warn("Failed to marshal provider stats", "err", err)
			} else if err := os.WriteFile(statsPath, append(data, '\n'), 0644); err != nil {
				slog.Warn("Failed to persist provider stats", "path", statsPath, "err", err)
			}
		}
	}
	if event != nil && eventsPath != "" {
		if err := ensureDir(filepath.Dir(eventsPath)); err != nil {
			slog.Warn("Failed to create provider event log directory", "err", err)
			return
		}
		f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			slog.Warn("Failed to open provider event log", "path", eventsPath, "err", err)
			return
		}
		enc := json.NewEncoder(f)
		if err := enc.Encode(event); err != nil {
			slog.Warn("Failed to append provider event", "path", eventsPath, "err", err)
		}
		if err := f.Close(); err != nil {
			slog.Warn("Failed to close provider event log", "path", eventsPath, "err", err)
		}
	}
}

func (c *LLMClient) chat(ctx context.Context, messages []ChatMessage, tools []ToolDef, maxTokens int, temperature float64, reasoningEffort string) (*LLMResponse, error) {
	providers := c.providerChain()
	requestID := c.requestSeq.Add(1)
	c.recordRequestStart(requestID, providers)

	if len(providers) == 0 {
		err := fmt.Errorf("no configured LLM providers")
		c.recordRequestCompletion(requestID, false, "", err.Error())
		return nil, err
	}
	var lastErr error
	attempted := false
	for idx, provider := range providers {
		attemptNum := idx + 1
		if disabled, reason := c.providerDisabled(provider); disabled {
			slog.Info("Skipping disabled LLM provider", "provider", provider.name, "reason", reason)
			c.recordProviderEvent(ProviderRoutingEvent{
				RequestID:     requestID,
				Attempt:       attemptNum,
				Provider:      provider.name,
				APIBase:       provider.apiBase,
				ProviderModel: provider.model,
				RequestModel:  c.requestModelFor(provider),
				Outcome:       providerEventSkippedDisabled,
				Error:         reason,
			})
			continue
		}
		attempted = true
		started := time.Now()
		resp, err := c.chatWithProvider(ctx, provider, messages, tools, maxTokens, temperature, reasoningEffort)
		if err == nil {
			c.recordProviderEvent(ProviderRoutingEvent{
				RequestID:        requestID,
				Attempt:          attemptNum,
				Provider:         provider.name,
				APIBase:          provider.apiBase,
				ProviderModel:    provider.model,
				RequestModel:     c.requestModelFor(provider),
				Outcome:          providerEventSuccess,
				DurationMS:       time.Since(started).Milliseconds(),
				FinishReason:     resp.FinishReason,
				PromptTokens:     resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
				TotalTokens:      resp.Usage.TotalTokens,
			})
			c.recordRequestCompletion(requestID, true, providerName(provider), "")
			return resp, nil
		}
		c.recordProviderEvent(ProviderRoutingEvent{
			RequestID:     requestID,
			Attempt:       attemptNum,
			Provider:      provider.name,
			APIBase:       provider.apiBase,
			ProviderModel: provider.model,
			RequestModel:  c.requestModelFor(provider),
			Outcome:       providerEventError,
			HTTPStatus:    providerHTTPStatus(err),
			DurationMS:    time.Since(started).Milliseconds(),
			Error:         err.Error(),
		})
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			c.recordRequestCompletion(requestID, false, "", err.Error())
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
		err := fmt.Errorf("no available LLM providers (all disabled)")
		c.recordRequestCompletion(requestID, false, "", err.Error())
		return nil, err
	}
	err := fmt.Errorf("all configured LLM providers failed: %w", lastErr)
	c.recordRequestCompletion(requestID, false, "", err.Error())
	return nil, err
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
