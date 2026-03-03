package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// mockTelegramServer creates a mock Telegram Bot API server.
func mockTelegramServer(t *testing.T) (*httptest.Server, *tgbotapi.BotAPI) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/file/bot") && strings.HasSuffix(path, "/photos/file.jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10})
		case strings.HasSuffix(path, "/getMe"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id":         123,
					"is_bot":     true,
					"first_name": "TestBot",
					"username":   "test_bot",
				},
			})
		case strings.HasSuffix(path, "/getFile"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"file_id": "test", "file_path": "photos/file.jpg"},
			})
		case strings.HasSuffix(path, "/sendMessage"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id": 1,
					"chat":       map[string]any{"id": 123},
					"date":       time.Now().Unix(),
					"text":       "ok",
				},
			})
		case strings.HasSuffix(path, "/sendChatAction"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))

	bot, err := tgbotapi.NewBotAPIWithClient("test-token", srv.URL+"/bot%s/%s", srv.Client())
	if err != nil {
		t.Fatalf("failed to create mock bot: %v", err)
	}
	return srv, bot
}

func TestMarkdownToTelegramHTML(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "bold",
			input: "**bold text**",
			want:  "<b>bold text</b>",
		},
		{
			name:  "inline code",
			input: "use `fmt.Println`",
			want:  "use <code>fmt.Println</code>",
		},
		{
			name:  "code block",
			input: "```go\nfunc main() {}\n```",
			want:  "<pre><code>func main() {}</code></pre>",
		},
		{
			name:  "link",
			input: "[Go](https://golang.org)",
			want:  `<a href="https://golang.org">Go</a>`,
		},
		{
			name:  "strikethrough",
			input: "~~deleted~~",
			want:  "<s>deleted</s>",
		},
		{
			name:  "html escaping",
			input: "x < y && y > z",
			want:  "x &lt; y &amp;&amp; y &gt; z",
		},
		{
			name:  "code block preserves content",
			input: "```\n<div>HTML</div>\n```",
			want:  "<pre><code>&lt;div&gt;HTML&lt;/div&gt;</code></pre>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := markdownToTelegramHTML(tt.input)
			if !strings.Contains(got, tt.want) {
				t.Errorf("markdownToTelegramHTML(%q)\n  got:  %q\n  want contains: %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMarkdownToTelegramHTMLTable(t *testing.T) {
	input := "| Name | Role |\n| ---- | ---- |\n| Ana | Dev |\n| Bob | SRE |"
	got := markdownToTelegramHTML(input)

	want := "• <b>Ana</b>\nDev\n\n• <b>Bob</b>\nSRE"
	if !strings.Contains(got, want) {
		t.Fatalf("table conversion mismatch\n  got:  %q\n  want contains: %q", got, want)
	}
	if strings.Contains(got, "<pre><code>") {
		t.Fatalf("expected table to render as readable text block, got: %q", got)
	}
}

func TestMarkdownToTelegramHTMLTableWithoutOuterPipes(t *testing.T) {
	input := "Name | Score\n:--- | ---:\nAlice | 10"
	got := markdownToTelegramHTML(input)

	if strings.Contains(got, "<pre><code>") {
		t.Fatalf("expected table to render as readable text block, got: %q", got)
	}
	if strings.Contains(got, "| ---") || strings.Contains(got, ":---") {
		t.Fatalf("expected markdown table separators to be removed, got: %q", got)
	}
	if !strings.Contains(got, "• <b>Alice</b>\n10") {
		t.Fatalf("expected row content to be present, got: %q", got)
	}
}

func TestMarkdownToTelegramHTMLTableCellInlineMarkdown(t *testing.T) {
	input := "| # | Idea | Notes |\n| --- | --- | --- |\n| **1** | **Personal Portfolio + Blog** | [Guide](https://example.com) + ~~old~~ |"
	got := markdownToTelegramHTML(input)

	if strings.Contains(got, "**1**") || strings.Contains(got, "**Personal Portfolio + Blog**") {
		t.Fatalf("expected markdown markers inside table cells to be rendered, got: %q", got)
	}
	if !strings.Contains(got, "<b>#:</b> <b>1</b>") {
		t.Fatalf("expected bold table value to be rendered, got: %q", got)
	}
	if !strings.Contains(got, "<b>Idea:</b> <b>Personal Portfolio + Blog</b>") {
		t.Fatalf("expected bold cell content to be rendered, got: %q", got)
	}
	if !strings.Contains(got, `<a href="https://example.com">Guide</a>`) {
		t.Fatalf("expected links inside table cells to be rendered, got: %q", got)
	}
	if !strings.Contains(got, "<s>old</s>") {
		t.Fatalf("expected strikethrough inside table cells to be rendered, got: %q", got)
	}
}

func TestMarkdownToTelegramHTMLTableHeaderMarkdownMarkers(t *testing.T) {
	input := "| Criterion | **SQLite** | **PostgreSQL** |\n| --- | --- | --- |\n| Setup effort | `brew install sqlite3` | `brew install postgresql` |"
	got := markdownToTelegramHTML(input)

	if strings.Contains(got, "**SQLite**") || strings.Contains(got, "**PostgreSQL**") {
		t.Fatalf("expected markdown markers in header labels to be stripped, got: %q", got)
	}
	if !strings.Contains(got, "<b>SQLite:</b>") || !strings.Contains(got, "<b>PostgreSQL:</b>") {
		t.Fatalf("expected clean bold header labels, got: %q", got)
	}
}

func TestNormalizeMarkdownTablesForTelegram(t *testing.T) {
	input := "```\n| keep | this |\n| ---- | ---- |\n| as | code |\n```\n\n| Name | Role |\n| ---- | ---- |\n| Ana | **Dev** |\n| Bob | [SRE](https://example.com) |"
	got := normalizeMarkdownTablesForTelegram(input)

	if strings.Count(got, "| ---- | ---- |") != 1 {
		t.Fatalf("expected only the fenced code block separator to remain after normalization, got: %q", got)
	}
	if strings.Contains(got, "| Name | Role |") {
		t.Fatalf("expected markdown table header to be normalized away, got: %q", got)
	}
	if !strings.Contains(got, "- **Ana**\n**Dev**") {
		t.Fatalf("expected normalized bullet block for 2-column table, got: %q", got)
	}
	if !strings.Contains(got, "```\n| keep | this |") {
		t.Fatalf("expected fenced code block table syntax to remain untouched, got: %q", got)
	}

	html := markdownToTelegramHTML(got)
	if !strings.Contains(html, "• <b>Ana</b>") {
		t.Fatalf("expected converted html bullets after normalization, got: %q", html)
	}
	if !strings.Contains(html, `<a href="https://example.com">SRE</a>`) {
		t.Fatalf("expected markdown links in normalized table details to render, got: %q", html)
	}
}

func TestSplitMessageForTelegramRespectsRenderedLength(t *testing.T) {
	input := strings.Repeat("**bold** ", 700) + "\n\n" + strings.Repeat("`code` ", 500)
	chunks := splitMessageForTelegram(input, 500)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for long rendered content, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		rendered := markdownToTelegramHTML(chunk)
		if len(rendered) > 500 {
			t.Fatalf("chunk %d rendered length=%d exceeds budget", i, len(rendered))
		}
	}
}

func TestSplitMessageForTelegramLongTableKeepsReadableChunks(t *testing.T) {
	var b strings.Builder
	b.WriteString("| Name | Role |\n| ---- | ---- |\n")
	for i := 0; i < 350; i++ {
		fmt.Fprintf(&b, "| User %d | Engineer |\n", i)
	}

	normalized := normalizeMarkdownTablesForTelegram(b.String())
	if strings.Contains(normalized, "| ---- | ---- |") {
		t.Fatalf("expected normalized long table to drop markdown separators")
	}

	chunks := splitMessageForTelegram(normalized, 500)
	for i, chunk := range chunks {
		html := markdownToTelegramHTML(chunk)
		if len(html) > 500 {
			t.Fatalf("chunk %d rendered length=%d exceeds budget", i, len(html))
		}
		if strings.Contains(html, "| ---- | ---- |") {
			t.Fatalf("chunk %d should not contain markdown table separator after conversion: %q", i, html)
		}
	}
}

func TestSplitMessageForTelegramBalancesFencedCodeBlocks(t *testing.T) {
	var b strings.Builder
	b.WriteString("Setup notes\n\n```bash\n")
	for i := 0; i < 220; i++ {
		fmt.Fprintf(&b, "echo line-%03d\n", i)
	}
	b.WriteString("```\n\nThen continue with more details.\n")

	chunks := splitMessageForTelegram(b.String(), 500)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	for i, chunk := range chunks {
		if strings.Count(chunk, "```")%2 != 0 {
			t.Fatalf("chunk %d has unbalanced fenced code block markers: %q", i, chunk)
		}
		rendered := markdownToTelegramHTML(chunk)
		if strings.Contains(rendered, "```") {
			t.Fatalf("chunk %d leaked raw fence markers after rendering: %q", i, rendered)
		}
		if len(rendered) > 500 {
			t.Fatalf("chunk %d rendered length=%d exceeds budget", i, len(rendered))
		}
	}
}

func TestMarkdownToTelegramHTMLCodeBlockWithTableSyntax(t *testing.T) {
	input := "```\n| Name | Role |\n| ---- | ---- |\n| Ana | Dev |\n```"
	got := markdownToTelegramHTML(input)
	want := "<pre><code>| Name | Role |\n| ---- | ---- |\n| Ana | Dev |</code></pre>"
	if !strings.Contains(got, want) {
		t.Fatalf("expected fenced code block content to remain unchanged\n  got:  %q\n  want contains: %q", got, want)
	}
}

func TestMarkdownToTelegramHTMLCodeBlockLanguageWithSymbols(t *testing.T) {
	input := "```c++\nint x;\n```"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, "<pre><code>int x;</code></pre>") {
		t.Fatalf("expected fenced code block body without language marker noise, got: %q", got)
	}
	if strings.Contains(got, "++\nint x;") {
		t.Fatalf("language marker should not leak into code body, got: %q", got)
	}
}

