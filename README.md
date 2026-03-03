# Cody

<img src="./cody.svg" alt="Cody pangolin mascot" width="220" />

Cody is your personal AI assistant on Telegram.

You message it like a teammate, and it can:

- remember long-term context about your work,
- run shell commands and edit files,
- search/fetch the web,
- schedule reminders and background jobs,
- handle voice and media.

Everything runs from a single self-hosted Go binary, with local plain-text storage you can inspect and edit.

## Why People Like Cody

- **Simple architecture**: one binary, one codebase, one place to debug.
- **Telegram-first**: optimized for chat-based workflows.
- **Practical tooling**: file ops, shell, web, scheduling, and subagents out of the box.
- **Local-first memory**: stored as readable markdown and JSONL files.
- **Easy to customize**: personality, instructions, and user profile are editable files.

## Quick Start

### 1. Prerequisites

- Go 1.25+
- Telegram bot token (from [@BotFather](https://t.me/BotFather))
- API key for an OpenAI-compatible endpoint (default target: Cerebras)
- Optional: Groq API key (voice transcription)
- Optional: Brave Search API key (web search)

### 2. Build

```bash
git clone <this-repo> && cd cody
go build -o cody .
```

### 3. Onboard

```bash
./cody onboard
```

This creates your Cody home at `~/.cody/`, including:

- `config.json`
- `workspace/` with templates and skills

### 4. Configure

Edit `~/.cody/config.json`:

```json
{
  "api_key": "",
  "cerebras": {
    "api_key": "csk_your-cerebras-key"
  },
  "api_base": "https://api.cerebras.ai/v1",
  "model": "gpt-oss-120b",
  "telegram": {
    "token": "123456:ABC-your-bot-token",
    "allow_from": ["your_telegram_user_id"],
    "reply_to_message": false,
    "send_progress": true,
    "send_tool_hints": false
  },
  "groq": {
    "api_key": "gsk_your-groq-key"
  },
  "tools": {
    "web_search_api_key": "your-brave-api-key"
  }
}
```

You can keep secrets in environment variables instead:

```bash
export CEREBRAS_API_KEY="your-cerebras-key"
export TELEGRAM_BOT_TOKEN="123456:ABC-your-bot-token"
```

Cody checks these env vars when config fields are empty:

- `CODY_API_KEY`, `CEREBRAS_API_KEY`, `OPENAI_API_KEY`
- `CODY_API_BASE`, `OPENAI_API_BASE`
- `CODY_MODEL`
- `TELEGRAM_BOT_TOKEN`
- `GROQ_API_KEY`
- `BRAVE_API_KEY`

To get your Telegram user ID, message [@userinfobot](https://t.me/userinfobot).

### 5. Run

```bash
./cody
```

Or with Docker Compose:

```bash
docker compose up -d
```

## What Cody Can Do

### Conversation + memory

Cody keeps two memory layers in `~/.cody/workspace/memory/`:

- `MEMORY.md`: long-term facts and preferences.
- `HISTORY.md`: append-only timeline with searchable conversation summaries.

Memory consolidation is automatic after enough session activity.

### Built-in tools

Cody ships with 10 built-in tools:

| Tool         | Purpose                                              |
| ------------ | ---------------------------------------------------- |
| `read_file`  | Read files (optionally by line ranges)               |
| `write_file` | Create/overwrite files                               |
| `edit_file`  | Targeted find/replace edits                          |
| `list_dir`   | List directories with depth control                  |
| `exec`       | Run shell commands with safety checks and timeout    |
| `web_search` | Search the web (Brave API)                           |
| `web_fetch`  | Fetch URL content as readable text                   |
| `message`    | Proactively send progress/result messages            |
| `cron`       | Add/list/enable/disable/remove scheduled jobs        |
| `spawn`      | Launch background subagents for longer-running tasks |

### Scheduling

Cody supports:

- `at` schedules for one-time jobs
- `every` schedules for interval jobs
- cron expressions for calendar-style schedules

It also has a heartbeat loop that scans `HEARTBEAT.md` periodically and can trigger tasks automatically.

### Voice + media

- Voice and audio transcription via Groq Whisper (`whisper-large-v3-turbo`)
- Photo/document handling from Telegram
- Media-group buffering so related photos are processed together

### Skills

Skills are markdown instructions loaded from `~/.cody/workspace/skills/<name>/SKILL.md`.

Built-in skills currently include:

- `memory`
- `cron`
- `github`
- `weather`
- `tmux`
- `summarize`
- `clawhub`
- `skill-creator`

You can add your own skills by creating folders with a `SKILL.md` file.

## Personalize Cody

Edit these workspace files to shape behavior:

- `USER.md`: your profile, preferences, context
- `SOUL.md`: personality and communication style
- `AGENTS.md`: behavioral instructions for reminders/automation
- `TOOLS.md`: guidance for tool use
- `HEARTBEAT.md`: periodic checks/tasks

Workspace copies override embedded defaults.

## Architecture

```text
Telegram -> MessageBus -> AgentLoop <-> LLM
                    |            |
                    |            +-> Tool Registry (10 tools)
                    |
                    +-> Session Store (JSONL)
                    +-> Memory Store (MEMORY.md / HISTORY.md)
                    +-> Cron + Heartbeat services
```

Processing flow:

1. Telegram receives an inbound message.
2. Agent loads session + memory + templates + relevant skills.
3. LLM responds with text and/or tool calls.
4. Tools execute in a loop until completion or max iterations.
5. Final response is sent back to Telegram.

## CLI Commands

```text
cody                Start the bot
cody onboard        Initialize config and workspace
cody status         Show configuration status
cody agent          Interactive agent mode
cody agent -m MSG   Send a single message
cody cron           Manage scheduled jobs
cody version        Show version
cody help           Show help
```

## Development

### Local CI

Run the full checks used in CI:

```bash
make ci
```

Go-only checks:

```bash
make ci-go
```

Install helper tooling locally:

```bash
make bootstrap-ci-tools
```

## Configuration Reference

| Key                          | Type     | Default                      | Description                                                            |
| ---------------------------- | -------- | ---------------------------- | ---------------------------------------------------------------------- |
| `api_key`                    | string   | _(or env var)_               | Primary API key (`CODY_API_KEY`, `CEREBRAS_API_KEY`, `OPENAI_API_KEY`) |
| `cerebras.api_key`           | string   | _(optional)_                 | Alternate place for Cerebras key (used when `api_key` is empty)        |
| `api_base`                   | string   | `https://api.cerebras.ai/v1` | OpenAI-compatible API base URL                                         |
| `model`                      | string   | `gpt-oss-120b`               | Model name used for chat completions                                   |
| `workspace`                  | string   | `~/.cody/workspace`          | Workspace root for memory, sessions, templates, and skills             |
| `telegram.token`             | string   | _(required)_                 | Telegram bot token                                                     |
| `telegram.allow_from`        | string[] | `[]`                         | Allowed Telegram sender IDs (empty means allow all)                    |
| `telegram.reply_to_message`  | bool     | `false`                      | Send responses as replies to the triggering message                    |
| `telegram.send_progress`     | bool     | `true`                       | Send progress updates during tool loops                                |
| `telegram.send_tool_hints`   | bool     | `false`                      | Send tool-call hint messages during tool loops                         |
| `groq.api_key`               | string   | _(optional)_                 | Groq API key for voice transcription                                   |
| `tools.web_search_api_key`   | string   | _(optional)_                 | Brave Search API key (or `BRAVE_API_KEY`)                              |
| `tools.exec_timeout`         | int      | `60`                         | Max seconds for `exec` tool command                                    |
| `tools.allowed_dir`          | string   | `""`                         | Optional extra allowed directory for file/shell tools                  |
| `tools.path_append`          | string   | `""`                         | Optional PATH suffix for exec environment                              |
| `heartbeat.enabled`          | bool     | `true`                       | Enable heartbeat scheduler                                             |
| `heartbeat.interval_minutes` | int      | `30`                         | Heartbeat run interval in minutes                                      |
| `agent.max_tokens`           | int      | `8192`                       | Max output tokens per model response                                   |
| `agent.temperature`          | float    | `0.1`                        | Sampling temperature                                                   |
| `agent.max_iterations`       | int      | `40`                         | Max tool-loop iterations per request                                   |
| `agent.memory_window`        | int      | `100`                        | Messages before memory consolidation                                   |
| `agent.reasoning_effort`     | string   | `""`                         | Optional reasoning effort hint for compatible models                   |

## Project Layout

```text
cody/
├── main.go
├── config.go
├── util.go
├── llm.go
├── session.go
├── tools.go
├── agent.go
├── cron.go
├── telegram.go
├── templates/
├── skills/
├── Dockerfile
└── docker-compose.yml
```

## Notes on Design

Cody is a focused fork of nanobot with a narrower surface area:

- Telegram-centered interaction
- OpenAI-compatible API path (no large provider abstraction layer)
- small, readable Go codebase with minimal package complexity

## License

MIT
