package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

//go:embed assets/cody.svg
var codyMascotSVG []byte

// mediaGroupBuffer holds accumulated media group messages.
type mediaGroupBuffer struct {
	mu        sync.Mutex
	senderID  string
	chatID    string
	messageID int
	contents  []string
	media     []Media
	timestamp time.Time
}

type TelegramBot struct {
	config      *Config
	bus         *MessageBus
	bot         *tgbotapi.BotAPI
	cancelFunc  context.CancelFunc
	typing      sync.Map // chatID -> cancel func
	mediaGroups sync.Map // "chatID:mediaGroupID" -> *mediaGroupBuffer
}

func newTelegramBot(cfg *Config, bus *MessageBus) *TelegramBot {
	return &TelegramBot{config: cfg, bus: bus}
}

func (tb *TelegramBot) start(ctx context.Context) error {
	bot, err := tgbotapi.NewBotAPI(tb.config.Telegram.Token)
	if err != nil {
		return fmt.Errorf("telegram bot init: %w", err)
	}
	tb.bot = bot
	slog.Info("Telegram bot connected", "username", bot.Self.UserName)

	ctx, tb.cancelFunc = context.WithCancel(ctx)

	// Drop stale pending updates on startup to match nanobot behavior.
	offset := tb.dropPendingUpdates()

	// Start polling
	u := tgbotapi.NewUpdate(offset)
	u.Timeout = 30
	updates := bot.GetUpdatesChan(u)

	// Start outbound dispatcher
	go tb.dispatchOutbound(ctx)

	// Process updates
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updates:
				if !ok {
					return
				}
				if update.Message != nil {
					tb.handleMessage(update.Message)
				}
			}
		}
	}()

	return nil
}

func (tb *TelegramBot) stop() {
	if tb.cancelFunc != nil {
		tb.cancelFunc()
	}
	if tb.bot != nil {
		tb.bot.StopReceivingUpdates()
	}
}

func (tb *TelegramBot) isAllowed(senderID string) bool {
	if len(tb.config.Telegram.AllowFrom) == 0 {
		return true
	}
	for _, allowed := range tb.config.Telegram.AllowFrom {
		if allowed == senderID {
			return true
		}
		// Also check individual parts (id|username format)
		for _, part := range strings.Split(senderID, "|") {
			if part != "" && part == allowed {
				return true
			}
		}
	}
	return false
}

func (tb *TelegramBot) handleMessage(msg *tgbotapi.Message) {
	if msg == nil || msg.From == nil || msg.Chat == nil {
		slog.Warn("Telegram: received malformed message update")
		return
	}

	senderID := fmt.Sprintf("%d", msg.From.ID)
	if msg.From.UserName != "" {
		senderID = fmt.Sprintf("%d|%s", msg.From.ID, msg.From.UserName)
	}
	chatID := fmt.Sprintf("%d", msg.Chat.ID)

	// Match nanobot behavior: /start and /help are always accessible.
	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			firstName := msg.From.FirstName
			if firstName == "" {
				firstName = "there"
			}
			if len(codyMascotSVG) > 0 {
				tb.sendMediaFile(msg.Chat.ID, Media{Type: "document", Name: "cody.svg", Data: codyMascotSVG}, 0)
			}
			tb.sendText(chatID, fmt.Sprintf("👋 Hi %s! I'm Cody.\n\nSend me a message and I'll respond!\nType /help to see available commands.", firstName))
			return
		case "help":
			tb.sendText(chatID, "🦔 Cody commands:\n/new — Start a new conversation\n/stop — Stop the current task\n/help — Show available commands")
			return
		}
	}

	if !tb.isAllowed(senderID) {
		slog.Warn("Telegram: blocked message from unauthorized user", "user", senderID)
		return
	}

	var content string
	var media []Media

	switch {
	case msg.Voice != nil:
		content = tb.handleVoice(msg)
	case msg.Audio != nil:
		content = tb.handleAudio(msg)
	case msg.Photo != nil:
		content, media = tb.handlePhoto(msg)
	case msg.Document != nil:
		content, media = tb.handleDocument(msg)
	case msg.Text != "":
		content = formatTelegramInboundMarkdown(msg.Text, msg.Entities)
	default:
		return
	}

	if content == "" && len(media) == 0 {
		return
	}

	// Handle commands
	if msg.IsCommand() {
		switch msg.Command() {
		case "new", "stop":
			// These are handled by the agent loop — pass through as content.
			content = "/" + msg.Command()
		}
	}

	// Media group aggregation: buffer grouped photos/documents for 0.6s
	if msg.MediaGroupID != "" {
		tb.bufferMediaGroup(senderID, chatID, msg.MessageID, msg.MediaGroupID, content, media)
		return
	}

	// Start typing indicator and send to bus
	tb.startTyping(chatID)

	sessionKey := "telegram:" + chatID
	tb.bus.Inbound <- &InboundMessage{
		SenderID:   senderID,
		ChatID:     chatID,
		Content:    content,
		Timestamp:  msg.Time(),
		Media:      media,
		SessionKey: sessionKey,
		MessageID:  msg.MessageID,
	}
}