func TestMarkdownToTelegramHTMLUnwrapTopLevelMarkdownFence(t *testing.T) {
	input := "```md\n# Render Test\n- **Bold**\n- `code`\n```"
	got := markdownToTelegramHTML(input)
	if strings.Contains(got, "<pre><code>") {
		t.Fatalf("expected top-level md fence to be unwrapped, got: %q", got)
	}
	if !strings.Contains(got, "Render Test") {
		t.Fatalf("expected heading text to be preserved, got: %q", got)
	}
	if !strings.Contains(got, "• <b>Bold</b>") {
		t.Fatalf("expected markdown bullet+bolder after unwrap, got: %q", got)
	}
	if !strings.Contains(got, "<code>code</code>") {
		t.Fatalf("expected inline code after unwrap, got: %q", got)
	}
}

func TestMarkdownToTelegramHTMLDoesNotUnwrapNonMarkdownFence(t *testing.T) {
	input := "```go\nfmt.Println(\"hi\")\n```"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, "<pre><code>fmt.Println(\"hi\")</code></pre>") {
		t.Fatalf("expected non-markdown fence to remain a code block, got: %q", got)
	}
}

func TestEscapeHTML(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"<b>bold</b>", "&lt;b&gt;bold&lt;/b&gt;"},
		{"a & b", "a &amp; b"},
		{"a&b<c>d", "a&amp;b&lt;c&gt;d"},
	}
	for _, tt := range tests {
		got := escapeHTML(tt.input)
		if got != tt.want {
			t.Errorf("escapeHTML(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSplitMessage(t *testing.T) {
	// Short message
	chunks := splitMessage("hello", 4000)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("short message: got %v", chunks)
	}

	// Long message
	long := strings.Repeat("a", 5000)
	chunks = splitMessage(long, 4000)
	if len(chunks) < 2 {
		t.Error("expected at least 2 chunks for 5000 char message")
	}
	// Verify all content is preserved
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != 5000 {
		t.Errorf("total chars = %d, want 5000", total)
	}

	// Split at newlines
	text := strings.Repeat("line\n", 1000)
	chunks = splitMessage(text, 100)
	for _, chunk := range chunks {
		if len(chunk) > 100 {
			t.Errorf("chunk too long: %d chars", len(chunk))
		}
	}
}

func TestSplitMessageEmpty(t *testing.T) {
	chunks := splitMessage("", 4000)
	if len(chunks) != 1 || chunks[0] != "" {
		t.Errorf("empty message: got %v", chunks)
	}
}

func TestSplitMessageTrimsLeadingWhitespaceAfterSplit(t *testing.T) {
	text := "hello world again"
	chunks := splitMessage(text, 11)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d (%v)", len(chunks), chunks)
	}
	if strings.HasPrefix(chunks[1], " ") {
		t.Fatalf("second chunk should not start with whitespace: %q", chunks[1])
	}
}

