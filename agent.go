package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// --- Skills Loader ---

type skillMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Always      bool   `yaml:"always"`
	Requires    struct {
		Bin []string `yaml:"bins"`
		Env []string `yaml:"env"`
	} `yaml:"requires"`
	Requirements struct {
		Bin []string `yaml:"bin"`
		Env []string `yaml:"env"`
	} `yaml:"requirements"`
}

type SkillsLoader struct {
	workspace string
}

func newSkillsLoader(workspace string) *SkillsLoader {
	return &SkillsLoader{workspace: workspace}
}

func (sl *SkillsLoader) loadSkill(name string) (string, error) {
	// Try workspace first, then embedded
	wsPath := filepath.Join(sl.workspace, "skills", name, "SKILL.md")
	if data, err := os.ReadFile(wsPath); err == nil {
		return stripFrontmatter(string(data)), nil
	}
	embPath := "skills/" + name + "/SKILL.md"
	if data, err := skillsFS.ReadFile(embPath); err == nil {
		return stripFrontmatter(string(data)), nil
	}
	return "", fmt.Errorf("skill not found: %s", name)
}

func (sl *SkillsLoader) getMeta(name string) *skillMeta {
	// Try workspace first, then embedded
	var data []byte
	wsPath := filepath.Join(sl.workspace, "skills", name, "SKILL.md")
	if d, err := os.ReadFile(wsPath); err == nil {
		data = d
	} else {
		embPath := "skills/" + name + "/SKILL.md"
		d, err := skillsFS.ReadFile(embPath)
		if err != nil {
			return nil
		}
		data = d
	}
	return parseFrontmatter(string(data))
}

func (sl *SkillsLoader) listSkills() []skillMeta {
	seen := make(map[string]bool)
	var skills []skillMeta

	// Workspace skills
	wsDir := filepath.Join(sl.workspace, "skills")
	if entries, err := os.ReadDir(wsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			seen[e.Name()] = true
			if meta := sl.getMeta(e.Name()); meta != nil {
				skills = append(skills, *meta)
			}
		}
	}

	// Embedded skills
	if entries, err := skillsFS.ReadDir("skills"); err == nil {
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			if meta := sl.getMeta(e.Name()); meta != nil {
				skills = append(skills, *meta)
			}
		}
	}
	return skills
}

func (sl *SkillsLoader) alwaysOnSkills() []string {
	var names []string
	for _, s := range sl.listSkills() {
		if s.Always && sl.checkRequirements(s) {
			names = append(names, s.Name)
		}
	}
	return names
}

func (sl *SkillsLoader) checkRequirements(meta skillMeta) bool {
	for _, bin := range meta.Requirements.Bin {
		if _, err := osexec.LookPath(bin); err != nil {
			return false
		}
	}
	for _, env := range meta.Requirements.Env {
		if os.Getenv(env) == "" {
			return false
		}
	}
	return true
}

func (sl *SkillsLoader) buildSummary() string {
	skills := sl.listSkills()
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<skills>\n")
	for _, s := range skills {
		available := sl.checkRequirements(s)
		name := escapeXML(s.Name)
		desc := escapeXML(s.Description)
		location := filepath.Join(sl.workspace, "skills", s.Name, "SKILL.md")

		fmt.Fprintf(&sb, "  <skill available=\"%t\">\n", available)
		fmt.Fprintf(&sb, "    <name>%s</name>\n", name)
		fmt.Fprintf(&sb, "    <description>%s</description>\n", desc)
		fmt.Fprintf(&sb, "    <location>%s</location>\n", location)

		if !available {
			if missing := sl.getMissingRequirements(s); missing != "" {
				fmt.Fprintf(&sb, "    <requires>%s</requires>\n", escapeXML(missing))
			}
		}
		sb.WriteString("  </skill>\n")
	}
	sb.WriteString("</skills>\n")
	return sb.String()
}

func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func (sl *SkillsLoader) getMissingRequirements(meta skillMeta) string {
	var missing []string
	for _, bin := range meta.Requirements.Bin {
		if _, err := osexec.LookPath(bin); err != nil {
			missing = append(missing, "CLI: "+bin)
		}
	}
	for _, env := range meta.Requirements.Env {
		if os.Getenv(env) == "" {
			missing = append(missing, "ENV: "+env)
		}
	}
	return strings.Join(missing, ", ")
}

