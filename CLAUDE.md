# CLAUDE.md — Cody Development Guide

Cody is a personal AI assistant that connects Telegram to an OpenAI-compatible LLM (gpt-oss-120b via OpenRouter). It is a Go source port of [nanobot](https://github.com/nano-bot/nanobot), stripped to only support Telegram + OpenRouter. The entire codebase is a single flat Go package (`package main`) in the repository root.

## Build & Run

```bash
go build .              # produces ./cody binary
./cody                  # starts the bot (requires config)
./cody onboard          # first-time setup wizard
./cody status           # show configuration status
./cody agent -m "msg"   # send a single message
./cody agent            # interactive CLI mode
./cody cron list        # list scheduled jobs
./cody version          # prints version
```

Docker:

```bash
docker build -t cody .
docker run -v ~/.cody:/root/.cody cody
```

## Test Commands

```bash
go test . -count=1 -timeout 120s                           # run all tests
go test -tags testcoverage . -count=1 -coverprofile=c.out   # coverage (excludes main.go)
go test -tags testcoverage -race . -count=1 -timeout 180s   # with race detector
go tool cover -func=c.out                                   # per-function coverage
```

The `-tags testcoverage` build tag excludes `main.go` from coverage measurement since its functions (`main`, `runGateway`, `runOnboard`) call `os.Exit` and start real services, making them untestable. Without the tag, all tests still pass — coverage just reports lower because those functions count as uncovered.

**Current stats:** 319 tests, 90.2% coverage, 0 race conditions.

## Project Layout

```
cody/
├── main.go             # Entry point (build-tagged !testcoverage)
├── agent.go            # Core: AgentLoop, ContextBuilder, MemoryStore, SkillsLoader, SubagentManager
├── config.go           # Config loading/saving/validation, defaults
├── session.go          # Session persistence (JSONL), MessageBus, message types
├── llm.go              # OpenAI-compatible HTTP client with retry logic
├── telegram.go         # Telegram bot: polling, media handling, markdown→HTML
├── tools.go            # ToolRegistry + all 10 built-in tools
├── cron.go             # CronService (scheduled jobs) + HeartbeatService (periodic checks)
├── util.go             # Helpers: ensureDir, syncTemplates, embedded FS
├── *_test.go           # Tests for each source file
├── coverage_test.go    # Additional coverage tests (~120 tests)
├── templates/          # Embedded MD templates (synced to workspace on first run)
│   ├── AGENTS.md       # Instructions for cron, heartbeat, reminders
│   ├── SOUL.md         # Personality and values
│   ├── USER.md         # User profile (editable by user)
│   ├── TOOLS.md        # Tool usage notes
│   ├── HEARTBEAT.md    # Periodic task definitions
│   └── memory/
│       └── MEMORY.md   # Default empty long-term memory
├── skills/             # Embedded default skills (SKILL.md per skill)
├── go.mod / go.sum
├── Dockerfile
└── docker-compose.yml
```

Everything is `package main` in the root. No `cmd/`, no `internal/`, no subpackages.

## Architecture

### Message Flow

```
Telegram → TelegramBot.handleMessage() → MessageBus.Inbound
  → AgentLoop.run() → dispatch() → processMessage()
    → ContextBuilder.buildMessages() (system prompt + history + user msg)
    → AgentLoop.runLoop() (up to 40 LLM iterations)
      → LLMClient.chat() → OpenRouter API
      → if tool_calls: ToolRegistry.execute() → loop again
      → if text: return response
    → save session, check memory consolidation
  → MessageBus.Outbound
→ TelegramBot.dispatchOutbound() → Telegram API
```

### Key Types

| Type                         | File        | Purpose                                                        |
| ---------------------------- | ----------- | -------------------------------------------------------------- |
| `Config`                     | config.go   | All settings (API, Telegram, agent behavior, tools, heartbeat) |
| `AgentLoop`                  | agent.go    | Main message processor, owns the agent iteration loop          |
| `ContextBuilder`             | agent.go    | Assembles system prompt from templates + memory + skills       |
| `MemoryStore`                | agent.go    | MEMORY.md (facts) + HISTORY.md (timestamped log)               |
| `SkillsLoader`               | agent.go    | Loads skill definitions from workspace or embedded FS          |
| `SubagentManager`            | agent.go    | Spawns background agent tasks with isolated contexts           |
| `LLMClient`                  | llm.go      | OpenAI-compatible HTTP client, 3 retries, 5min timeout         |
| `TelegramBot`                | telegram.go | Telegram polling, media handling, markdown→HTML conversion     |
| `ToolRegistry`               | tools.go    | Manages 10 built-in tools, JSON schema generation              |
| `Session` / `SessionManager` | session.go  | JSONL conversation persistence, in-memory cache                |
| `MessageBus`                 | session.go  | Channel-based routing (Inbound/Outbound, 64 buffer)            |
| `CronService`                | cron.go     | Timer-based job scheduler (at/every/cron expressions)          |
| `HeartbeatService`           | cron.go     | Periodic LLM check of HEARTBEAT.md tasks                       |

### Built-in Tools

| Tool         | Purpose                                                          |
| ------------ | ---------------------------------------------------------------- |
| `read_file`  | Read file with optional line range                               |
| `write_file` | Create or overwrite file                                         |
| `edit_file`  | Exact string replacement (shows close match on failure)          |
| `list_dir`   | Tree listing (configurable depth, default 2)                     |
| `exec`       | Run bash command (60s timeout, dangerous pattern blocking)       |
| `web_search` | Brave Search API (needs BRAVE_API_KEY)                           |
| `web_fetch`  | Fetch URL, extract text via readability (1MB limit, 5 redirects) |
| `message`    | Send message to user (with optional media attachments)           |
| `cron`       | Manage scheduled jobs (add/remove/list/enable/disable)           |
| `spawn`      | Launch background subagent task (max 15 iterations)              |

## Config

Location: `~/.cody/config.json`

```json
{
  "api_key": "",
  "cerebras": { "api_key": "" },
  "api_base": "",
  "model": "gpt-oss-120b",
  "workspace": "~/.cody/workspace",
  "telegram": {
    "token": "",
    "allow_from": [],
    "reply_to_message": true,
    "send_progress": true,
    "send_tool_hints": false
  },
  "agent": {
    "max_tokens": 8192,
    "temperature": 0.1,
    "max_iterations": 40,
    "memory_window": 100
  },
  "heartbeat": { "enabled": true, "interval_minutes": 30 },
  "tools": { "exec_timeout": 60 },
  "groq": { "api_key": "" }
}
```

Required fields: `api_key` (or `cerebras.api_key`), `api_base`, `telegram.token`.

## Workspace Structure (runtime)

```
~/.cody/workspace/
├── AGENTS.md, SOUL.md, USER.md, TOOLS.md, HEARTBEAT.md  # Editable templates
├── memory/
│   ├── MEMORY.md          # Long-term facts (LLM-managed, user-editable)
│   └── HISTORY.md         # Append-only timestamped log
├── sessions/
│   └── {key}.jsonl        # Conversation history per chat
├── skills/
│   └── {name}/SKILL.md    # Custom skills with YAML frontmatter
└── cron.json              # Scheduled job definitions
```

## Dependencies

| Package                                              | Purpose                            |
| ---------------------------------------------------- | ---------------------------------- |
| `github.com/go-telegram-bot-api/telegram-bot-api/v5` | Telegram bot API                   |
| `github.com/robfig/cron/v3`                          | Cron expression parsing            |
| `gopkg.in/yaml.v3`                                   | YAML parsing for skill frontmatter |
| `github.com/go-shiori/go-readability`                | Web page text extraction           |

No CGO. Single static binary. Go 1.25+.

## Code Conventions

- **Single package, flat layout.** No subpackages, no exported symbols (everything is unexported since it's all `package main`).
- **No interfaces for internal types.** Concrete structs with methods. Interfaces only where Go requires them (embed.FS, io.Reader, etc.).
- **Constructor pattern:** `newXxx()` functions return `*Xxx`. No init functions.
- **Error handling:** Return errors up. Log + continue for non-fatal. `slog` for structured logging.
- **Concurrency:** `sync.Mutex` for shared state. Channels for message passing. `context.Context` for cancellation.
- **Testing:** `httptest.NewServer` for HTTP mocking. `t.TempDir()` for filesystem isolation. Channel-based assertions for async code.
- **No global state.** Everything passed via constructors or method parameters. Config is threaded through, not global.

## Hardcoded Values to Know

| Value                                                 | Location    | What                             |
| ----------------------------------------------------- | ----------- | -------------------------------- |
| `https://api.groq.com/openai/v1/audio/transcriptions` | llm.go      | Groq Whisper endpoint            |
| `https://api.search.brave.com/res/v1/web/search`      | tools.go    | Brave Search endpoint            |
| 5 min                                                 | llm.go      | LLM HTTP timeout                 |
| 4000 chars                                            | telegram.go | Telegram message split threshold |
| 50,000 chars                                          | tools.go    | Tool output max                  |
| 10,000 chars                                          | tools.go    | exec output max                  |
| 600ms                                                 | telegram.go | Media group buffer delay         |
| 15 iterations                                         | agent.go    | Subagent max iterations          |
| 3 retries                                             | llm.go      | LLM retry count                  |

## Memory System

Cody has a two-tier memory system:

1. **Session memory** — conversation history in `sessions/{key}.jsonl`. Kept until explicitly cleared with `/new`.
2. **Long-term memory** — LLM-consolidated facts in `memory/MEMORY.md` + append-only `memory/HISTORY.md`.

When unconsolidated message count exceeds `agent.memory_window` (default 100), the MemoryStore calls the LLM to summarize old messages into MEMORY.md and append a timestamped entry to HISTORY.md. The `/new` command forces immediate consolidation.

## Nanobot Parity

Cody is a behavioral clone of nanobot configured with:

- Channel: Telegram only
- LLM provider: OpenRouter (OpenAI-compatible) with gpt-oss-120b (text-to-text only — no vision/image input)
- All agent harness logic, prompt templates, tool schemas, memory consolidation, cron/heartbeat, and subagent spawning match nanobot's behavior exactly

Differences from nanobot:

- Written in Go instead of Python
- Named "Cody" instead of "nanobot"
- No support for Discord, Slack, or other chat platforms
- No support for Anthropic/Google/other non-OpenAI-compatible APIs
- Single binary deployment (no Python runtime needed)
- No MCP (Model Context Protocol) server integration
- No image/vision input to LLM (gpt-oss-120b is text-only; photos are rejected with a text-only notice, captions are still processed)
- No OAuth-based provider login (OpenAI Codex, GitHub Copilot)
- No gateway HTTP server (nanobot exposes port 18790)
- No LiteLLM routing layer (direct OpenAI-compatible HTTP only)
- No prompt caching optimization (Anthropic/OpenRouter cache_control)