func TestSplitMessagePreservesUTF8Runes(t *testing.T) {
	input := strings.Repeat("😀", 20)
	chunks := splitMessage(input, 5) // 5 bytes can split inside an emoji if not guarded
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d is invalid UTF-8: %q", i, c)
		}
	}
	if strings.Join(chunks, "") != input {
		t.Fatalf("recombined chunks differ from original input")
	}
}

func TestTelegramSendRepliesOnEveryChunk(t *testing.T) {
	var (
		mu       sync.Mutex
		replyIDs []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id":         123,
					"is_bot":     true,
					"first_name": "TestBot",
					"username":   "test_bot",
				},
			})
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_ = r.ParseForm()
			mu.Lock()
			replyIDs = append(replyIDs, r.Form.Get("reply_to_message_id"))
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id": 1,
					"chat":       map[string]any{"id": 123},
					"date":       time.Now().Unix(),
					"text":       "ok",
				},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))
	defer srv.Close()

	bot, err := tgbotapi.NewBotAPIWithClient("test-token", srv.URL+"/bot%s/%s", srv.Client())
	if err != nil {
		t.Fatalf("failed to create mock bot: %v", err)
	}

	cfg := &Config{}
	cfg.Telegram.ReplyToMessage = true
	tb := &TelegramBot{config: cfg, bus: newMessageBus(), bot: bot}

	tb.send(&OutboundMessage{
		ChatID:           "123",
		Content:          strings.Repeat("word ", 1200), // forces chunking
		ReplyToMessageID: 42,
	})

	mu.Lock()
	ids := append([]string(nil), replyIDs...)
	mu.Unlock()

	if len(ids) < 2 {
		t.Fatalf("expected multiple chunks/messages, got %d", len(ids))
	}
	for i, id := range ids {
		if id != "42" {
			t.Fatalf("chunk %d reply_to_message_id = %q, want 42", i, id)
		}
	}
}

func TestParseChatID(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"12345", 12345},
		{"-100123", -100123},
		{"", 0},
		{"notanumber", 0},
	}
	for _, tt := range tests {
		got := parseChatID(tt.input)
		if got != tt.want {
			t.Errorf("parseChatID(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestFormatTelegramInboundMarkdownEntities(t *testing.T) {
	text := "bold italic strike code link"
	entities := []tgbotapi.MessageEntity{
		{Type: "bold", Offset: 0, Length: 4},
		{Type: "italic", Offset: 5, Length: 6},
		{Type: "strikethrough", Offset: 12, Length: 6},
		{Type: "code", Offset: 19, Length: 4},
		{Type: "text_link", Offset: 24, Length: 4, URL: "https://example.com/a_(b)_c?x=1&y=2"},
	}

	got := formatTelegramInboundMarkdown(text, entities)
	want := "**bold** _italic_ ~~strike~~ `code` [link](https://example.com/a_(b)_c?x=1&y=2)"
	if got != want {
		t.Fatalf("inbound markdown rebuild mismatch\n  got:  %q\n  want: %q", got, want)
	}
}

func TestFormatTelegramInboundMarkdownNestedEntities(t *testing.T) {
	text := "bold italic"
	entities := []tgbotapi.MessageEntity{
		{Type: "bold", Offset: 0, Length: 11},
		{Type: "italic", Offset: 5, Length: 6},
	}

	got := formatTelegramInboundMarkdown(text, entities)
	want := "**bold _italic_**"
	if got != want {
		t.Fatalf("nested entity rebuild mismatch\n  got:  %q\n  want: %q", got, want)
	}
}

func TestFormatTelegramInboundMarkdownUTF16Offsets(t *testing.T) {
	text := "🙂bold"
	entities := []tgbotapi.MessageEntity{
		// Emoji is two UTF-16 code units; bold starts after it.
		{Type: "bold", Offset: 2, Length: 4},
	}

	got := formatTelegramInboundMarkdown(text, entities)
	want := "🙂**bold**"
	if got != want {
		t.Fatalf("utf16 entity offsets should map correctly\n  got:  %q\n  want: %q", got, want)
	}
}

func TestHandlePhotoCaptionEntities(t *testing.T) {
	tb := &TelegramBot{config: &Config{}, bus: newMessageBus()}
	msg := &tgbotapi.Message{
		Caption: "bold cap",
		CaptionEntities: []tgbotapi.MessageEntity{
			{Type: "bold", Offset: 0, Length: 4},
		},
		Photo: []tgbotapi.PhotoSize{},
	}

	content, media := tb.handlePhoto(msg)
	if content != "**bold** cap" {
		t.Fatalf("caption entities should be rebuilt to markdown, got: %q", content)
	}
	if media != nil {
		t.Fatalf("expected nil media for empty photo list, got: %#v", media)
	}
}

func TestMarkdownToTelegramHTMLPreservesCodeInBlocks(t *testing.T) {
	input := "text **bold** and ```\n**not bold**\n```"
	result := markdownToTelegramHTML(input)

	// The bold outside code block should be converted
	if !strings.Contains(result, "<b>bold</b>") {
		t.Error("bold outside code block should be converted")
	}

	// The ** inside code block should NOT be converted to bold
	if strings.Contains(result, "<b>not bold</b>") {
		t.Error("bold inside code block should NOT be converted")
	}
}

func TestFormatToolHint(t *testing.T) {
	tc := ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "read_file",
			Arguments: `{"path": "test.txt"}`,
		},
	}
	hint := formatToolHint(tc)
	if !strings.Contains(hint, "read_file") {
		t.Error("hint should contain tool name")
	}
	if !strings.Contains(hint, "test.txt") {
		t.Error("hint should contain first arg value")
	}
}

