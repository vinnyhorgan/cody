package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// --- Message types and bus ---

type Media struct {
	Type     string `json:"type"` // "image", "audio", "document"
	URL      string `json:"url,omitempty"`
	Data     []byte `json:"data,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

type InboundMessage struct {
	SenderID   string
	ChatID     string
	Content    string
	Timestamp  time.Time
	Media      []Media
	SessionKey string
	MessageID  int // Telegram message ID for reply-to tracking
}

type OutboundMessage struct {
	ChatID           string
	Content          string
	ReplyTo          string
	Media            []Media
	ReplyToMessageID int  // Telegram message ID to reply to
	IsProgress       bool // true = progress/tool hint update, not a final response
	IsToolHint       bool // true = this is a tool hint specifically
}

type MessageBus struct {
	Inbound  chan *InboundMessage
	Outbound chan *OutboundMessage
}

func newMessageBus() *MessageBus {
	return &MessageBus{
		Inbound:  make(chan *InboundMessage, 64),
		Outbound: make(chan *OutboundMessage, 64),
	}
}

// --- Session ---

type SessionMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	Ts         string     `json:"ts,omitempty"`
}

func (m *SessionMessage) toChatMessage() ChatMessage {
	return ChatMessage{
		Role:       m.Role,
		Content:    m.Content,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
}

type Session struct {
	Key              string
	Messages         []SessionMessage
	CreatedAt        time.Time
	UpdatedAt        time.Time
	LastConsolidated int
	mu               sync.Mutex
}

func (s *Session) addMessage(role string, content any, toolCalls []ToolCall, toolCallID, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, SessionMessage{
		Role:       role,
		Content:    content,
		ToolCalls:  toolCalls,
		ToolCallID: toolCallID,
		Name:       name,
		Ts:         timestamp(),
	})
	s.UpdatedAt = time.Now()
}

func (s *Session) getHistory(maxMessages int) []ChatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.Messages[s.LastConsolidated:]

	// Slice to max first, then align (matching nanobot order)
	if maxMessages > 0 && len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}

	// Drop leading non-user messages to avoid orphaned tool_result blocks
	for i, m := range msgs {
		if m.Role == "user" {
			msgs = msgs[i:]
			break
		}
	}

	out := make([]ChatMessage, len(msgs))
	for i, m := range msgs {
		out[i] = m.toChatMessage()
	}
	return out
}

func (s *Session) unconsolidatedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Messages) - s.LastConsolidated
}

// clear resets the session messages and consolidation pointer.
func (s *Session) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = nil
	s.LastConsolidated = 0
	s.UpdatedAt = time.Now()
}

// --- Session Manager ---

type SessionManager struct {
	workspace string
	cache     map[string]*Session
	mu        sync.RWMutex
}

func newSessionManager(workspace string) *SessionManager {
	dir := filepath.Join(workspace, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		slog.Warn("Failed to create sessions directory", "err", err)
	}
	return &SessionManager{
		workspace: workspace,
		cache:     make(map[string]*Session),
	}
}

func (m *SessionManager) getOrCreate(key string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.cache[key]; ok {
		return s
	}

	s := m.load(key)
	if s == nil {
		s = &Session{Key: key, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	}
	m.cache[key] = s
	return s
}

func (m *SessionManager) save(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := filepath.Join(m.workspace, "sessions")
	path := filepath.Join(dir, safeFilename(s.Key)+".jsonl")
	tmpPath := path + ".tmp"

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create session file: %w", err)
	}

	meta := map[string]any{
		"_meta":             true,
		"key":               s.Key,
		"created_at":        s.CreatedAt.Format(time.RFC3339),
		"updated_at":        s.UpdatedAt.Format(time.RFC3339),
		"last_consolidated": s.LastConsolidated,
	}
	metaLine, err := json.Marshal(meta)
	if err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("marshal session metadata: %w", err)
	}
	if _, err := f.Write(metaLine); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write session metadata: %w", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write session metadata newline: %w", err)
	}

	enc := json.NewEncoder(f)
	for _, msg := range s.Messages {
		if err := enc.Encode(msg); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("encode session message: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close session file: %w", err)
	}

	// Atomic rename for crash safety.
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename session file: %w", err)
	}
	return nil
}

func (m *SessionManager) load(key string) *Session {
	dir := filepath.Join(m.workspace, "sessions")
	path := filepath.Join(dir, safeFilename(key)+".jsonl")

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	s := &Session{Key: key, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw map[string]any
		if json.Unmarshal(line, &raw) != nil {
			slog.Warn("Skipping malformed JSONL line in session", "key", key)
			continue
		}
		if _, ok := raw["_meta"]; ok {
			if v, ok := raw["last_consolidated"].(float64); ok {
				s.LastConsolidated = int(v)
			}
			if v, ok := raw["created_at"].(string); ok {
				if parsed, err := time.Parse(time.RFC3339, v); err == nil {
					s.CreatedAt = parsed
				}
			}
			if v, ok := raw["updated_at"].(string); ok {
				if parsed, err := time.Parse(time.RFC3339, v); err == nil && !parsed.IsZero() {
					s.UpdatedAt = parsed
				}
			}
			continue
		}
		var msg SessionMessage
		if json.Unmarshal(line, &msg) == nil {
			s.Messages = append(s.Messages, msg)
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("Failed reading session file", "key", key, "err", err)
	}

	if len(s.Messages) == 0 {
		return nil
	}
	return s
}

// mostRecentTelegramChatID returns the chat_id from the most recently updated
// Telegram session, checking both in-memory cache and persisted session files.
func (m *SessionManager) mostRecentTelegramChatID() string {
	var bestKey string
	var bestUpdated time.Time

	m.mu.RLock()
	for key, s := range m.cache {
		if !strings.HasPrefix(key, "telegram:") {
			continue
		}
		if bestKey == "" || s.UpdatedAt.After(bestUpdated) {
			bestKey = key
			bestUpdated = s.UpdatedAt
		}
	}
	m.mu.RUnlock()

	dir := filepath.Join(m.workspace, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if bestKey == "" {
			return ""
		}
		return strings.TrimPrefix(bestKey, "telegram:")
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		if !scanner.Scan() {
			f.Close()
			continue
		}
		line := scanner.Bytes()
		if err := scanner.Err(); err != nil {
			f.Close()
			continue
		}
		f.Close()

		var meta map[string]any
		if json.Unmarshal(line, &meta) != nil || meta["_meta"] == nil {
			continue
		}

		key, _ := meta["key"].(string)
		if !strings.HasPrefix(key, "telegram:") {
			continue
		}

		updated := time.Time{}
		if raw, ok := meta["updated_at"].(string); ok {
			updated, _ = time.Parse(time.RFC3339, raw)
		}
		if updated.IsZero() {
			if raw, ok := meta["created_at"].(string); ok {
				updated, _ = time.Parse(time.RFC3339, raw)
			}
		}
		if updated.IsZero() {
			if info, err := os.Stat(path); err == nil {
				updated = info.ModTime()
			}
		}
		if bestKey == "" || updated.After(bestUpdated) {
			bestKey = key
			bestUpdated = updated
		}
	}

	if bestKey == "" {
		return ""
	}
	return strings.TrimPrefix(bestKey, "telegram:")
}
