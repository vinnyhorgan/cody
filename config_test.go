package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := defaultConfig()

	if cfg.Model != "gpt-oss-120b" {
		t.Errorf("default model = %q, want %q", cfg.Model, "gpt-oss-120b")
	}
	if cfg.APIBase != "https://api.cerebras.ai/v1" {
		t.Errorf("default api_base = %q, want %q", cfg.APIBase, "https://api.cerebras.ai/v1")
	}
	if cfg.Agent.MaxIterations != 40 {
		t.Errorf("default max_iterations = %d, want 40", cfg.Agent.MaxIterations)
	}
	if cfg.Agent.MaxTokens != 8192 {
		t.Errorf("default max_tokens = %d, want 8192", cfg.Agent.MaxTokens)
	}
	if cfg.Agent.MemoryWindow != 100 {
		t.Errorf("default memory_window = %d, want 100", cfg.Agent.MemoryWindow)
	}
	if cfg.Heartbeat.IntervalMinutes != 30 {
		t.Errorf("default heartbeat interval = %d, want 30", cfg.Heartbeat.IntervalMinutes)
	}
	if cfg.Tools.ExecTimeout != 60 {
		t.Errorf("default exec_timeout = %d, want 60", cfg.Tools.ExecTimeout)
	}
	if !cfg.Heartbeat.Enabled {
		t.Error("heartbeat should be enabled by default")
	}
	if cfg.Telegram.ReplyToMessage {
		t.Error("reply_to_message should default to false")
	}
	if !cfg.Telegram.SendProgress {
		t.Error("send_progress should default to true")
	}
	if cfg.Telegram.SendToolHints {
		t.Error("send_tool_hints should default to false")
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := defaultConfig()

	// Missing everything
	if err := cfg.validate(); err == nil {
		t.Error("expected validation error for empty config")
	}

	cfg.Model = "custom-model"
	cfg.APIKey = "test-key"
	if err := cfg.validate(); err == nil {
		t.Error("expected validation error for missing telegram token")
	}

	cfg.Telegram.Token = "bot-token"
	if err := cfg.validate(); err == nil {
		t.Error("expected validation error for missing telegram.allow_from")
	}

	cfg.Telegram.AllowFrom = "allowed_user"
	if err := cfg.validate(); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	// Also allow cerebras.api_key as config source.
	cfg.APIKey = ""
	cfg.Cerebras.APIKey = "csk-test"
	if err := cfg.validate(); err != nil {
		t.Errorf("expected no error with cerebras.api_key, got: %v", err)
	}
}

func TestConfigValidationManagedGPTOSSRequiresAllProviders(t *testing.T) {
	cfg := defaultConfig()
	cfg.Telegram.Token = "bot-token"
	cfg.Telegram.AllowFrom = "allowed_user"
	cfg.Cerebras.APIKey = "csk-test"
	cfg.OpenRouter.APIKey = "sk-or-test"

	if err := cfg.validate(); err == nil {
		t.Fatal("expected validation error when groq key is missing")
	}

	cfg.Groq.APIKey = "gsk-test"
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected no error with all gpt-oss providers configured, got: %v", err)
	}
}

func TestConfigJSONRoundTrip(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "sk-test-123"
	cfg.Cerebras.APIKey = "csk-test-456"
	cfg.APIBase = "https://api.test.com/v1"
	cfg.Telegram.Token = "12345:ABC"
	cfg.Telegram.AllowFrom = "user1"

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var loaded Config
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	if loaded.APIKey != cfg.APIKey {
		t.Errorf("api_key = %q, want %q", loaded.APIKey, cfg.APIKey)
	}
	if loaded.Cerebras.APIKey != cfg.Cerebras.APIKey {
		t.Errorf("cerebras.api_key = %q, want %q", loaded.Cerebras.APIKey, cfg.Cerebras.APIKey)
	}
	if loaded.Telegram.Token != cfg.Telegram.Token {
		t.Errorf("telegram.token = %q, want %q", loaded.Telegram.Token, cfg.Telegram.Token)
	}
	if loaded.Telegram.AllowFrom != "user1" {
		t.Errorf("telegram.allow_from = %q, want %q", loaded.Telegram.AllowFrom, "user1")
	}
}