func TestFormatToolHintTruncation(t *testing.T) {
	longVal := strings.Repeat("x", 60)
	tc := ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "write_file",
			Arguments: fmt.Sprintf(`{"path": "%s"}`, longVal),
		},
	}
	hint := formatToolHint(tc)
	if !strings.Contains(hint, "…") {
		t.Error("truncated hint should contain ellipsis")
	}
}

func TestOutboundMessageProgressFields(t *testing.T) {
	msg := &OutboundMessage{
		ChatID:     "12345",
		Content:    "thinking...",
		IsProgress: true,
		IsToolHint: false,
	}
	if !msg.IsProgress {
		t.Error("IsProgress should be true")
	}
	if msg.IsToolHint {
		t.Error("IsToolHint should be false")
	}

	toolMsg := &OutboundMessage{
		ChatID:     "12345",
		Content:    "🔧 read_file",
		IsProgress: true,
		IsToolHint: true,
	}
	if !toolMsg.IsToolHint {
		t.Error("IsToolHint should be true")
	}
}

func TestInboundMessageID(t *testing.T) {
	msg := &InboundMessage{
		SenderID:   "user1",
		ChatID:     "12345",
		Content:    "hello",
		MessageID:  42,
		SessionKey: "test",
	}
	if msg.MessageID != 42 {
		t.Errorf("MessageID = %d, want 42", msg.MessageID)
	}
}

func TestOutboundReplyToMessageID(t *testing.T) {
	msg := &OutboundMessage{
		ChatID:           "12345",
		Content:          "response",
		ReplyToMessageID: 42,
	}
	if msg.ReplyToMessageID != 42 {
		t.Errorf("ReplyToMessageID = %d, want 42", msg.ReplyToMessageID)
	}
}

func TestMarkdownBulletLists(t *testing.T) {
	input := "- item one\n- item two\n* item three"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, "• item one") {
		t.Errorf("expected bullet conversion with -, got: %s", got)
	}
	if !strings.Contains(got, "• item three") {
		t.Errorf("expected bullet conversion with *, got: %s", got)
	}
}

func TestMarkdownBulletListsIndented(t *testing.T) {
	input := "  - item one\n\t* item two"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, "• item one") {
		t.Errorf("expected indented '-' bullet conversion, got: %s", got)
	}
	if !strings.Contains(got, "• item two") {
		t.Errorf("expected indented '*' bullet conversion, got: %s", got)
	}
}

func TestMarkdownBulletListsInsideCodeBlock(t *testing.T) {
	// Bullets inside code blocks should NOT be converted
	input := "```\n- not a bullet\n```"
	got := markdownToTelegramHTML(input)
	if strings.Contains(got, "•") {
		t.Errorf("bullets inside code blocks should not be converted, got: %s", got)
	}
}

// --- isAllowed ---

func TestIsAllowedEmpty(t *testing.T) {
	tb := &TelegramBot{config: &Config{}}
	if !tb.isAllowed("anyone") {
		t.Error("empty allow_from should allow all")
	}
}

func TestIsAllowedMatch(t *testing.T) {
	cfg := &Config{}
	cfg.Telegram.AllowFrom = []string{"123", "456"}
	tb := &TelegramBot{config: cfg}

	if !tb.isAllowed("123") {
		t.Error("should allow 123")
	}
	if !tb.isAllowed("456") {
		t.Error("should allow 456")
	}
	if tb.isAllowed("789") {
		t.Error("should reject 789")
	}
}

// --- dispatchOutbound filtering ---