func (tb *TelegramBot) dropPendingUpdates() int {
	if tb.bot == nil {
		return 0
	}

	offset := 0
	for {
		u := tgbotapi.NewUpdate(offset)
		u.Timeout = 0
		u.Limit = 100
		updates, err := tb.bot.GetUpdates(u)
		if err != nil {
			slog.Warn("Failed to drain pending Telegram updates", "err", err)
			return offset
		}
		if len(updates) == 0 {
			return offset
		}
		for _, up := range updates {
			if up.UpdateID >= offset {
				offset = up.UpdateID + 1
			}
		}
	}
}

func (tb *TelegramBot) bufferMediaGroup(senderID, chatID string, messageID int, mediaGroupID, content string, media []Media) {
	key := chatID + ":" + mediaGroupID

	val, loaded := tb.mediaGroups.LoadOrStore(key, &mediaGroupBuffer{
		senderID:  senderID,
		chatID:    chatID,
		messageID: messageID,
		timestamp: time.Now(),
	})
	buf := val.(*mediaGroupBuffer)

	buf.mu.Lock()
	if content != "" && !strings.HasPrefix(content, "[image: ") && !strings.HasPrefix(content, "[file: ") {
		buf.contents = append(buf.contents, content)
	}
	buf.media = append(buf.media, media...)
	buf.mu.Unlock()

	if !loaded {
		// First message in group — start flush timer
		go func() {
			time.Sleep(600 * time.Millisecond)
			tb.flushMediaGroup(key)
		}()
	}
}

func (tb *TelegramBot) flushMediaGroup(key string) {
	val, ok := tb.mediaGroups.LoadAndDelete(key)
	if !ok {
		return
	}
	buf := val.(*mediaGroupBuffer)

	buf.mu.Lock()
	content := strings.Join(buf.contents, "\n")
	media := buf.media
	buf.mu.Unlock()

	if content == "" {
		content = "[Media group]"
	}

	tb.startTyping(buf.chatID)

	sessionKey := "telegram:" + buf.chatID
	tb.bus.Inbound <- &InboundMessage{
		SenderID:   buf.senderID,
		ChatID:     buf.chatID,
		Content:    content,
		Timestamp:  buf.timestamp,
		Media:      media,
		SessionKey: sessionKey,
		MessageID:  buf.messageID,
	}
}

func (tb *TelegramBot) handleVoice(msg *tgbotapi.Message) string {
	if tb.config.Groq.APIKey == "" {
		return "[Voice message received but transcription not configured - set groq.api_key]"
	}
	data, err := tb.downloadFile(msg.Voice.FileID)
	if err != nil {
		slog.Error("Failed to download voice", "err", err)
		return "[Failed to download voice message]"
	}
	text, err := transcribeAudio(tb.config.Groq.APIKey, data, "voice.ogg")
	if err != nil {
		slog.Error("Transcription failed", "err", err)
		return "[Transcription failed]"
	}
	return fmt.Sprintf("[transcription: %s]", text)
}

func (tb *TelegramBot) handleAudio(msg *tgbotapi.Message) string {
	if tb.config.Groq.APIKey == "" {
		return "[Audio message received but transcription not configured]"
	}
	data, err := tb.downloadFile(msg.Audio.FileID)
	if err != nil {
		return "[Failed to download audio]"
	}
	filename := msg.Audio.FileName
	if filename == "" {
		filename = "audio.mp3"
	}
	text, err := transcribeAudio(tb.config.Groq.APIKey, data, filename)
	if err != nil {
		return "[Transcription failed]"
	}
	return fmt.Sprintf("[transcription: %s]", text)
}

func (tb *TelegramBot) handlePhoto(msg *tgbotapi.Message) (string, []Media) {
	caption := formatTelegramInboundMarkdown(msg.Caption, msg.CaptionEntities)
	if caption == "" {
		caption = "[image: photo.jpg]"
	}
	// Cody intentionally does not send images to the LLM (text-only model setup).
	return caption, nil
}

