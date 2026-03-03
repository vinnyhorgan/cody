package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
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

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
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
}

func newLLMClient(apiKey, apiBase, model string) *LLMClient {
	return &LLMClient{
		apiKey:  apiKey,
		apiBase: apiBase,
		model:   model,
		client:  &http.Client{Timeout: 5 * time.Minute},
	}
}

func (c *LLMClient) requestModel() string {
	model := strings.TrimSpace(c.model)
	if model == "" {
		return model
	}
	// Accept nanobot/litellm-style "openrouter/<model>" when calling OpenRouter
	// directly through its OpenAI-compatible API.
	isOpenRouter := strings.Contains(strings.ToLower(c.apiBase), "openrouter") || strings.HasPrefix(c.apiKey, "sk-or-")
	if isOpenRouter && strings.HasPrefix(model, "openrouter/") {
		return strings.TrimPrefix(model, "openrouter/")
	}
	return model
}

func (c *LLMClient) chat(ctx context.Context, messages []ChatMessage, tools []ToolDef, maxTokens int, temperature float64, reasoningEffort string) (*LLMResponse, error) {
	req := chatRequest{
		Model:    c.requestModel(),
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

	url := c.apiBase + "/chat/completions"
	for attempt := range 3 {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.client.Do(httpReq)
		if err != nil {
			if attempt < 2 {
				slog.Warn("LLM request failed, retrying", "attempt", attempt+1, "err", err)
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
				slog.Warn("LLM response read failed, retrying", "attempt", attempt+1, "err", readErr)
				if !sleepWithContext(ctx, time.Duration(1<<uint(attempt))*time.Second) {
					return nil, fmt.Errorf("LLM request canceled: %w", ctx.Err())
				}
				continue
			}
			return nil, fmt.Errorf("read response body: %w", readErr)
		}

		if resp.StatusCode >= 500 && attempt < 2 {
			slog.Warn("LLM server error, retrying", "status", resp.StatusCode, "attempt", attempt+1)
			if !sleepWithContext(ctx, time.Duration(1<<uint(attempt))*time.Second) {
				return nil, fmt.Errorf("LLM request canceled: %w", ctx.Err())
			}
			continue
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("LLM API error %d: %s", resp.StatusCode, string(respBody))
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