func TestDispatchOutboundFiltering(t *testing.T) {
	cfg := &Config{}
	cfg.Telegram.SendProgress = false
	cfg.Telegram.SendToolHints = false

	bus := newMessageBus()
	tb := &TelegramBot{config: cfg, bus: bus}

	// Progress message should be filtered when SendProgress=false
	msg := &OutboundMessage{Content: "thinking...", IsProgress: true}
	shouldFilter := msg.IsProgress && (!msg.IsToolHint && !cfg.Telegram.SendProgress)
	if !shouldFilter {
		t.Error("progress should be filtered when SendProgress=false")
	}

	// Tool hint should be filtered when SendToolHints=false
	msg2 := &OutboundMessage{Content: "🔧 tool", IsProgress: true, IsToolHint: true}
	shouldFilter2 := msg2.IsProgress && (msg2.IsToolHint && !cfg.Telegram.SendToolHints)
	if !shouldFilter2 {
		t.Error("tool hint should be filtered when SendToolHints=false")
	}

	// Non-progress should NOT be filtered
	msg3 := &OutboundMessage{Content: "real msg", IsProgress: false}
	shouldFilter3 := msg3.IsProgress
	if shouldFilter3 {
		t.Error("non-progress should not be filtered")
	}

	_ = tb
}

// --- markdownToTelegramHTML edge cases ---

func TestMarkdownItalic(t *testing.T) {
	input := "This is *italic* text"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, "<i>italic</i>") {
		t.Errorf("expected italic HTML, got: %s", got)
	}
}

func TestMarkdownNestedFormatting(t *testing.T) {
	input := "**bold and `code` inside**"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, "<b>") {
		t.Errorf("expected bold tag, got: %s", got)
	}
}

func TestMarkdownLinks(t *testing.T) {
	input := "[click here](https://example.com)"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("expected link, got: %s", got)
	}
}

func TestMarkdownHTMLBreakTagsBecomeNewlines(t *testing.T) {
	input := "line one<br>line two<br/>line three<BR />line four"
	got := markdownToTelegramHTML(input)

	if strings.Contains(got, "&lt;br") || strings.Contains(strings.ToLower(got), "<br") {
		t.Fatalf("expected html break tags to be normalized away, got: %s", got)
	}
	if !strings.Contains(got, "line one\nline two\nline three\nline four") {
		t.Fatalf("expected newline conversion, got: %s", got)
	}
}

func TestMarkdownLinksWithParenthesesInURL(t *testing.T) {
	input := "[Parens](https://en.wikipedia.org/wiki/Function_(mathematics))"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, `<a href="https://en.wikipedia.org/wiki/Function_(mathematics)">Parens</a>`) {
		t.Fatalf("expected full parenthesized URL to remain clickable, got: %s", got)
	}
}

func TestMarkdownLinksWithAmpersandQuery(t *testing.T) {
	input := "[Query](https://example.com/search?q=a&b=c)"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, `<a href="https://example.com/search?q=a&amp;b=c">Query</a>`) {
		t.Fatalf("expected query URL link conversion, got: %s", got)
	}
}

func TestMarkdownAutoLinks(t *testing.T) {
	input := "Status page: <https://status.example.com/incidents/db_(primary)?env=prod&region=eu>"
	got := markdownToTelegramHTML(input)
	want := `<a href="https://status.example.com/incidents/db_(primary)?env=prod&amp;region=eu">https://status.example.com/incidents/db_(primary)?env=prod&amp;region=eu</a>`
	if !strings.Contains(got, want) {
		t.Fatalf("expected autolink conversion, got: %s", got)
	}
	if strings.Contains(got, "&lt;https://") {
		t.Fatalf("autolink should not be rendered as escaped literal, got: %s", got)
	}
}

func TestUnderscoreWordsAreNotItalicized(t *testing.T) {
	input := "some_var_name path_like_this/file_name.txt"
	got := markdownToTelegramHTML(input)
	if strings.Contains(got, "<i>") {
		t.Fatalf("underscores in words/paths should not become italic, got: %s", got)
	}
	if !strings.Contains(got, "some_var_name") || !strings.Contains(got, "path_like_this/file_name.txt") {
		t.Fatalf("expected underscore words preserved, got: %s", got)
	}
}

func TestStandaloneUnderscoreItalicStillWorks(t *testing.T) {
	input := "_italic_"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, "<i>italic</i>") {
		t.Fatalf("expected standalone underscore italics to render, got: %s", got)
	}
}

func TestMarkdownV2EscapedPunctuationIsUnescaped(t *testing.T) {
	input := "\\_ \\* \\[ \\] \\( \\) \\~ \\` \\> \\# \\+ \\- \\= \\| \\{ \\} \\. \\!"
	got := markdownToTelegramHTML(input)

	if strings.Contains(got, "\\_") || strings.Contains(got, "\\*") || strings.Contains(got, "\\[") {
		t.Fatalf("expected markdownv2 escape slashes removed, got: %s", got)
	}
	if !strings.Contains(got, "_ * [ ] ( ) ~ ` &gt; # + - = | { } . !") {
		t.Fatalf("expected punctuation preserved after unescape, got: %s", got)
	}
}

func TestEscapedBackticksDoNotCreateInlineCode(t *testing.T) {
	input := "\\` \\> \\# \\+ \\`"
	got := markdownToTelegramHTML(input)

	if strings.Contains(got, "<code>") {
		t.Fatalf("escaped backticks should not create inline code, got: %s", got)
	}
	if !strings.Contains(got, "` &gt; # + `") {
		t.Fatalf("expected literal backticks and symbols, got: %s", got)
	}
}