func (tb *TelegramBot) handleDocument(msg *tgbotapi.Message) (string, []Media) {
	filename := sanitizeInboundFilename(msg.Document.FileName)
	caption := formatTelegramInboundMarkdown(msg.Caption, msg.CaptionEntities)
	fileRef := fmt.Sprintf("[file: %s]", filename)

	data, err := tb.downloadFile(msg.Document.FileID)
	if err != nil {
		if caption == "" {
			caption = fileRef
		}
		return caption, nil
	}

	savedPath, err := tb.saveInboundDocument(filename, data)
	if err != nil {
		slog.Warn("Failed to save inbound Telegram document", "file", filename, "err", err)
		if caption == "" {
			caption = fileRef
		}
		return caption, nil
	}

	pathRef := fmt.Sprintf("[file: %s]", savedPath)
	if caption == "" {
		caption = pathRef
	} else {
		caption = caption + "\n" + pathRef
	}

	return caption, []Media{{
		Type:     "document",
		URL:      savedPath,
		Name:     filename,
		MimeType: msg.Document.MimeType,
	}}
}

func (tb *TelegramBot) inboundMediaDir() string {
	if tb.config != nil {
		if ws := strings.TrimSpace(tb.config.workspacePath()); ws != "" {
			return filepath.Join(ws, "media")
		}
	}
	return filepath.Join(codyDir(), "media")
}

func sanitizeInboundFilename(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." {
		return "file"
	}
	return base
}

func uniqueMediaPath(dir, name string) string {
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem = "file"
	}
	for i := 1; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s_%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func (tb *TelegramBot) saveInboundDocument(filename string, data []byte) (string, error) {
	dir := tb.inboundMediaDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	path := uniqueMediaPath(dir, sanitizeInboundFilename(filename))
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}

func (tb *TelegramBot) downloadFile(fileID string) ([]byte, error) {
	url, err := tb.bot.GetFileDirectURL(fileID)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed with status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read downloaded file: %w", err)
	}
	return data, nil
}

func (tb *TelegramBot) dispatchOutbound(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-tb.bus.Outbound:
			// Filter progress messages based on config
			if msg.IsProgress {
				if msg.IsToolHint && !tb.config.Telegram.SendToolHints {
					continue
				}
				if !msg.IsToolHint && !tb.config.Telegram.SendProgress {
					continue
				}
			}
			tb.stopTyping(msg.ChatID)
			tb.send(msg)
		}
	}
}

func (tb *TelegramBot) send(msg *OutboundMessage) {
	if msg.Content == "" && len(msg.Media) == 0 {
		return
	}

	chatID := parseChatID(msg.ChatID)
	if chatID == 0 {
		slog.Error("Invalid chat ID", "id", msg.ChatID)
		return
	}

	// Send media files before text (matching nanobot behavior)
	replyToID := 0
	if !msg.IsProgress && msg.ReplyToMessageID != 0 && tb.config.Telegram.ReplyToMessage {
		replyToID = msg.ReplyToMessageID
	}
	for _, m := range msg.Media {
		tb.sendMediaFile(chatID, m, replyToID)
	}

	if msg.Content == "" {
		return
	}

	content := normalizeMarkdownTablesForTelegram(msg.Content)
	chunks := splitMessageForTelegram(content, 3900)

	for _, chunk := range chunks {
		html := markdownToTelegramHTML(chunk)
		m := tgbotapi.NewMessage(chatID, chunk)
		m.ParseMode = "HTML"
		m.DisableWebPagePreview = true
		m.Text = html

		// Match nanobot behavior: apply reply target to every chunk.
		if !msg.IsProgress && msg.ReplyToMessageID != 0 && tb.config.Telegram.ReplyToMessage {
			m.ReplyToMessageID = msg.ReplyToMessageID
		}

		if _, err := tb.bot.Send(m); err != nil {
			// Retry without HTML formatting
			slog.Warn("Telegram send with HTML failed, retrying plain", "err", err)
			m.ParseMode = ""
			m.Text = chunk
			if _, err = tb.bot.Send(m); err != nil {
				slog.Warn("Telegram send plain fallback failed", "err", err)
			}
		}
	}
}

func (tb *TelegramBot) sendText(chatID, text string) {
	id := parseChatID(chatID)
	if id == 0 {
		return
	}
	m := tgbotapi.NewMessage(id, text)
	if _, err := tb.bot.Send(m); err != nil {
		slog.Warn("Telegram sendText failed", "err", err)
	}
}

func (tb *TelegramBot) sendMediaFile(chatID int64, m Media, replyToMessageID int) {
	mediaType := mediaSendType(m)
	var file tgbotapi.RequestFileData
	if len(m.Data) > 0 {
		name := m.Name
		if name == "" {
			name = "file"
		}
		file = tgbotapi.FileBytes{Name: name, Bytes: m.Data}
	} else if m.URL != "" {
		file = tgbotapi.FilePath(m.URL)
	} else {
		return
	}
	var msg tgbotapi.Chattable
	switch mediaType {
	case "photo":
		c := tgbotapi.NewPhoto(chatID, file)
		c.ReplyToMessageID = replyToMessageID
		msg = c
	case "voice":
		c := tgbotapi.NewVoice(chatID, file)
		c.ReplyToMessageID = replyToMessageID
		msg = c
	case "audio":
		c := tgbotapi.NewAudio(chatID, file)
		c.ReplyToMessageID = replyToMessageID
		msg = c
	default:
		c := tgbotapi.NewDocument(chatID, file)
		c.ReplyToMessageID = replyToMessageID
		msg = c
	}
	if _, err := tb.bot.Send(msg); err != nil {
		name := m.Name
		if name == "" {
			name = m.URL
		}
		slog.Error("Failed to send media", "name", name, "err", err)
		errMsg := tgbotapi.NewMessage(chatID, fmt.Sprintf("[Failed to send: %s]", filepath.Base(name)))
		if _, sendErr := tb.bot.Send(errMsg); sendErr != nil {
			slog.Warn("Failed to send media error notification", "err", sendErr)
		}
	}
}