func parseFrontmatter(content string) *skillMeta {
	if !strings.HasPrefix(content, "---") {
		return nil
	}
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return nil
	}
	var meta skillMeta
	if err := yaml.Unmarshal([]byte(parts[1]), &meta); err != nil {
		return nil
	}

	// Support both native YAML requirements and nanobot/openclaw-style
	// metadata JSON frontmatter.
	meta.Requirements.Bin = appendUnique(meta.Requirements.Bin, meta.Requires.Bin...)
	meta.Requirements.Env = appendUnique(meta.Requirements.Env, meta.Requires.Env...)

	var raw map[string]any
	if err := yaml.Unmarshal([]byte(parts[1]), &raw); err == nil {
		mergeSkillRequirements(&meta, raw["requires"])
		mergeSkillRequirements(&meta, raw["requirements"])
		if scoped := parseSkillMetadataScope(raw["metadata"]); scoped != nil {
			if !meta.Always {
				if v, ok := scoped["always"].(bool); ok {
					meta.Always = v
				}
			}
			mergeSkillRequirements(&meta, scoped["requires"])
			mergeSkillRequirements(&meta, scoped["requirements"])
		}
	}
	return &meta
}

func parseSkillMetadataScope(raw any) map[string]any {
	metadata := asStringAnyMap(raw)
	if metadata == nil {
		if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
			var parsed any
			if err := json.Unmarshal([]byte(s), &parsed); err == nil {
				metadata = asStringAnyMap(parsed)
			}
		}
	}
	if metadata == nil {
		return nil
	}
	for _, key := range []string{"cody", "nanobot", "openclaw"} {
		if scoped := asStringAnyMap(metadata[key]); scoped != nil {
			return scoped
		}
	}
	return metadata
}

func asStringAnyMap(raw any) map[string]any {
	switch m := raw.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, v := range m {
			s, ok := k.(string)
			if !ok {
				continue
			}
			out[s] = v
		}
		return out
	default:
		return nil
	}
}

func mergeSkillRequirements(meta *skillMeta, raw any) {
	m := asStringAnyMap(raw)
	if m == nil {
		return
	}
	meta.Requirements.Bin = appendUnique(meta.Requirements.Bin, asStringSlice(m["bin"])...)
	meta.Requirements.Bin = appendUnique(meta.Requirements.Bin, asStringSlice(m["bins"])...)
	meta.Requirements.Env = appendUnique(meta.Requirements.Env, asStringSlice(m["env"])...)
}

func asStringSlice(raw any) []string {
	switch v := raw.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	default:
		return nil
	}
}

func appendUnique(base []string, values ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(values))
	for _, v := range base {
		key := strings.TrimSpace(v)
		if key == "" {
			continue
		}
		seen[key] = struct{}{}
	}
	for _, v := range values {
		key := strings.TrimSpace(v)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		base = append(base, key)
		seen[key] = struct{}{}
	}
	return base
}

func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return content
	}
	return strings.TrimSpace(parts[2])
}

// --- Memory Store ---

type MemoryStore struct {
	workspace     string
	consolidateMu sync.Map // per-session consolidation lock: sessionKey -> *sync.Mutex
}

func newMemoryStore(workspace string) *MemoryStore {
	if err := ensureDir(filepath.Join(workspace, "memory")); err != nil {
		slog.Warn("Failed to create memory directory", "err", err)
	}
	return &MemoryStore{workspace: workspace}
}

func (ms *MemoryStore) memoryPath() string {
	return filepath.Join(ms.workspace, "memory", "MEMORY.md")
}

func (ms *MemoryStore) historyPath() string {
	return filepath.Join(ms.workspace, "memory", "HISTORY.md")
}

func (ms *MemoryStore) readMemory() string {
	data, err := os.ReadFile(ms.memoryPath())
	if err != nil {
		return ""
	}
	return string(data)
}

func (ms *MemoryStore) writeMemory(content string) error {
	return os.WriteFile(ms.memoryPath(), []byte(content), 0644)
}