func TestMarkdownToTelegramHTMLNoInternalPlaceholderLeak(t *testing.T) {
	input := "Quick Guide\n\n```\nfunc add(a, b int) int {\n\treturn a + b\n}\n```\n\nUse `go test ./...`"
	got := markdownToTelegramHTML(input)

	if strings.Contains(got, "CODEBLOCK0") || strings.Contains(got, "INLINECODE0") || strings.Contains(got, "TABLEBLOCK0") {
		t.Fatalf("internal placeholder leaked into output: %q", got)
	}
	if strings.Contains(got, telegramPlaceholderPrefix) {
		t.Fatalf("new internal placeholder leaked into output: %q", got)
	}
	if !strings.Contains(got, "<pre><code>func add(a, b int) int {") {
		t.Fatalf("expected fenced code block rendering, got: %q", got)
	}
	if !strings.Contains(got, "<code>go test ./...</code>") {
		t.Fatalf("expected inline code rendering, got: %q", got)
	}
}

func TestMarkdownEmptyInput(t *testing.T) {
	got := markdownToTelegramHTML("")
	if got != "" {
		t.Errorf("expected empty, got: %q", got)
	}
}

func TestMarkdownHeadings(t *testing.T) {
	// Headers are stripped to plain text (matching nanobot)
	input := "# Title\n## Subtitle"
	got := markdownToTelegramHTML(input)
	if strings.Contains(got, "#") {
		t.Errorf("heading markers should be stripped, got: %s", got)
	}
	if !strings.Contains(got, "Title") || !strings.Contains(got, "Subtitle") {
		t.Errorf("heading text should be preserved, got: %s", got)
	}
}

func TestMarkdownHorizontalRule(t *testing.T) {
	input := "above\n---\nbelow"
	got := markdownToTelegramHTML(input)
	if !strings.Contains(got, "above") || !strings.Contains(got, "below") {
		t.Errorf("hr should preserve text, got: %s", got)
	}
}

func TestNewTelegramBot(t *testing.T) {
	cfg := &Config{}
	bus := newMessageBus()
	tb := newTelegramBot(cfg, bus)
	if tb.config != cfg {
		t.Error("config not set")
	}
	if tb.bus != bus {
		t.Error("bus not set")
	}
}

func TestTelegramBotStopNilBot(t *testing.T) {
	tb := &TelegramBot{config: &Config{}}
	tb.stop() // Should not panic
}

func TestHandleMessageTextOnly(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	cfg := &Config{}
	tb := &TelegramBot{config: cfg, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 42},
		Chat:      &tgbotapi.Chat{ID: 100},
		Text:      "hello bot",
		Date:      int(time.Now().Unix()),
	}

	go tb.handleMessage(msg)

	inbound := <-bus.Inbound
	if inbound.Content != "hello bot" {
		t.Errorf("content = %q", inbound.Content)
	}
	if inbound.ChatID != "100" {
		t.Errorf("chatID = %q", inbound.ChatID)
	}
	if inbound.SenderID != "42" {
		t.Errorf("senderID = %q", inbound.SenderID)
	}
}

func TestHandleMessageBlocked(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	cfg := &Config{}
	cfg.Telegram.AllowFrom = []string{"999"}
	bus := newMessageBus()
	tb := &TelegramBot{config: cfg, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 42},
		Chat:      &tgbotapi.Chat{ID: 100},
		Text:      "hello",
		Date:      int(time.Now().Unix()),
	}

	tb.handleMessage(msg)

	// Should not send to bus
	select {
	case <-bus.Inbound:
		t.Error("blocked user message should not reach bus")
	case <-time.After(50 * time.Millisecond):
		// Good
	}
}

func TestHandleMessageEmptyContent(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	tb := &TelegramBot{config: &Config{}, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 1},
		Chat:      &tgbotapi.Chat{ID: 1},
		Date:      int(time.Now().Unix()),
		// No text, no media - should be ignored
	}

	tb.handleMessage(msg)

	select {
	case <-bus.Inbound:
		t.Error("empty message should not reach bus")
	case <-time.After(50 * time.Millisecond):
		// Good
	}
}

func TestHandleMessageMalformedMessage(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	tb := &TelegramBot{config: &Config{}, bus: bus, bot: bot}

	tb.handleMessage(nil)
	tb.handleMessage(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 1}, Text: "hi"})
	tb.handleMessage(&tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Text: "hi"})

	select {
	case <-bus.Inbound:
		t.Error("malformed messages should be ignored")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHandleMessageStartCommand(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	tb := &TelegramBot{config: &Config{}, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 1},
		Chat:      &tgbotapi.Chat{ID: 100},
		Text:      "/start",
		Date:      int(time.Now().Unix()),
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 6},
		},
	}

	tb.handleMessage(msg)

	// /start sends a welcome message, not through bus
	select {
	case <-bus.Inbound:
		t.Error("/start should not send to bus")
	case <-time.After(50 * time.Millisecond):
		// Good
	}
}

func TestHandleMessageHelpCommand(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	tb := &TelegramBot{config: &Config{}, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 1},
		Chat:      &tgbotapi.Chat{ID: 100},
		Text:      "/help",
		Date:      int(time.Now().Unix()),
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}

	tb.handleMessage(msg)

	select {
	case <-bus.Inbound:
		t.Error("/help should not send to bus")
	case <-time.After(50 * time.Millisecond):
		// Good
	}
}

func TestHandleMessageHelpBypassesACL(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	cfg := &Config{}
	cfg.Telegram.AllowFrom = []string{"999"} // sender 1 is blocked for normal messages
	bus := newMessageBus()
	tb := &TelegramBot{config: cfg, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 1},
		Chat:      &tgbotapi.Chat{ID: 100},
		Text:      "/help",
		Date:      int(time.Now().Unix()),
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}

	tb.handleMessage(msg)

	// /help is handled directly and should not be forwarded to the agent bus.
	select {
	case <-bus.Inbound:
		t.Error("/help should not send to bus")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHandleMessageNewCommand(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	tb := &TelegramBot{config: &Config{}, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 1},
		Chat:      &tgbotapi.Chat{ID: 100},
		Text:      "/new",
		Date:      int(time.Now().Unix()),
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 4},
		},
	}

	go tb.handleMessage(msg)

	inbound := <-bus.Inbound
	if inbound.Content != "/new" {
		t.Errorf("content = %q, want /new", inbound.Content)
	}
}