func mediaSendType(m Media) string {
	if m.Type == "image" {
		return "photo"
	}
	name := m.Name
	if name == "" {
		name = m.URL
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "photo"
	case ".ogg":
		return "voice"
	case ".mp3", ".m4a", ".wav", ".aac":
		return "audio"
	default:
		return "document"
	}
}

func (tb *TelegramBot) startTyping(chatID string) {
	id := parseChatID(chatID)
	if id == 0 {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	if prev, loaded := tb.typing.LoadOrStore(chatID, cancel); loaded {
		// Cancel previous typing
		prev.(context.CancelFunc)()
		tb.typing.Store(chatID, cancel)
	}

	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			action := tgbotapi.NewChatAction(id, tgbotapi.ChatTyping)
			if _, err := tb.bot.Send(action); err != nil {
				slog.Debug("Telegram typing indicator failed", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (tb *TelegramBot) stopTyping(chatID string) {
	if cancel, ok := tb.typing.LoadAndDelete(chatID); ok {
		cancel.(context.CancelFunc)()
	}
}

func parseChatID(s string) int64 {
	var id int64
	fmt.Sscanf(s, "%d", &id)
	return id
}

type telegramInboundEntityMarker struct {
	start int
	end   int
	open  string
	close string
}

func formatTelegramInboundMarkdown(text string, entities []tgbotapi.MessageEntity) string {
	if text == "" || len(entities) == 0 {
		return text
	}

	markers := make([]telegramInboundEntityMarker, 0, len(entities))
	for _, ent := range entities {
		start, end, ok := telegramEntityRuneRange(text, ent.Offset, ent.Length)
		if !ok || start >= end {
			continue
		}

		open, close, ok := telegramEntityMarkdownDelimiters(ent)
		if !ok {
			continue
		}

		markers = append(markers, telegramInboundEntityMarker{
			start: start,
			end:   end,
			open:  open,
			close: close,
		})
	}

	if len(markers) == 0 {
		return text
	}

	opens := make(map[int][]telegramInboundEntityMarker)
	closes := make(map[int][]telegramInboundEntityMarker)
	for _, m := range markers {
		opens[m.start] = append(opens[m.start], m)
		closes[m.end] = append(closes[m.end], m)
	}

	runes := []rune(text)
	var b strings.Builder

	for i := 0; i <= len(runes); i++ {
		if cs := closes[i]; len(cs) > 0 {
			sort.Slice(cs, func(a, b int) bool {
				spanA := cs[a].end - cs[a].start
				spanB := cs[b].end - cs[b].start
				if spanA != spanB {
					return spanA < spanB // close inner entities first
				}
				return cs[a].start > cs[b].start
			})
			for _, m := range cs {
				b.WriteString(m.close)
			}
		}

		if i == len(runes) {
			break
		}

		if os := opens[i]; len(os) > 0 {
			sort.Slice(os, func(a, b int) bool {
				spanA := os[a].end - os[a].start
				spanB := os[b].end - os[b].start
				if spanA != spanB {
					return spanA > spanB // open outer entities first
				}
				return os[a].start < os[b].start
			})
			for _, m := range os {
				b.WriteString(m.open)
			}
		}

		b.WriteRune(runes[i])
	}

	return b.String()
}

func telegramEntityRuneRange(text string, utf16Offset, utf16Length int) (int, int, bool) {
	if utf16Offset < 0 || utf16Length <= 0 {
		return 0, 0, false
	}
	start, ok := utf16OffsetToRuneIndex(text, utf16Offset)
	if !ok {
		return 0, 0, false
	}
	end, ok := utf16OffsetToRuneIndex(text, utf16Offset+utf16Length)
	if !ok {
		return 0, 0, false
	}
	return start, end, true
}

func utf16OffsetToRuneIndex(text string, target int) (int, bool) {
	if target < 0 {
		return 0, false
	}

	utf16Pos := 0
	runePos := 0
	for _, r := range text {
		if utf16Pos == target {
			return runePos, true
		}
		if r > 0xFFFF {
			utf16Pos += 2
		} else {
			utf16Pos++
		}
		runePos++
	}

	if utf16Pos == target {
		return runePos, true
	}
	return 0, false
}

func telegramEntityMarkdownDelimiters(ent tgbotapi.MessageEntity) (string, string, bool) {
	switch ent.Type {
	case "bold":
		return "**", "**", true
	case "italic":
		return "_", "_", true
	case "strikethrough":
		return "~~", "~~", true
	case "code":
		return "`", "`", true
	case "pre":
		lang := strings.TrimSpace(ent.Language)
		if lang == "" {
			return "```\n", "\n```", true
		}
		return "```" + lang + "\n", "\n```", true
	case "text_link":
		if ent.URL == "" {
			return "", "", false
		}
		return "[", "](" + ent.URL + ")", true
	case "text_mention":
		if ent.User == nil {
			return "", "", false
		}
		return "[", fmt.Sprintf("](tg://user?id=%d)", ent.User.ID), true
	default:
		return "", "", false
	}
}

// --- Markdown to Telegram HTML conversion ---

var (
	telegramBoldRe     = regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__`)
	telegramItalicRe   = regexp.MustCompile(`(^|[^*])\*([^*\n]+?)\*([^*]|$)`)
	underscoreItalicRe = regexp.MustCompile(`(^|[^A-Za-z0-9])_([^_\n]+?)_([^A-Za-z0-9]|$)`)
	telegramStrikeRe   = regexp.MustCompile(`~~(.+?)~~`)
	markdownV2EscapeRe = regexp.MustCompile("\\\\([_\\*\\[\\]\\(\\)~`>#+\\-=|{}.!])")
	htmlBreakRe        = regexp.MustCompile(`(?i)<br\s*/?>`)
	markdownAutoLinkRe = regexp.MustCompile(`<(https?://[^>\s]+)>`)
)

const telegramPlaceholderPrefix = "@@CODYTG_"

func telegramPlaceholder(kind string, idx int) string {
	return fmt.Sprintf("%s%s_%d@@", telegramPlaceholderPrefix, kind, idx)
}

func unescapeMarkdownV2Escapes(text string) string {
	return markdownV2EscapeRe.ReplaceAllString(text, "$1")
}

func formatTelegramInlineMarkdown(text string) string {
	// Some models emit HTML break tags even when asked for markdown.
	// Normalize them to plain newlines before escaping.
	text = htmlBreakRe.ReplaceAllString(text, "\n")
	// Normalize markdown autolinks (<https://...>) to regular markdown links
	// so they become clickable anchors in Telegram HTML mode.
	text = normalizeMarkdownAutoLinks(text)

	text = escapeHTML(text)

	// Links [text](url) - must be before bold/italic to handle nested cases.
	text = replaceMarkdownLinks(text)

	// Bold **text** or __text__
	text = telegramBoldRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := telegramBoldRe.FindStringSubmatch(match)
		content := parts[1]
		if content == "" {
			content = parts[2]
		}
		return "<b>" + content + "</b>"
	})

	// Italic *text*
	text = telegramItalicRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := telegramItalicRe.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		return parts[1] + "<i>" + parts[2] + "</i>" + parts[3]
	})

	// Italic _text_ with word-boundary safety to avoid some_var_name false positives.
	text = underscoreItalicRe.ReplaceAllString(text, "$1<i>$2</i>$3")

	// Strikethrough ~~text~~
	text = telegramStrikeRe.ReplaceAllString(text, "<s>$1</s>")

	return text
}

