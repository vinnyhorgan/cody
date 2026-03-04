package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Config struct {
	APIKey     string           `json:"api_key,omitempty"`
	APIBase    string           `json:"api_base"`
	Model      string           `json:"model"`
	Telegram   TelegramConfig   `json:"telegram"`
	Workspace  string           `json:"workspace"`
	Tools      ToolsConfig      `json:"tools"`
	Heartbeat  HeartbeatConfig  `json:"heartbeat"`
	Agent      AgentConfig      `json:"agent"`
	Groq       GroqConfig       `json:"groq"`
	Cerebras   CerebrasConfig   `json:"cerebras"`
	OpenRouter OpenRouterConfig `json:"openrouter"`
}

type TelegramConfig struct {
	Token          string `json:"token"`
	AllowFrom      string `json:"allow_from"`
	ReplyToMessage bool   `json:"reply_to_message"`
	SendProgress   bool   `json:"send_progress"`
	SendToolHints  bool   `json:"send_tool_hints"`
}

type ToolsConfig struct {
	WebSearchAPIKey string `json:"web_search_api_key"`
	ExecTimeout     int    `json:"exec_timeout"`
	AllowedDir      string `json:"allowed_dir"`
	PathAppend      string `json:"path_append"`
}

type HeartbeatConfig struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

type AgentConfig struct {
	MaxTokens       int     `json:"max_tokens"`
	Temperature     float64 `json:"temperature"`
	MaxIterations   int     `json:"max_iterations"`
	MemoryWindow    int     `json:"memory_window"`
	ReasoningEffort string  `json:"reasoning_effort,omitempty"`
}

type GroqConfig struct {
	APIKey string `json:"api_key"`
}

type CerebrasConfig struct {
	APIKey string `json:"api_key"`
}

type OpenRouterConfig struct {
	APIKey string `json:"api_key"`
}

var telegramUsernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{5,32}$`)

func defaultConfig() *Config {
	return &Config{
		APIBase:   "https://api.cerebras.ai/v1",
		Model:     "gpt-oss-120b",
		Workspace: "~/.cody/workspace",
		Telegram: TelegramConfig{
			ReplyToMessage: false,
			SendProgress:   true,
			SendToolHints:  false,
		},
		Tools: ToolsConfig{
			ExecTimeout: 60,
		},
		Heartbeat: HeartbeatConfig{
			Enabled:         true,
			IntervalMinutes: 30,
		},
		Agent: AgentConfig{
			MaxTokens:     8192,
			Temperature:   0.1,
			MaxIterations: 40,
			MemoryWindow:  100,
		},
	}
}

func codyDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".cody"
	}
	return filepath.Join(home, ".cody")
}

func configPath() string {
	return filepath.Join(codyDir(), "config.json")
}

func loadConfig() (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			cfg.applyEnvFallbacks()
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyEnvFallbacks()
	return cfg, nil
}

func (c *Config) applyEnvFallbacks() {
	if strings.TrimSpace(c.Cerebras.APIKey) == "" {
		c.Cerebras.APIKey = strings.TrimSpace(c.APIKey)
	}
	if strings.TrimSpace(c.Cerebras.APIKey) == "" {
		c.Cerebras.APIKey = firstNonEmptyEnv("CEREBRAS_API_KEY", "CODY_API_KEY", "OPENAI_API_KEY")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		c.APIKey = strings.TrimSpace(c.Cerebras.APIKey)
	}
	if strings.TrimSpace(c.APIBase) == "" {
		c.APIBase = firstNonEmptyEnv("CODY_API_BASE", "OPENAI_API_BASE")
	}
	if strings.TrimSpace(c.Model) == "" {
		c.Model = firstNonEmptyEnv("CODY_MODEL")
	}
	if strings.TrimSpace(c.Telegram.Token) == "" {
		c.Telegram.Token = firstNonEmptyEnv("TELEGRAM_BOT_TOKEN")
	}
	if strings.TrimSpace(c.Telegram.AllowFrom) == "" {
		c.Telegram.AllowFrom = firstNonEmptyEnv("TELEGRAM_ALLOW_FROM")
	}
	if strings.TrimSpace(c.Groq.APIKey) == "" {
		c.Groq.APIKey = firstNonEmptyEnv("GROQ_API_KEY")
	}
	if strings.TrimSpace(c.OpenRouter.APIKey) == "" {
		c.OpenRouter.APIKey = firstNonEmptyEnv("OPENROUTER_API_KEY", "OPEN_ROUTER_API_KEY")
	}
	if strings.TrimSpace(c.Tools.WebSearchAPIKey) == "" {
		c.Tools.WebSearchAPIKey = firstNonEmptyEnv("BRAVE_API_KEY")
	}
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		value := strings.TrimSpace(os.Getenv(key))
		if value != "" {
			return value
		}
	}
	return ""
}

func (c *Config) workspacePath() string {
	return expandHome(c.Workspace)
}

func (c *Config) validate() error {
	if isManagedGPTOSSModel(c.Model) {
		if err := c.validateManagedGPTOSSProviders(); err != nil {
			return err
		}
	} else if c.cerebrasAPIKey() == "" {
		return fmt.Errorf("api_key (or cerebras.api_key) is required")
	}
	if !isManagedGPTOSSModel(c.Model) && strings.TrimSpace(c.APIBase) == "" {
		return fmt.Errorf("api_base is required")
	}
	if strings.TrimSpace(c.Telegram.Token) == "" {
		return fmt.Errorf("telegram.token is required")
	}
	allowed := normalizeTelegramUsername(c.Telegram.AllowFrom)
	if allowed == "" {
		return fmt.Errorf("telegram.allow_from is required and must be a single Telegram username")
	}
	if !telegramUsernameRe.MatchString(allowed) {
		return fmt.Errorf("telegram.allow_from must be a valid Telegram username (5-32 chars: letters, numbers, underscore)")
	}
	c.Telegram.AllowFrom = allowed
	return nil
}

// validateAgent checks only LLM config (no Telegram token needed for CLI-only usage).
func (c *Config) validateAgent() error {
	if isManagedGPTOSSModel(c.Model) {
		if err := c.validateManagedGPTOSSProviders(); err != nil {
			return err
		}
	} else if c.cerebrasAPIKey() == "" {
		return fmt.Errorf("api_key (or cerebras.api_key) is required")
	}
	if !isManagedGPTOSSModel(c.Model) && strings.TrimSpace(c.APIBase) == "" {
		return fmt.Errorf("api_base is required")
	}
	return nil
}

func (c *Config) cerebrasAPIKey() string {
	if strings.TrimSpace(c.Cerebras.APIKey) != "" {
		return strings.TrimSpace(c.Cerebras.APIKey)
	}
	return strings.TrimSpace(c.APIKey)
}

func (c *Config) validateManagedGPTOSSProviders() error {
	if strings.TrimSpace(c.Groq.APIKey) == "" {
		return fmt.Errorf("groq.api_key is required for gpt-oss-120b failover mode")
	}
	if c.cerebrasAPIKey() == "" {
		return fmt.Errorf("cerebras.api_key (or api_key) is required for gpt-oss-120b failover mode")
	}
	if strings.TrimSpace(c.OpenRouter.APIKey) == "" {
		return fmt.Errorf("openrouter.api_key is required for gpt-oss-120b failover mode")
	}
	return nil
}

func saveConfig(cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(codyDir(), 0755); err != nil {
		return err
	}
	return os.WriteFile(configPath(), append(data, '\n'), 0644)
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func normalizeTelegramUsername(username string) string {
	name := strings.TrimSpace(username)
	name = strings.TrimPrefix(name, "@")
	return strings.ToLower(strings.TrimSpace(name))
}