func TestHandleMessageVoiceNoGroq(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	cfg := &Config{} // No Groq API key
	tb := &TelegramBot{config: cfg, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 1},
		Chat:      &tgbotapi.Chat{ID: 100},
		Date:      int(time.Now().Unix()),
		Voice:     &tgbotapi.Voice{FileID: "voice123"},
	}

	go tb.handleMessage(msg)

	inbound := <-bus.Inbound
	if !strings.Contains(inbound.Content, "not configured") {
		t.Errorf("voice without groq should mention not configured, got: %q", inbound.Content)
	}
}

func TestHandleMessageAudioNoGroq(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	cfg := &Config{}
	tb := &TelegramBot{config: cfg, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 1},
		Chat:      &tgbotapi.Chat{ID: 100},
		Date:      int(time.Now().Unix()),
		Audio:     &tgbotapi.Audio{FileID: "audio123"},
	}

	go tb.handleMessage(msg)

	inbound := <-bus.Inbound
	if !strings.Contains(inbound.Content, "not configured") {
		t.Errorf("audio without groq should mention not configured, got: %q", inbound.Content)
	}
}

func TestHandleMessagePhoto(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	tb := &TelegramBot{config: &Config{}, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 1},
		Chat:      &tgbotapi.Chat{ID: 100},
		Date:      int(time.Now().Unix()),
		Caption:   "Look at this",
		Photo: []tgbotapi.PhotoSize{
			{FileID: "small", Width: 100, Height: 100},
			{FileID: "large", Width: 800, Height: 600},
		},
	}

	go tb.handleMessage(msg)

	inbound := <-bus.Inbound
	if inbound.Content != "Look at this" {
		t.Errorf("content = %q, want %q", inbound.Content, "Look at this")
	}
	if len(inbound.Media) != 0 {
		t.Fatalf("expected photo media to be ignored, got %d item(s)", len(inbound.Media))
	}
}

func TestHandleMessageDocument(t *testing.T) {
	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/documents/test.txt"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("file content here"))
		case strings.HasSuffix(path, "/getMe"):
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id":         1,
					"is_bot":     true,
					"first_name": "Bot",
					"username":   "bot",
				},
			})
		case strings.HasSuffix(path, "/getFile"):
			json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"file_id": "doc1", "file_path": "documents/test.txt"},
			})
		case strings.HasSuffix(path, "/sendChatAction"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))
	defer tgSrv.Close()

	bot, _ := tgbotapi.NewBotAPIWithClient("tok", tgSrv.URL+"/bot%s/%s", tgSrv.Client())

	bus := newMessageBus()
	cfg := defaultConfig()
	cfg.Workspace = t.TempDir()
	tb := &TelegramBot{config: cfg, bus: bus, bot: bot}

	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 1},
		Chat:      &tgbotapi.Chat{ID: 100},
		Date:      int(time.Now().Unix()),
		Document: &tgbotapi.Document{
			FileID:   "doc1",
			FileName: "test.txt",
			MimeType: "text/plain",
		},
	}

	go tb.handleMessage(msg)

	inbound := <-bus.Inbound
	if !strings.Contains(inbound.Content, "[file: ") || !strings.Contains(inbound.Content, "test.txt") {
		t.Errorf("content = %q", inbound.Content)
	}
}