func normalizeMarkdownAutoLinks(text string) string {
	return markdownAutoLinkRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := markdownAutoLinkRe.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		url := strings.TrimSpace(parts[1])
		if url == "" {
			return match
		}
		return "[" + url + "](" + url + ")"
	})
}

func markdownToTelegramHTML(text string) string {
	text = unwrapTopLevelMarkdownFence(text)

	// Protect code blocks first
	var codeBlocks []string
	codeBlockRe := regexp.MustCompile("(?s)```([^`\n]*)\n?(.*?)```")
	text = codeBlockRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := codeBlockRe.FindStringSubmatch(match)
		placeholder := telegramPlaceholder("CODEBLOCK", len(codeBlocks))
		code := escapeHTML(parts[2])
		codeBlocks = append(codeBlocks, fmt.Sprintf("<pre><code>%s</code></pre>", strings.TrimSpace(code)))
		return placeholder
	})

	// Inline code
	var inlineCodes []string
	inlineCodeRe := regexp.MustCompile("(^|[^\\\\])`([^`\n]+)`")
	text = inlineCodeRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := inlineCodeRe.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		placeholder := telegramPlaceholder("INLINECODE", len(inlineCodes))
		inlineCodes = append(inlineCodes, fmt.Sprintf("<code>%s</code>", escapeHTML(parts[2])))
		return parts[1] + placeholder
	})

	// Convert markdown tables into readable HTML list blocks since Telegram does not render table syntax.
	text, tableBlocks := extractMarkdownTables(text)

	// Strip headers: # Title -> Title
	headerRe := regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}\s+(.+)$`)
	text = headerRe.ReplaceAllString(text, "$1")

	// Strip blockquotes: > text -> text
	blockquoteRe := regexp.MustCompile(`(?m)^[ \t]{0,3}>\s*(.*)$`)
	text = blockquoteRe.ReplaceAllString(text, "$1")

	// LLMs often emit MarkdownV2-escaped punctuation; strip escape slashes before HTML conversion.
	text = unescapeMarkdownV2Escapes(text)

	// Escape remaining HTML and apply inline markdown transforms.
	text = formatTelegramInlineMarkdown(text)

	// Convert markdown list markers to unicode bullets
	bulletRe := regexp.MustCompile(`(?m)^[ \t]{0,3}[-*]\s+`)
	text = bulletRe.ReplaceAllString(text, "• ")

	// Restore table blocks, code blocks and inline code
	for i, block := range tableBlocks {
		text = strings.ReplaceAll(text, telegramPlaceholder("TABLEBLOCK", i), block)
	}
	for i, block := range codeBlocks {
		text = strings.ReplaceAll(text, telegramPlaceholder("CODEBLOCK", i), block)
	}
	for i, code := range inlineCodes {
		text = strings.ReplaceAll(text, telegramPlaceholder("INLINECODE", i), code)
	}

	return text
}

func unwrapTopLevelMarkdownFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") {
		return text
	}

	firstNL := strings.IndexByte(trimmed, '\n')
	if firstNL == -1 {
		return text
	}

	lang := strings.ToLower(strings.TrimSpace(trimmed[3:firstNL]))
	if lang != "md" && lang != "markdown" {
		return text
	}

	body := strings.TrimSuffix(trimmed[firstNL+1:], "```")
	body = strings.Trim(body, "\n")
	if body == "" {
		return ""
	}
	return body
}