func (ms *MemoryStore) appendHistory(entry string) error {
	f, err := os.OpenFile(ms.historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n\n", strings.TrimRight(entry, "\n\r\t "))
	return err
}

func (ms *MemoryStore) getContext() string {
	mem := ms.readMemory()
	if mem == "" {
		return ""
	}
	return "## Long-term Memory\n" + mem
}

// consolidate triggers LLM-driven memory consolidation.
// It preserves the most recent keepCount messages for continuity.
// When memoryWindow is 0, all unconsolidated messages are consolidated (used by /new).
func (ms *MemoryStore) consolidate(ctx context.Context, llm *LLMClient, session *Session, maxTokens, memoryWindow int) bool {
	// Per-session lock prevents concurrent consolidations on the same session.
	mu, _ := ms.consolidateMu.LoadOrStore(session.Key, &sync.Mutex{})
	sessionMu := mu.(*sync.Mutex)
	sessionMu.Lock()
	defer sessionMu.Unlock()

	keepCount := 0
	if memoryWindow > 0 {
		keepCount = memoryWindow / 2
	}

	session.mu.Lock()
	total := len(session.Messages)
	consolidated := session.LastConsolidated
	session.mu.Unlock()

	unconsolidated := total - consolidated
	if unconsolidated == 0 || (keepCount > 0 && unconsolidated <= keepCount) {
		return true // not enough to consolidate
	}

	// Only consolidate the old messages, preserving the most recent keepCount.
	endIdx := total - keepCount
	session.mu.Lock()
	oldMsgs := append([]SessionMessage(nil), session.Messages[consolidated:endIdx]...)
	session.mu.Unlock()

	if len(oldMsgs) == 0 {
		return true
	}

	var historyText strings.Builder
	for _, m := range oldMsgs {
		content, _ := m.Content.(string)
		if content == "" {
			continue
		}
		ts := m.Ts
		if ts == "" {
			ts = "?"
		} else if len(ts) > 16 {
			ts = ts[:16]
		}
		fmt.Fprintf(&historyText, "[%s] %s: %s\n", ts, strings.ToUpper(m.Role), content)
	}

	currentMemory := ms.readMemory()
	if currentMemory == "" {
		currentMemory = "(empty)"
	}
	prompt := fmt.Sprintf(`Process this conversation and call the save_memory tool with your consolidation.

## Current Long-term Memory
%s

## Conversation to Process
%s`, currentMemory, historyText.String())

	saveMemoryTool := ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        "save_memory",
			Description: "Save the memory consolidation result to persistent storage.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"history_entry": map[string]any{
						"type":        "string",
						"description": "A paragraph (2-5 sentences) summarizing key events/decisions/topics. Start with [YYYY-MM-DD HH:MM]. Include detail useful for grep search.",
					},
					"memory_update": map[string]any{
						"type":        "string",
						"description": "Full updated long-term memory as markdown. Include all existing facts plus new ones. Return unchanged if nothing new.",
					},
				},
				"required": []string{"history_entry", "memory_update"},
			},
		},
	}

	messages := []ChatMessage{
		{Role: "system", Content: "You are a memory consolidation agent. Call the save_memory tool with your consolidation of the conversation."},
		{Role: "user", Content: prompt},
	}

	resp, err := llm.chat(ctx, messages, []ToolDef{saveMemoryTool}, maxTokens, 0.3, "")
	if err != nil {
		slog.Error("Memory consolidation failed", "err", err)
		return false
	}

	for _, tc := range resp.ToolCalls {
		if tc.Function.Name == "save_memory" {
			var args struct {
				MemoryUpdate string `json:"memory_update"`
				HistoryEntry string `json:"history_entry"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				slog.Warn("Failed to parse save_memory arguments", "err", err)
				continue
			}
			if args.MemoryUpdate != "" {
				if err := ms.writeMemory(args.MemoryUpdate); err != nil {
					slog.Warn("Failed to write memory", "err", err)
				}
			}
			if args.HistoryEntry != "" {
				if err := ms.appendHistory(args.HistoryEntry); err != nil {
					slog.Warn("Failed to append history", "err", err)
				}
			}
			session.mu.Lock()
			session.LastConsolidated = endIdx
			session.mu.Unlock()
			slog.Info("Memory consolidated", "session", session.Key)
			return true
		}
	}
	return false
}

// --- Context Builder ---

type ContextBuilder struct {
	workspace string
	memory    *MemoryStore
	skills    *SkillsLoader
	model     string
}

func newContextBuilder(workspace string, memory *MemoryStore, skills *SkillsLoader, model string) *ContextBuilder {
	return &ContextBuilder{workspace: workspace, memory: memory, skills: skills, model: model}
}

func (cb *ContextBuilder) buildSystemPrompt() string {
	var parts []string

	// Identity section (matching nanobot's structured format)
	workspacePath := cb.workspace
	if absPath, err := filepath.Abs(cb.workspace); err == nil {
		workspacePath = absPath
	}
	goRuntime := fmt.Sprintf("%s %s, Go %s",
		func() string {
			if runtime.GOOS == "darwin" {
				return "macOS"
			}
			s := runtime.GOOS
			if len(s) > 0 {
				return strings.ToUpper(s[:1]) + s[1:]
			}
			return s
		}(),
		runtime.GOARCH, runtime.Version())
	modelName := strings.TrimSpace(cb.model)
	if modelName == "" {
		modelName = "(unknown)"
	}
	modelIdentity := fmt.Sprintf(`## Model Identity
- This Cody instance runs on: %s
- If asked what model you are using, answer with this exact model family and do not claim GPT-4.`, modelName)
	if isManagedGPTOSSModel(modelName) {
		modelIdentity = `## Model Identity
- This Cody instance runs on GPT OSS 120B.
- If asked what model you are using, answer: "gpt-oss-120b".
- Provider-specific IDs:
  - Groq: openai/gpt-oss-120b
  - Cerebras: gpt-oss-120b
  - OpenRouter: openai/gpt-oss-120b:free
- Never claim you are GPT-4 or ChatGPT-4.`
	}

	identity := fmt.Sprintf(`# Cody 🦔

You are Cody, a helpful AI assistant.

## Runtime
%s

## Workspace
Your workspace is at: %s
- Long-term memory: %s/memory/MEMORY.md (write important facts here)
- History log: %s/memory/HISTORY.md (grep-searchable). Each entry starts with [YYYY-MM-DD HH:MM].
- Custom skills: %s/skills/{skill-name}/SKILL.md

%s

## Cody Guidelines
- State intent before tool calls, but NEVER predict or claim results before receiving them.
- Before modifying a file, read it first. Do not assume files or directories exist.
- After writing or editing a file, re-read it if accuracy matters.
- If a tool call fails, analyze the error before retrying with a different approach.
- Ask for clarification when the request is ambiguous.

Reply directly with text for conversations. Only use the 'message' tool to send to a specific chat channel.`,
		goRuntime, workspacePath, workspacePath, workspacePath, workspacePath, modelIdentity)
	parts = append(parts, identity)

	// Load templates from workspace (falling back to embedded)
	var bootstrapParts []string
	for _, name := range []string{"AGENTS.md", "SOUL.md", "USER.md", "TOOLS.md", "IDENTITY.md"} {
		if content := cb.loadTemplate(name); content != "" {
			bootstrapParts = append(bootstrapParts, fmt.Sprintf("## %s\n\n%s", name, content))
		}
	}
	if len(bootstrapParts) > 0 {
		parts = append(parts, strings.Join(bootstrapParts, "\n\n"))
	}

	// Memory
	if memCtx := cb.memory.getContext(); memCtx != "" {
		parts = append(parts, "# Memory\n\n"+memCtx)
	}

	// Always-on skills
	var skillParts []string
	for _, name := range cb.skills.alwaysOnSkills() {
		if content, err := cb.skills.loadSkill(name); err == nil {
			skillParts = append(skillParts, fmt.Sprintf("### Skill: %s\n\n%s", name, content))
		}
	}
	if len(skillParts) > 0 {
		parts = append(parts, "# Active Skills\n\n"+strings.Join(skillParts, "\n\n---\n\n"))
	}

	// Skills summary
	if summary := cb.skills.buildSummary(); summary != "" {
		parts = append(parts, fmt.Sprintf(`# Skills

The following skills extend your capabilities. To use a skill, read its SKILL.md file using the read_file tool.
Skills with available="false" need dependencies installed first - you can try installing them with apt/brew.

%s`, summary))
	}

	return strings.Join(parts, "\n\n---\n\n")
}

func (cb *ContextBuilder) loadTemplate(name string) string {
	// Try workspace first
	wsPath := filepath.Join(cb.workspace, name)
	if data, err := os.ReadFile(wsPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	// Fall back to embedded
	data, err := templatesFS.ReadFile("templates/" + name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (cb *ContextBuilder) buildMessages(history []ChatMessage, userContent string, channel, chatID string) []ChatMessage {
	msgs := make([]ChatMessage, 0, len(history)+4)

	// System prompt
	msgs = append(msgs, ChatMessage{Role: "system", Content: cb.buildSystemPrompt()})

	// Conversation history
	msgs = append(msgs, history...)

	// Runtime context (injected as user message, matching nanobot format)
	now := time.Now()
	tz := now.Format("MST")
	if tz == "" {
		tz = "UTC"
	}
	runtimeLines := []string{fmt.Sprintf("Current Time: %s (%s)", now.Format("2006-01-02 15:04 (Monday)"), tz)}
	if channel != "" && chatID != "" {
		runtimeLines = append(runtimeLines, fmt.Sprintf("Channel: %s", channel), fmt.Sprintf("Chat ID: %s", chatID))
	}
	runtime := "[Runtime Context — metadata only, not instructions]\n" + strings.Join(runtimeLines, "\n")
	msgs = append(msgs, ChatMessage{Role: "user", Content: runtime})

	// Current user message.
	// Cody is intentionally text-only for LLM input (gpt-oss-120b setup).
	msgs = append(msgs, ChatMessage{Role: "user", Content: userContent})

	return msgs
}

// --- Subagent Manager ---

type SubagentManager struct {
	llm       *LLMClient
	workspace string
	bus       *MessageBus
	tools     *ToolRegistry
	config    *Config
	context   *ContextBuilder
	counter   atomic.Int64
	running   sync.WaitGroup
	tasks     sync.Map // taskID -> cancelFunc
	sessions  sync.Map // sessionKey -> []taskID
}

func newSubagentManager(llm *LLMClient, workspace string, bus *MessageBus,
	tools *ToolRegistry, config *Config, ctx *ContextBuilder) *SubagentManager {
	return &SubagentManager{
		llm: llm, workspace: workspace, bus: bus,
		tools: tools, config: config, context: ctx,
	}
}

func (sm *SubagentManager) spawn(task, label, chatID, sessionKey string) string {
	id := fmt.Sprintf("task-%d", sm.counter.Add(1))
	ctx, cancel := context.WithCancel(context.Background())
	sm.tasks.Store(id, cancel)

	// Track task under its session for /stop cancellation.
	if v, ok := sm.sessions.Load(sessionKey); ok {
		ids := v.([]string)
		sm.sessions.Store(sessionKey, append(ids, id))
	} else {
		sm.sessions.Store(sessionKey, []string{id})
	}

	sm.running.Add(1)
	go sm.run(ctx, id, task, label, chatID, sessionKey)
	return id
}

func (sm *SubagentManager) run(ctx context.Context, taskID, task, label, chatID, sessionKey string) {
	defer sm.running.Done()
	defer sm.tasks.Delete(taskID)

	systemPrompt := sm.buildSubagentPrompt()

	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: task},
	}

	// Subagent uses all tools except spawn and message (no recursive spawning,
	// no direct messaging — results are delivered by the subagent manager).
	tools := sm.tools.schemasExcluding("spawn", "message")
	maxIter := 15
	var lastContent string

	for i := range maxIter {
		resp, err := sm.llm.chat(ctx, messages, tools, sm.config.Agent.MaxTokens, sm.config.Agent.Temperature, sm.config.Agent.ReasoningEffort)
		if err != nil {
			slog.Error("Subagent LLM error", "task", taskID, "err", err)
			lastContent = "Error: " + err.Error()
			break
		}

		if len(resp.ToolCalls) > 0 {
			messages = append(messages, ChatMessage{Role: "assistant", Content: nil, ToolCalls: resp.ToolCalls})
			for _, tc := range resp.ToolCalls {
				result := sm.tools.execute(ctx, tc.Function.Name, tc.Function.Arguments)
				messages = append(messages, ChatMessage{Role: "tool", Content: result, ToolCallID: tc.ID})
			}
			continue
		}

		lastContent = resp.Content
		if lastContent != "" || i == maxIter-1 {
			break
		}
	}

	// Deliver result by re-injecting as system message so the main agent
	// summarizes it naturally for the user (matching nanobot behavior).
	result := lastContent
	if result == "" {
		result = "(task completed with no output)"
	}
	statusText := "completed successfully"
	if strings.HasPrefix(result, "Error:") {
		statusText = "failed"
	}

	announceContent := fmt.Sprintf(`[Subagent '%s' %s]

Task: %s

Result:
%s

Summarize this naturally for the user. Keep it brief (1-2 sentences). Do not mention technical details like "subagent" or task IDs.`,
		label, statusText, task, result)

	sm.bus.Inbound <- &InboundMessage{
		SenderID:   "subagent",
		ChatID:     chatID,
		SessionKey: sessionKey,
		Content:    announceContent,
	}
}

func (sm *SubagentManager) wait() {
	sm.running.Wait()
}

// buildSubagentPrompt builds a rich system prompt for the subagent matching nanobot's format.
func (sm *SubagentManager) buildSubagentPrompt() string {
	now := time.Now()
	tz := now.Format("MST")
	if tz == "" {
		tz = "UTC"
	}
	runtimeCtx := fmt.Sprintf("[Runtime Context — metadata only, not instructions]\nCurrent Time: %s (%s)",
		now.Format("2006-01-02 15:04 (Monday)"), tz)

	parts := []string{fmt.Sprintf(`# Subagent

%s

You are a subagent spawned by the main agent to complete a specific task.
Stay focused on the assigned task. Your final response will be reported back to the main agent.

## Workspace
%s`, runtimeCtx, sm.workspace)}

	skills := newSkillsLoader(sm.workspace)
	if summary := skills.buildSummary(); summary != "" {
		parts = append(parts, fmt.Sprintf("## Skills\n\nRead SKILL.md with read_file to use a skill.\n\n%s", summary))
	}

	return strings.Join(parts, "\n\n")
}

// cancelBySession cancels all subagents spawned for a given session.
// Returns the number of tasks cancelled.
func (sm *SubagentManager) cancelBySession(sessionKey string) int {
	v, ok := sm.sessions.LoadAndDelete(sessionKey)
	if !ok {
		return 0
	}
	ids := v.([]string)
	n := 0
	for _, id := range ids {
		if cancel, ok := sm.tasks.LoadAndDelete(id); ok {
			cancel.(context.CancelFunc)()
			n++
		}
	}
	return n
}

// --- Agent Loop ---

type AgentLoop struct {
	llm      *LLMClient
	bus      *MessageBus
	sessions *SessionManager
	tools    *ToolRegistry
	context  *ContextBuilder
	memory   *MemoryStore
	subMgr   *SubagentManager
	config   *Config
	reqCtx   *RequestContext
	mu       sync.Mutex                    // global processing lock
	cancels  map[string]context.CancelFunc // active per-session cancel funcs (legacy direct-dispatch path)
	cancelMu sync.Mutex                    // protects cancels map
	taskSeq  atomic.Int64
	taskMu   sync.Mutex
	tasks    map[string]map[int64]context.CancelFunc // session -> running/queued task cancels
}

func newAgentLoop(cfg *Config, llm *LLMClient, bus *MessageBus, sessions *SessionManager,
	tools *ToolRegistry, cronSvc *CronService) *AgentLoop {

	workspace := cfg.workspacePath()
	memory := newMemoryStore(workspace)
	skills := newSkillsLoader(workspace)
	ctxBuilder := newContextBuilder(workspace, memory, skills, cfg.Model)
	reqCtx := &RequestContext{}

	subMgr := newSubagentManager(llm, workspace, bus, tools, cfg, ctxBuilder)

	registerDefaultTools(tools, cfg, workspace, bus, reqCtx, cronSvc, subMgr)

	return &AgentLoop{
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
		tasks:    make(map[string]map[int64]context.CancelFunc),
	}
}

func (a *AgentLoop) run(ctx context.Context) {
	slog.Info("Agent loop started")
	for {
		select {
		case <-ctx.Done():
			slog.Info("Agent loop stopping")
			a.subMgr.wait()
			return
		case msg := <-a.bus.Inbound:
			if strings.TrimSpace(msg.Content) == "/stop" {
				a.handleStop(msg)
				continue
			}

			taskCtx, cancel := context.WithCancel(ctx)
			taskID := a.registerTask(msg.SessionKey, cancel)

			go func(m *InboundMessage, c context.Context, id int64) {
				defer a.unregisterTask(m.SessionKey, id)
				a.dispatch(c, m)
			}(msg, taskCtx, taskID)
		}
	}
}

func (a *AgentLoop) dispatch(ctx context.Context, msg *InboundMessage) {
	content := strings.TrimSpace(msg.Content)

	// Handle /stop outside the processing lock so it can cancel an active request.
	if content == "/stop" {
		a.handleStop(msg)
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Request may have been cancelled while waiting for the global lock.
	if ctx.Err() != nil {
		return
	}

	// Set per-request context
	a.reqCtx.ChatID = msg.ChatID
	a.reqCtx.SessionKey = msg.SessionKey
	a.reqCtx.MessageID = msg.MessageID
	a.reqCtx.MessageSent = false

	// Handle /new — consolidate memory and clear session.
	if content == "/new" {
		a.handleNew(ctx, msg)
		return
	}

	// Create a cancellable context for this request.
	reqCtx, cancel := context.WithCancel(ctx)
	a.cancelMu.Lock()
	a.cancels[msg.SessionKey] = cancel
	a.cancelMu.Unlock()
	defer func() {
		cancel()
		a.cancelMu.Lock()
		delete(a.cancels, msg.SessionKey)
		a.cancelMu.Unlock()
	}()

	response, err := a.processMessage(reqCtx, msg)
	if err != nil {
		if errors.Is(reqCtx.Err(), context.Canceled) {
			slog.Info("Task cancelled", "session", msg.SessionKey)
			return
		}
		slog.Error("Message processing failed", "session", msg.SessionKey, "err", err)
		a.bus.Outbound <- &OutboundMessage{
			ChatID:           msg.ChatID,
			Content:          "Sorry, I encountered an error.",
			ReplyToMessageID: msg.MessageID,
		}
		return
	}

	if errors.Is(reqCtx.Err(), context.Canceled) {
		slog.Info("Task cancelled", "session", msg.SessionKey)
		return
	}

	if response != "" && !a.reqCtx.MessageSent {
		a.bus.Outbound <- &OutboundMessage{
			ChatID:           msg.ChatID,
			Content:          response,
			ReplyToMessageID: msg.MessageID,
		}
	}
}

func (a *AgentLoop) registerTask(sessionKey string, cancel context.CancelFunc) int64 {
	id := a.taskSeq.Add(1)
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	if a.tasks == nil {
		a.tasks = make(map[string]map[int64]context.CancelFunc)
	}
	if a.tasks[sessionKey] == nil {
		a.tasks[sessionKey] = make(map[int64]context.CancelFunc)
	}
	a.tasks[sessionKey][id] = cancel
	return id
}

func (a *AgentLoop) unregisterTask(sessionKey string, taskID int64) {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	if a.tasks == nil {
		return
	}
	tm := a.tasks[sessionKey]
	if tm == nil {
		return
	}
	delete(tm, taskID)
	if len(tm) == 0 {
		delete(a.tasks, sessionKey)
	}
}

func (a *AgentLoop) handleStop(msg *InboundMessage) {
	n := a.cancelSession(msg.SessionKey)
	n += a.subMgr.cancelBySession(msg.SessionKey)
	stopMsg := "No active task to stop."
	if n > 0 {
		stopMsg = fmt.Sprintf("⏹ Stopped %d task(s).", n)
	}
	a.bus.Outbound <- &OutboundMessage{
		ChatID:           msg.ChatID,
		Content:          stopMsg,
		ReplyToMessageID: msg.MessageID,
	}
}

// cancelSession cancels the active agent loop for the given session key.
// Returns 1 if a task was cancelled, 0 otherwise.
func (a *AgentLoop) cancelSession(sessionKey string) int {
	a.taskMu.Lock()
	taskCancels := map[int64]context.CancelFunc(nil)
	if a.tasks != nil {
		taskCancels = a.tasks[sessionKey]
		delete(a.tasks, sessionKey)
	}
	a.taskMu.Unlock()

	n := 0
	for _, cancel := range taskCancels {
		cancel()
		n++
	}

	a.cancelMu.Lock()
	cancel, ok := a.cancels[sessionKey]
	a.cancelMu.Unlock()
	if ok {
		cancel()
		if n == 0 {
			n = 1
		}
	}
	return n
}

// handleNew consolidates memory and clears the session.
func (a *AgentLoop) handleNew(ctx context.Context, msg *InboundMessage) {
	session := a.sessions.getOrCreate(msg.SessionKey)

	if session.unconsolidatedCount() > 0 {
		if !a.memory.consolidate(ctx, a.llm, session, a.config.Agent.MaxTokens, 0) {
			a.bus.Outbound <- &OutboundMessage{
				ChatID:           msg.ChatID,
				Content:          "Memory archival failed, session not cleared. Please try again.",
				ReplyToMessageID: msg.MessageID,
			}
			return
		}
	}

	session.clear()
	if err := a.sessions.save(session); err != nil {
		slog.Error("Session clear failed", "err", err)
	}

	a.bus.Outbound <- &OutboundMessage{
		ChatID:           msg.ChatID,
		Content:          "New session started.",
		ReplyToMessageID: msg.MessageID,
	}
}

func (a *AgentLoop) processMessage(ctx context.Context, msg *InboundMessage) (string, error) {
	session := a.sessions.getOrCreate(msg.SessionKey)

	// Save user message to session.
	session.addMessage("user", msg.Content, nil, "", "")

	// Build context: use history BEFORE the message we just added (it's included separately)
	allHistory := session.getHistory(a.config.Agent.MemoryWindow)
	// Exclude the last message (the one we just added) since buildMessages appends it
	history := allHistory
	if len(history) > 0 {
		history = history[:len(history)-1]
	}
	messages := a.context.buildMessages(history, msg.Content, "telegram", msg.ChatID)

	// Progress callback sends updates to user during processing
	onProgress := func(content string, toolHint bool) {
		a.bus.Outbound <- &OutboundMessage{
			ChatID:     msg.ChatID,
			Content:    content,
			IsProgress: true,
			IsToolHint: toolHint,
		}
	}

	// Run agent loop
	content, newMessages := a.runLoop(ctx, messages, onProgress)

	// Save assistant response to session unless it is an LLM/provider error response.
	if shouldPersistAssistant(content) {
		session.addMessage("assistant", content, nil, "", "")
	}

	// Save tool interactions from this turn only (newMessages beyond what we sent in)
	startIdx := len(messages)
	for _, m := range newMessages[startIdx:] {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				session.addMessage("assistant", m.Content, m.ToolCalls, "", "")
			}
		case "tool":
			result, _ := m.Content.(string)
			// Truncate tool results to avoid bloating session history.
			const toolResultMaxChars = 500
			if len(result) > toolResultMaxChars {
				result = result[:toolResultMaxChars] + "… (truncated)"
			}
			session.addMessage("tool", result, nil, m.ToolCallID, m.Name)
		}
	}

	// Persist session
	if err := a.sessions.save(session); err != nil {
		slog.Error("Session save failed", "err", err)
	}

	// Memory consolidation check
	if session.unconsolidatedCount() >= a.config.Agent.MemoryWindow {
		go a.memory.consolidate(context.Background(), a.llm, session, a.config.Agent.MaxTokens, a.config.Agent.MemoryWindow)
	}

	return content, nil
}

// progressFunc is called with intermediate updates during the agent loop.
type progressFunc func(content string, toolHint bool)

const llmUnavailableMessage = "Sorry, I couldn't complete that request right now. Please try again in a moment."

func (a *AgentLoop) runLoop(ctx context.Context, messages []ChatMessage, onProgress progressFunc) (string, []ChatMessage) {
	tools := a.tools.schemas()
	maxIter := a.config.Agent.MaxIterations

	for i := range maxIter {
		resp, err := a.llm.chat(ctx, messages, tools, a.config.Agent.MaxTokens, a.config.Agent.Temperature, a.config.Agent.ReasoningEffort)
		if err != nil {
			slog.Error("LLM call failed", "iteration", i, "err", err)
			return llmUnavailableMessage, messages
		}

		slog.Info("LLM response",
			"iteration", i,
			"has_tools", len(resp.ToolCalls) > 0,
			"tokens", resp.Usage.TotalTokens)

		// Don't persist error responses — they can poison the context and cause
		// permanent 400 loops (matching nanobot #1303 fix).
		if resp.FinishReason == "error" {
			slog.Error("LLM returned error finish_reason", "iteration", i)
			return llmUnavailableMessage, messages
		}

		if len(resp.ToolCalls) == 0 {
			return stripThinkBlocks(resp.Content), messages
		}

		// Send thinking content as progress, then tool hints (matching nanobot order)
		if onProgress != nil {
			clean := stripThinkBlocks(resp.Content)
			if clean != "" {
				onProgress(clean, false)
			}
			onProgress(formatToolHints(resp.ToolCalls), true)
		}

		// Append assistant message with tool calls
		messages = append(messages, ChatMessage{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute each tool call
		for _, tc := range resp.ToolCalls {
			slog.Info("Executing tool", "name", tc.Function.Name, "id", tc.ID)

			result := a.tools.execute(ctx, tc.Function.Name, tc.Function.Arguments)
			messages = append(messages, ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}

	return fmt.Sprintf("I reached the maximum number of tool call iterations (%d) without completing the task. You can try breaking the task into smaller steps.", maxIter), messages
}

func shouldPersistAssistant(content string) bool {
	if content == "" {
		return false
	}
	if strings.HasPrefix(content, "I encountered an error:") {
		return false
	}
	if content == llmUnavailableMessage {
		return false
	}
	return true
}

// stripThinkBlocks removes <think>...</think> blocks from LLM output.
// Some models emit reasoning in think blocks that shouldn't be shown to users.
var thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

func stripThinkBlocks(content string) string {
	return strings.TrimSpace(thinkBlockRe.ReplaceAllString(content, ""))
}

// formatToolHint produces a short description of a tool call for progress updates.
// Matches nanobot format: name("first_arg_value") or just name if no string arg.
func formatToolHint(tc ToolCall) string {
	var params map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &params); err == nil {
		for _, v := range params {
			if s, ok := v.(string); ok {
				if len(s) > 40 {
					return fmt.Sprintf("%s(\"%s…\")", tc.Function.Name, s[:40])
				}
				return fmt.Sprintf("%s(\"%s\")", tc.Function.Name, s)
			}
			break
		}
	}
	return tc.Function.Name
}

// formatToolHints formats all tool calls as a combined hint string.
func formatToolHints(tcs []ToolCall) string {
	hints := make([]string, len(tcs))
	for i, tc := range tcs {
		hints[i] = formatToolHint(tc)
	}
	return strings.Join(hints, ", ")
}

// ProcessDirect runs a message through the agent without the message bus (used by cron/heartbeat).
func (a *AgentLoop) processDirect(ctx context.Context, content, sessionKey, chatID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.reqCtx.ChatID = chatID
	a.reqCtx.SessionKey = sessionKey
	a.reqCtx.MessageID = 0
	a.reqCtx.MessageSent = false

	session := a.sessions.getOrCreate(sessionKey)
	session.addMessage("user", content, nil, "", "")

	allHistory := session.getHistory(a.config.Agent.MemoryWindow)
	history := allHistory
	if len(history) > 0 {
		history = history[:len(history)-1]
	}
	channel := ""
	if chatID != "" {
		channel = "telegram"
	}
	messages := a.context.buildMessages(history, content, channel, chatID)

	result, newMessages := a.runLoop(ctx, messages, nil)
	if shouldPersistAssistant(result) {
		session.addMessage("assistant", result, nil, "", "")
	}

	// Save tool interactions from this turn.
	startIdx := len(messages)
	for _, m := range newMessages[startIdx:] {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				session.addMessage("assistant", m.Content, m.ToolCalls, "", "")
			}
		case "tool":
			toolResult, _ := m.Content.(string)
			const toolResultMaxChars = 500
			if len(toolResult) > toolResultMaxChars {
				toolResult = toolResult[:toolResultMaxChars] + "… (truncated)"
			}
			session.addMessage("tool", toolResult, nil, m.ToolCallID, m.Name)
		}
	}
	if err := a.sessions.save(session); err != nil {
		slog.Error("Session save failed", "err", err)
	}

	if a.reqCtx.MessageSent {
		return ""
	}

	return result
}