func TestSaveInboundDocument(t *testing.T) {
	cfg := defaultConfig()
	cfg.Workspace = t.TempDir()
	tb := &TelegramBot{config: cfg, bus: newMessageBus()}

	path, err := tb.saveInboundDocument("test.txt", []byte("hello"))
	if err != nil {
		t.Fatalf("saveInboundDocument failed: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("expected absolute path, got %q", path)
	}
	if !strings.HasPrefix(path, filepath.Join(cfg.workspacePath(), "media")+string(filepath.Separator)) {
		t.Fatalf("expected saved path under workspace media dir, got %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected saved file at %s: %v", path, err)
	}
	if string(data) != "hello" {
		t.Fatalf("saved file content mismatch: %q", string(data))
	}
}

func TestSendMessage(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	cfg := &Config{}
	cfg.Telegram.ReplyToMessage = true
	tb := &TelegramBot{config: cfg, bus: bus, bot: bot}

	tb.send(&OutboundMessage{
		ChatID:           "100",
		Content:          "Hello **user**!",
		ReplyToMessageID: 5,
	})
	// No panic = success (actual Telegram Send is mocked)
}

func TestSendEmptyMessage(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	tb := &TelegramBot{config: &Config{}, bus: newMessageBus(), bot: bot}
	tb.send(&OutboundMessage{ChatID: "100", Content: ""})
	// Should return early, no panic
}

func TestSendInvalidChatID(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	tb := &TelegramBot{config: &Config{}, bus: newMessageBus(), bot: bot}
	tb.send(&OutboundMessage{ChatID: "invalid", Content: "test"})
	// Should log error and return, no panic
}

func TestSendText(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	tb := &TelegramBot{config: &Config{}, bus: newMessageBus(), bot: bot}
	tb.sendText("100", "hello")
	// No panic
}

func TestSendTextInvalidID(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	tb := &TelegramBot{config: &Config{}, bus: newMessageBus(), bot: bot}
	tb.sendText("", "hello") // Should return early
}

func TestStartStopTyping(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	tb := &TelegramBot{config: &Config{}, bus: newMessageBus(), bot: bot}

	tb.startTyping("100")
	time.Sleep(20 * time.Millisecond) // Let goroutine start
	tb.stopTyping("100")

	// Start typing again then stop (tests the "loaded" path)
	tb.startTyping("100")
	tb.startTyping("100") // Replace previous
	tb.stopTyping("100")
}

func TestStartTypingInvalidID(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	tb := &TelegramBot{config: &Config{}, bus: newMessageBus(), bot: bot}
	tb.startTyping("0")          // Zero chat ID — should return early
	tb.stopTyping("nonexistent") // No panic
}

func TestDispatchOutboundIntegration(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	cfg := &Config{}
	cfg.Telegram.SendProgress = true
	cfg.Telegram.SendToolHints = false
	cfg.Telegram.ReplyToMessage = true
	bus := newMessageBus()
	tb := &TelegramBot{config: cfg, bus: bus, bot: bot}

	ctx, cancel := context.WithCancel(context.Background())
	go tb.dispatchOutbound(ctx)

	// Send a normal message
	bus.Outbound <- &OutboundMessage{ChatID: "100", Content: "Hello!", ReplyToMessageID: 1}
	time.Sleep(50 * time.Millisecond)

	// Send a progress message (should be delivered since SendProgress=true)
	bus.Outbound <- &OutboundMessage{ChatID: "100", Content: "thinking...", IsProgress: true}
	time.Sleep(50 * time.Millisecond)

	// Send a tool hint (should be filtered since SendToolHints=false)
	bus.Outbound <- &OutboundMessage{ChatID: "100", Content: "🔧 tool", IsProgress: true, IsToolHint: true}
	time.Sleep(50 * time.Millisecond)

	cancel()
}

func TestFlushMediaGroup(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	tb := &TelegramBot{config: &Config{}, bus: bus, bot: bot}

	// Buffer a media group
	tb.bufferMediaGroup("user1", "100", 1, "group1", "caption", []Media{{Type: "image", URL: "http://example.com/photo.jpg"}})

	// Wait for flush timer
	time.Sleep(700 * time.Millisecond)

	select {
	case inbound := <-bus.Inbound:
		if inbound.Content != "caption" {
			t.Errorf("content = %q", inbound.Content)
		}
		if len(inbound.Media) != 1 {
			t.Errorf("media count = %d", len(inbound.Media))
		}
	case <-time.After(2 * time.Second):
		t.Error("media group was not flushed")
	}
}

func TestFlushMediaGroupNoCaption(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	tb := &TelegramBot{config: &Config{}, bus: bus, bot: bot}

	// Buffer with only a [image: photo.jpg] content which gets filtered
	tb.bufferMediaGroup("user1", "100", 1, "group2", "[image: photo.jpg]", []Media{{Type: "image", URL: "http://example.com"}})

	time.Sleep(700 * time.Millisecond)

	select {
	case inbound := <-bus.Inbound:
		if inbound.Content != "[Media group]" {
			t.Errorf("content = %q, want [Media group]", inbound.Content)
		}
	case <-time.After(2 * time.Second):
		t.Error("media group was not flushed")
	}
}

func TestBufferMediaGroupMultipleMessages(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	bus := newMessageBus()
	tb := &TelegramBot{config: &Config{}, bus: bus, bot: bot}

	// Buffer multiple messages in same group
	tb.bufferMediaGroup("u1", "100", 1, "group3", "first", []Media{{Type: "image", URL: "http://a.com"}})
	tb.bufferMediaGroup("u1", "100", 2, "group3", "second", []Media{{Type: "image", URL: "http://b.com"}})

	time.Sleep(700 * time.Millisecond)

	select {
	case inbound := <-bus.Inbound:
		if !strings.Contains(inbound.Content, "first") || !strings.Contains(inbound.Content, "second") {
			t.Errorf("content = %q, want both captions", inbound.Content)
		}
		if len(inbound.Media) != 2 {
			t.Errorf("media count = %d, want 2", len(inbound.Media))
		}
	case <-time.After(2 * time.Second):
		t.Error("media group was not flushed")
	}
}

func TestStopWithBot(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	tb := &TelegramBot{config: &Config{}, bus: newMessageBus(), bot: bot}

	ctx, cancel := context.WithCancel(context.Background())
	tb.cancelFunc = cancel

	tb.stop()
	// ctx should be cancelled
	select {
	case <-ctx.Done():
		// Good
	default:
		t.Error("stop should cancel context")
	}
}

func TestSendLongMessage(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	cfg := &Config{}
	cfg.Telegram.ReplyToMessage = true
	tb := &TelegramBot{config: cfg, bus: newMessageBus(), bot: bot}

	// Send a message longer than 4000 chars to trigger splitting
	longMsg := strings.Repeat("Hello world! ", 400) // ~5200 chars
	tb.send(&OutboundMessage{
		ChatID:  "100",
		Content: longMsg,
	})
	// No panic = success
}

func TestHandlePhotoNoPhotos(t *testing.T) {
	srv, bot := mockTelegramServer(t)
	defer srv.Close()

	tb := &TelegramBot{config: &Config{}, bus: newMessageBus(), bot: bot}

	msg := &tgbotapi.Message{
		Caption: "just a caption",
		Photo:   []tgbotapi.PhotoSize{},
	}
	content, media := tb.handlePhoto(msg)
	if content != "just a caption" {
		t.Errorf("content = %q", content)
	}
	if media != nil {
		t.Error("expected nil media for empty photos")
	}
}

// Add context import for dispatchOutbound test
var _ = fmt.Sprintf // ensure fmt is used