func replaceMarkdownLinks(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		if text[i] != '[' {
			b.WriteByte(text[i])
			i++
			continue
		}

		closeBracket := i + 1
		for closeBracket < len(text) {
			if text[closeBracket] == '\\' && closeBracket+1 < len(text) {
				closeBracket += 2
				continue
			}
			if text[closeBracket] == ']' {
				break
			}
			closeBracket++
		}
		if closeBracket >= len(text) || closeBracket+1 >= len(text) || text[closeBracket+1] != '(' {
			b.WriteByte(text[i])
			i++
			continue
		}

		urlStart := closeBracket + 2
		depth := 1
		urlEnd := -1
		for j := urlStart; j < len(text); j++ {
			if text[j] == '\\' && j+1 < len(text) {
				j++
				continue
			}
			switch text[j] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					urlEnd = j
					j = len(text)
				}
			}
		}

		if urlEnd == -1 {
			b.WriteByte(text[i])
			i++
			continue
		}

		label := text[i+1 : closeBracket]
		href := strings.TrimSpace(text[urlStart:urlEnd])
		if strings.TrimSpace(label) == "" || href == "" {
			b.WriteByte(text[i])
			i++
			continue
		}

		href = strings.ReplaceAll(href, `"`, "&quot;")
		b.WriteString(`<a href="`)
		b.WriteString(href)
		b.WriteString(`">`)
		b.WriteString(label)
		b.WriteString(`</a>`)
		i = urlEnd + 1
	}
	return b.String()
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func extractMarkdownTables(text string) (string, []string) {
	return extractMarkdownTablesWithRenderer(text, renderTableCodeBlock)
}

func extractMarkdownTablesWithRenderer(text string, renderer func(header []string, rows [][]string) string) (string, []string) {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	var blocks []string

	for i := 0; i < len(lines); {
		header, ok := parseMarkdownTableRow(lines[i])
		if !ok || i+1 >= len(lines) || !isMarkdownTableSeparator(lines[i+1], len(header)) {
			out = append(out, lines[i])
			i++
			continue
		}

		j := i + 2
		rows := make([][]string, 0)
		for j < len(lines) {
			cells, rowOK := parseMarkdownTableRow(lines[j])
			if !rowOK || len(cells) != len(header) {
				break
			}
			rows = append(rows, cells)
			j++
		}

		placeholder := telegramPlaceholder("TABLEBLOCK", len(blocks))
		blocks = append(blocks, renderer(header, rows))
		out = append(out, placeholder)
		i = j
	}

	return strings.Join(out, "\n"), blocks
}