func TestConfigOverrides(t *testing.T) {
	// Create temp config
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	override := map[string]any{
		"api_key":  "sk-override",
		"api_base": "https://custom.api.com/v1",
		"agent":    map[string]any{"max_tokens": 8192},
	}
	data, _ := json.Marshal(override)
	os.WriteFile(cfgPath, data, 0644)

	// Load into defaults
	cfg := defaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.APIKey != "sk-override" {
		t.Errorf("api_key = %q, want %q", cfg.APIKey, "sk-override")
	}
	if cfg.Agent.MaxTokens != 8192 {
		t.Errorf("max_tokens = %d, want 8192", cfg.Agent.MaxTokens)
	}
	// Default model preserved
	if cfg.Model != "gpt-oss-120b" {
		t.Errorf("model = %q, want %q", cfg.Model, "gpt-oss-120b")
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input string
		want  string
	}{
		{"~/workspace", filepath.Join(home, "workspace")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"~nothome", "~nothome"},
	}

	for _, tt := range tests {
		got := expandHome(tt.input)
		if got != tt.want {
			t.Errorf("expandHome(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestWorkspacePath(t *testing.T) {
	cfg := defaultConfig()
	cfg.Workspace = "/tmp/testworkspace"
	if got := cfg.workspacePath(); got != "/tmp/testworkspace" {
		t.Errorf("WorkspacePath = %q, want /tmp/testworkspace", got)
	}

	cfg.Workspace = "~/myworkspace"
	home, _ := os.UserHomeDir()
	if got := cfg.workspacePath(); got != filepath.Join(home, "myworkspace") {
		t.Errorf("WorkspacePath with ~ = %q", got)
	}
}

func TestCodyDirAndConfigPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir := codyDir()
	if dir != filepath.Join(home, ".cody") {
		t.Errorf("codyDir = %q", dir)
	}
	cp := configPath()
	if cp != filepath.Join(home, ".cody", "config.json") {
		t.Errorf("configPath = %q", cp)
	}
}

func TestSaveAndLoadConfig(t *testing.T) {
	// Save uses codyDir()/config.json which is the real home dir.
	// We test the JSON round-trip portion directly instead.
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	cfg := defaultConfig()
	cfg.APIKey = "sk-test"
	cfg.APIBase = "https://test.com"
	cfg.Telegram.Token = "tok123"

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cfgFile, append(data, '\n'), 0644)

	// Read back
	readData, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	loaded := defaultConfig()
	if err := json.Unmarshal(readData, loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.APIKey != "sk-test" {
		t.Errorf("loaded api_key = %q", loaded.APIKey)
	}
	if loaded.Agent.MaxIterations != 40 {
		t.Errorf("defaults should be preserved, max_iterations = %d", loaded.Agent.MaxIterations)
	}
}

func TestLoadConfigEnvFallbacks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CEREBRAS_API_KEY", "csk-test")
	t.Setenv("CODY_API_BASE", "https://api.cerebras.ai/v1")
	t.Setenv("CODY_MODEL", "gpt-oss-120b")
	t.Setenv("TELEGRAM_BOT_TOKEN", "telegram-test")
	t.Setenv("GROQ_API_KEY", "gsk-test")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	t.Setenv("BRAVE_API_KEY", "brave-test")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.APIKey != "csk-test" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "csk-test")
	}
	if cfg.APIBase != "https://api.cerebras.ai/v1" {
		t.Errorf("APIBase = %q, want %q", cfg.APIBase, "https://api.cerebras.ai/v1")
	}
	if cfg.Model != "gpt-oss-120b" {
		t.Errorf("Model = %q, want %q", cfg.Model, "gpt-oss-120b")
	}
	if cfg.Telegram.Token != "telegram-test" {
		t.Errorf("Telegram.Token = %q, want %q", cfg.Telegram.Token, "telegram-test")
	}
	if cfg.Groq.APIKey != "gsk-test" {
		t.Errorf("Groq.APIKey = %q, want %q", cfg.Groq.APIKey, "gsk-test")
	}
	if cfg.OpenRouter.APIKey != "sk-or-test" {
		t.Errorf("OpenRouter.APIKey = %q, want %q", cfg.OpenRouter.APIKey, "sk-or-test")
	}
	if cfg.Tools.WebSearchAPIKey != "brave-test" {
		t.Errorf("Tools.WebSearchAPIKey = %q, want %q", cfg.Tools.WebSearchAPIKey, "brave-test")
	}
}

func TestLoadConfigPrefersFileValuesOverEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CEREBRAS_API_KEY", "from-env")

	cfgDir := filepath.Join(home, ".cody")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	data := []byte(`{"api_key":"from-file","api_base":"https://api.cerebras.ai/v1","telegram":{"token":"file-token"}}`)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), data, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.APIKey != "from-file" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "from-file")
	}
}

func TestLoadConfigSupportsCerebrasAPIKeyInFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CEREBRAS_API_KEY", "from-env")

	cfgDir := filepath.Join(home, ".cody")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	data := []byte(`{"api_base":"https://api.cerebras.ai/v1","telegram":{"token":"file-token"},"cerebras":{"api_key":"from-cerebras-field"}}`)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), data, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.APIKey != "from-cerebras-field" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "from-cerebras-field")
	}
}

func TestLoadConfigSupportsOpenRouterEnvFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.OpenRouter.APIKey != "sk-or-test" {
		t.Errorf("OpenRouter.APIKey = %q, want %q", cfg.OpenRouter.APIKey, "sk-or-test")
	}
}