func normalizeMarkdownTablesForTelegram(text string) string {
	if text == "" {
		return text
	}

	var codeBlocks []string
	codeBlockRe := regexp.MustCompile("(?s)```([^`\n]*)\n?(.*?)```")
	text = codeBlockRe.ReplaceAllStringFunc(text, func(match string) string {
		placeholder := telegramPlaceholder("TABLECODEBLOCK", len(codeBlocks))
		codeBlocks = append(codeBlocks, match)
		return placeholder
	})

	text, tableBlocks := extractMarkdownTablesWithRenderer(text, renderTableMarkdownBlock)
	for i, block := range tableBlocks {
		text = strings.ReplaceAll(text, telegramPlaceholder("TABLEBLOCK", i), block)
	}
	for i, block := range codeBlocks {
		text = strings.ReplaceAll(text, telegramPlaceholder("TABLECODEBLOCK", i), block)
	}

	return text
}

func parseMarkdownTableRow(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, "|") {
		return nil, false
	}
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")

	parts := strings.Split(trimmed, "|")
	if len(parts) < 2 {
		return nil, false
	}
	cells := make([]string, 0, len(parts))
	nonEmpty := false
	for _, part := range parts {
		cell := strings.TrimSpace(part)
		if cell != "" {
			nonEmpty = true
		}
		cells = append(cells, cell)
	}
	if !nonEmpty {
		return nil, false
	}
	return cells, true
}

func isMarkdownTableSeparator(line string, columns int) bool {
	cells, ok := parseMarkdownTableRow(line)
	if !ok || len(cells) != columns {
		return false
	}
	for _, cell := range cells {
		if !isMarkdownSeparatorCell(cell) {
			return false
		}
	}
	return true
}

func isMarkdownSeparatorCell(cell string) bool {
	c := strings.TrimSpace(cell)
	if c == "" {
		return false
	}
	c = strings.TrimPrefix(c, ":")
	c = strings.TrimSuffix(c, ":")
	if len(c) < 3 {
		return false
	}
	for _, r := range c {
		if r != '-' {
			return false
		}
	}
	return true
}

func normalizeMarkdownLabel(label string) string {
	s := strings.TrimSpace(label)
	if s == "" {
		return s
	}

	for {
		changed := false
		switch {
		case len(s) > 4 && strings.HasPrefix(s, "**") && strings.HasSuffix(s, "**"):
			s = strings.TrimSpace(s[2 : len(s)-2])
			changed = true
		case len(s) > 4 && strings.HasPrefix(s, "__") && strings.HasSuffix(s, "__"):
			s = strings.TrimSpace(s[2 : len(s)-2])
			changed = true
		case len(s) > 2 && strings.HasPrefix(s, "`") && strings.HasSuffix(s, "`"):
			s = strings.TrimSpace(s[1 : len(s)-1])
			changed = true
		case len(s) > 2 && strings.HasPrefix(s, "*") && strings.HasSuffix(s, "*"):
			s = strings.TrimSpace(s[1 : len(s)-1])
			changed = true
		case len(s) > 2 && strings.HasPrefix(s, "_") && strings.HasSuffix(s, "_"):
			s = strings.TrimSpace(s[1 : len(s)-1])
			changed = true
		}
		if !changed {
			break
		}
	}

	return s
}

func renderTableCodeBlock(header []string, rows [][]string) string {
	var b strings.Builder
	for ri, row := range rows {
		if ri > 0 {
			b.WriteString("\n\n")
		}

		// Two-column markdown tables are usually "label | details". Render them as stacked bullets.
		if len(header) == 2 {
			title := ""
			details := ""
			if len(row) > 0 {
				title = normalizeMarkdownLabel(row[0])
			}
			if len(row) > 1 {
				details = strings.TrimSpace(row[1])
			}

			b.WriteString("• <b>")
			b.WriteString(escapeHTML(title))
			b.WriteString("</b>")
			if details != "" {
				b.WriteString("\n")
				b.WriteString(formatTelegramInlineMarkdown(details))
			}
			continue
		}

		b.WriteString("• ")
		wrote := false
		for i, cell := range row {
			if i >= len(header) {
				break
			}
			value := strings.TrimSpace(cell)
			if value == "" {
				continue
			}
			if wrote {
				b.WriteString(" | ")
			}
			b.WriteString("<b>")
			b.WriteString(escapeHTML(normalizeMarkdownLabel(header[i])))
			b.WriteString(":</b> ")
			b.WriteString(formatTelegramInlineMarkdown(value))
			wrote = true
		}
		if !wrote {
			b.WriteString(formatTelegramInlineMarkdown(strings.Join(row, " ")))
		}
	}

	return b.String()
}

func renderTableMarkdownBlock(header []string, rows [][]string) string {
	var b strings.Builder
	for ri, row := range rows {
		if ri > 0 {
			b.WriteString("\n\n")
		}

		if len(header) == 2 {
			title := ""
			details := ""
			if len(row) > 0 {
				title = normalizeMarkdownLabel(row[0])
			}
			if len(row) > 1 {
				details = strings.TrimSpace(row[1])
			}

			b.WriteString("- ")
			if title != "" {
				b.WriteString("**")
				b.WriteString(title)
				b.WriteString("**")
			}
			if details != "" {
				if title != "" {
					b.WriteString("\n")
				}
				b.WriteString(details)
			}
			continue
		}

		b.WriteString("- ")
		wrote := false
		for i, cell := range row {
			if i >= len(header) {
				break
			}
			value := strings.TrimSpace(cell)
			if value == "" {
				continue
			}
			if wrote {
				b.WriteString(" | ")
			}
			label := normalizeMarkdownLabel(header[i])
			if label != "" {
				b.WriteString("**")
				b.WriteString(label)
				b.WriteString(":** ")
			}
			b.WriteString(value)
			wrote = true
		}
		if !wrote {
			b.WriteString(strings.TrimSpace(strings.Join(row, " ")))
		}
	}

	return b.String()
}

func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		// Match nanobot behavior: split at newline, then space, else hard cut.
		cut := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n"); idx != -1 {
			cut = idx
		} else if idx := strings.LastIndex(text[:maxLen], " "); idx != -1 {
			cut = idx
		}
		if cut == 0 {
			cut = maxLen
		}
		cut = clampToRuneBoundary(text, cut)
		if cut == 0 {
			_, size := utf8.DecodeRuneInString(text)
			if size <= 0 {
				size = 1
			}
			cut = size
		}
		chunks = append(chunks, text[:cut])
		text = strings.TrimLeft(text[cut:], " \t\r\n")
	}
	return chunks
}

func clampToRuneBoundary(text string, idx int) int {
	if idx <= 0 {
		return 0
	}
	if idx >= len(text) {
		return len(text)
	}
	for idx > 0 && !utf8.RuneStart(text[idx]) {
		idx--
	}
	return idx
}

func splitMessageForTelegram(text string, maxHTMLLen int) []string {
	if text == "" {
		return []string{text}
	}
	initial := splitMessage(text, maxHTMLLen)
	var out []string
	for _, chunk := range initial {
		out = append(out, splitChunkToRenderedLimit(chunk, maxHTMLLen)...)
	}

	// Keep fenced code blocks balanced per chunk so Telegram HTML rendering
	// doesn't leak raw ``` markers when a split lands inside a fence.
	for range 2 {
		out = rebalanceFencedCodeChunks(out)
		var adjusted []string
		changed := false
		for _, chunk := range out {
			if len(markdownToTelegramHTML(chunk)) <= maxHTMLLen {
				adjusted = append(adjusted, chunk)
				continue
			}
			parts := splitChunkToRenderedLimit(chunk, maxHTMLLen)
			if len(parts) > 1 {
				changed = true
			}
			adjusted = append(adjusted, parts...)
		}
		out = adjusted
		if !changed {
			break
		}
	}

	return out
}

func splitChunkToRenderedLimit(chunk string, maxHTMLLen int) []string {
	queue := []string{chunk}
	var out []string

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		rendered := markdownToTelegramHTML(current)
		if len(rendered) <= maxHTMLLen || len(current) <= 1 {
			out = append(out, current)
			continue
		}

		nextLimit := len(current) * maxHTMLLen / len(rendered)
		if nextLimit >= len(current) {
			nextLimit = len(current) - 1
		}
		if nextLimit < 200 {
			nextLimit = 200
		}

		parts := splitMessage(current, nextLimit)
		if len(parts) < 2 {
			mid := len(current) / 2
			if mid == 0 {
				out = append(out, current)
				continue
			}
			parts = []string{current[:mid], strings.TrimLeft(current[mid:], " \t\r\n")}
		}
		queue = append(parts, queue...)
	}

	return out
}

func rebalanceFencedCodeChunks(chunks []string) []string {
	if len(chunks) <= 1 {
		return chunks
	}

	out := make([]string, 0, len(chunks))
	openLang := ""

	for i, original := range chunks {
		chunk := original
		if openLang != "" {
			chunk = "```" + openLang + "\n" + strings.TrimLeft(chunk, "\n")
		}

		inFence := false
		lang := ""
		for _, line := range strings.Split(chunk, "\n") {
			l, ok := markdownFenceLang(line)
			if !ok {
				continue
			}
			if inFence {
				inFence = false
				lang = ""
			} else {
				inFence = true
				lang = l
			}
		}

		if inFence && i < len(chunks)-1 {
			chunk = strings.TrimRight(chunk, "\n") + "\n```"
			openLang = lang
		} else {
			openLang = ""
		}

		out = append(out, chunk)
	}

	return out
}

func markdownFenceLang(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "```") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "```")), true
}
