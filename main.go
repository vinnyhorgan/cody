//go:build !testcoverage

package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "run", "start", "gateway":
		runGateway()
	case "onboard":
		runOnboard()
	case "status":
		runStatus()
	case "agent":
		runAgent()
	case "cron":
		runCron()
	case "version", "--version", "-v":
		fmt.Println("cody", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`cody - Personal AI Assistant

Usage:
  cody                Start the bot (default)
  cody onboard        Initialize config and workspace
  cody status         Show configuration status
  cody agent          Interactive agent mode
  cody agent -m MSG   Send a single message
  cody cron           Manage scheduled jobs
  cody version        Show version
  cody help           Show this help`)
}

func runGateway() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("Failed to load config", "err", err)
		os.Exit(1)
	}
	if err := cfg.validate(); err != nil {
		slog.Error("Invalid config", "err", err)
		fmt.Fprintf(os.Stderr, "\nRun 'cody onboard' to set up your configuration.\n")
		os.Exit(1)
	}

	workspace := cfg.workspacePath()
	if err := ensureDir(workspace); err != nil {
		slog.Error("Failed to create workspace directory", "err", err)
		os.Exit(1)
	}
	if err := syncTemplates(workspace); err != nil {
		slog.Warn("Failed to sync templates", "err", err)
	}

	if cfg.Tools.WebSearchAPIKey != "" && os.Getenv("BRAVE_API_KEY") == "" {
		os.Setenv("BRAVE_API_KEY", cfg.Tools.WebSearchAPIKey)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	llm := newLLMClientFromConfig(cfg)
	bus := newMessageBus()
	sessions := newSessionManager(workspace)
	tools := newToolRegistry()
	cronPath := filepath.Join(workspace, "cron.json")

	var agent *AgentLoop
	cronSvc := newCronService(cronPath, func(message string, deliver bool, sessionKey, chatID string) string {
		if agent == nil {
			return ""
		}
		response := agent.processDirect(ctx, message, sessionKey, chatID)
		if deliver && chatID != "" && response != "" {
			bus.Outbound <- &OutboundMessage{ChatID: chatID, Content: response}
		}
		return response
	})

	agent = newAgentLoop(cfg, llm, bus, sessions, tools, cronSvc)

	heartbeat := newHeartbeatService(workspace, llm, cfg, sessions, func(ctx context.Context, content, sessionKey, chatID string) string {
		response := agent.processDirect(ctx, content, sessionKey, chatID)
		if chatID != "" && response != "" {
			bus.Outbound <- &OutboundMessage{ChatID: chatID, Content: response}
		}
		return response
	})

	tg := newTelegramBot(cfg, bus)

	cronSvc.start(ctx)
	heartbeat.start(ctx)

	if err := tg.start(ctx); err != nil {
		slog.Error("Failed to start Telegram bot", "err", err)
		os.Exit(1)
	}

	go agent.run(ctx)

	slog.Info("Cody is running",
		"model", cfg.Model,
		"workspace", workspace,
		"heartbeat", cfg.Heartbeat.Enabled)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	slog.Info("Shutting down...")
	cancel()
	tg.stop()
	heartbeat.stop()
	slog.Info("Goodbye!")
}

func runOnboard() {
	fmt.Println("🦔 Cody Setup")
	fmt.Println("=============")

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load existing config (%v); using defaults.\n", err)
		cfg = defaultConfig()
	}

	workspace := cfg.workspacePath()
	if err := ensureDir(workspace); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create workspace: %v\n", err)
		os.Exit(1)
	}
	if err := syncTemplates(workspace); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to sync templates: %v\n", err)
	}

	fmt.Printf("\n✅ Workspace initialized at: %s\n", workspace)
	fmt.Printf("📝 Config file: %s\n", configPath())

	if _, err := os.Stat(configPath()); os.IsNotExist(err) {
		if err := saveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
		}
		fmt.Println("\n📄 Created default config file.")
	}

	fmt.Printf(`
Next steps:
  1. Edit %s and set:
     - groq.api_key: Groq API key (preferred gpt-oss-120b provider + voice transcription)
     - cerebras.api_key: Cerebras API key
     - openrouter.api_key: OpenRouter API key
     - telegram.token: Your Telegram bot token (from @BotFather)
     - telegram.allow_from: ["your_telegram_user_id"]
     - model: gpt-oss-120b (default, uses automatic failover: groq -> cerebras -> openrouter)

  2. Run: cody
`, configPath())
}

// --- Status command ---

func runStatus() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("🦔 Cody Status")
	fmt.Println()

	// Config file
	if _, err := os.Stat(configPath()); err == nil {
		fmt.Printf("Config:    %s ✓\n", configPath())
	} else {
		fmt.Printf("Config:    %s ✗ (not found)\n", configPath())
	}

	// Workspace
	workspace := cfg.workspacePath()
	if info, err := os.Stat(workspace); err == nil && info.IsDir() {
		fmt.Printf("Workspace: %s ✓\n", workspace)
	} else {
		fmt.Printf("Workspace: %s ✗ (not found)\n", workspace)
	}

	fmt.Printf("Model:     %s\n", cfg.Model)

	if isManagedGPTOSSModel(cfg.Model) {
		fmt.Printf("LLM Mode:  failover (groq -> cerebras -> openrouter)\n")
		if cfg.Groq.APIKey != "" {
			fmt.Printf("Groq:      %s ✓ (chat + voice)\n", maskAPIKey(cfg.Groq.APIKey))
		} else {
			fmt.Printf("Groq:      not set ✗\n")
		}
		if key := cfg.cerebrasAPIKey(); key != "" {
			fmt.Printf("Cerebras:  %s ✓\n", maskAPIKey(key))
		} else {
			fmt.Printf("Cerebras:  not set ✗\n")
		}
		if cfg.OpenRouter.APIKey != "" {
			fmt.Printf("OpenRouter: %s ✓\n", maskAPIKey(cfg.OpenRouter.APIKey))
		} else {
			fmt.Printf("OpenRouter: not set ✗\n")
		}
	} else {
		if key := cfg.cerebrasAPIKey(); key != "" {
			fmt.Printf("API Key:   %s ✓\n", maskAPIKey(key))
		} else {
			fmt.Printf("API Key:   not set ✗\n")
		}
		if cfg.APIBase != "" {
			fmt.Printf("API Base:  %s ✓\n", cfg.APIBase)
		} else {
			fmt.Printf("API Base:  not set ✗\n")
		}
	}

	// Telegram
	if cfg.Telegram.Token != "" {
		fmt.Printf("Telegram:  configured ✓\n")
	} else {
		fmt.Printf("Telegram:  not configured ✗\n")
	}

	// Groq voice path
	if cfg.Groq.APIKey != "" {
		fmt.Printf("Voice:     configured ✓ (Groq Whisper)\n")
	} else {
		fmt.Printf("Voice:     not set (voice disabled)\n")
	}

	// Brave Search
	if cfg.Tools.WebSearchAPIKey != "" {
		fmt.Printf("Search:    configured ✓ (Brave Search)\n")
	} else {
		fmt.Printf("Search:    not set (web_search disabled)\n")
	}

	// Heartbeat
	if cfg.Heartbeat.Enabled {
		fmt.Printf("Heartbeat: every %d minutes ✓\n", cfg.Heartbeat.IntervalMinutes)
	} else {
		fmt.Printf("Heartbeat: disabled\n")
	}

	// Cron jobs
	cronPath := filepath.Join(workspace, "cron.json")
	cronSvc := newCronService(cronPath, nil)
	cronSvc.loadStore()
	jobs := cronSvc.listJobs()
	enabled := 0
	for _, j := range jobs {
		if j.Enabled {
			enabled++
		}
	}
	fmt.Printf("Cron:      %d jobs (%d enabled)\n", len(jobs), enabled)
}

// --- Agent command ---

func runAgent() {
	args := os.Args[2:]
	var message, sessionID string
	sessionID = "cli:direct"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-m", "--message":
			if i+1 < len(args) {
				i++
				message = args[i]
			} else {
				fmt.Fprintln(os.Stderr, "Error: -m requires a message argument")
				os.Exit(1)
			}
		case "-s", "--session":
			if i+1 < len(args) {
				i++
				sessionID = args[i]
			} else {
				fmt.Fprintln(os.Stderr, "Error: -s requires a session argument")
				os.Exit(1)
			}
		case "--help", "-h":
			fmt.Println(`Usage: cody agent [options]

Options:
  -m, --message MSG    Send a single message and exit
  -s, --session ID     Session ID (default: cli:direct)
  -h, --help           Show this help

Without -m, starts an interactive session.`)
			return
		default:
			fmt.Fprintf(os.Stderr, "Unknown option: %s\n", args[i])
			os.Exit(1)
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.validateAgent(); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid config: %v\nRun 'cody onboard' to set up.\n", err)
		os.Exit(1)
	}

	workspace := cfg.workspacePath()
	if err := ensureDir(workspace); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create workspace: %v\n", err)
		os.Exit(1)
	}
	if err := syncTemplates(workspace); err != nil {
		slog.Warn("Failed to sync templates", "err", err)
	}
	if cfg.Tools.WebSearchAPIKey != "" && os.Getenv("BRAVE_API_KEY") == "" {
		os.Setenv("BRAVE_API_KEY", cfg.Tools.WebSearchAPIKey)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	llm := newLLMClientFromConfig(cfg)
	bus := newMessageBus()
	sessions := newSessionManager(workspace)
	tools := newToolRegistry()
	cronPath := filepath.Join(workspace, "cron.json")
	cronSvc := newCronService(cronPath, nil)
	cronSvc.loadStore()

	agent := newAgentLoop(cfg, llm, bus, sessions, tools, cronSvc)

	if message != "" {
		// Single message mode
		response := agent.processDirect(ctx, message, sessionID, "")
		if response != "" {
			fmt.Println(response)
		}
	} else {
		// Interactive mode
		fmt.Println("🦔 Cody interactive mode (type 'exit' or Ctrl+C to quit)")
		fmt.Println()

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sig
			fmt.Println("\nGoodbye!")
			cancel()
			os.Exit(0)
		}()

		scanner := bufio.NewScanner(os.Stdin)
		for {
			fmt.Print("You: ")
			if !scanner.Scan() {
				fmt.Println("\nGoodbye!")
				break
			}
			input := strings.TrimSpace(scanner.Text())
			if input == "" {
				continue
			}
			if input == "exit" || input == "quit" || input == "/exit" || input == "/quit" || input == ":q" {
				fmt.Println("Goodbye!")
				break
			}

			response := agent.processDirect(ctx, input, sessionID, "")
			if response != "" {
				fmt.Println()
				fmt.Println(response)
				fmt.Println()
			}
		}
	}
}

// --- Cron command ---

func runCron() {
	if len(os.Args) < 3 {
		printCronUsage()
		os.Exit(1)
	}
	sub := os.Args[2]
	switch sub {
	case "list":
		cronList()
	case "add":
		cronAdd()
	case "remove":
		cronRemove()
	case "enable":
		cronEnable(true)
	case "disable":
		cronEnable(false)
	case "run":
		cronRun()
	case "help", "--help", "-h":
		printCronUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown cron command: %s\n", sub)
		printCronUsage()
		os.Exit(1)
	}
}

func printCronUsage() {
	fmt.Println(`Usage: cody cron <command>

Commands:
  list                 List scheduled jobs
  add                  Add a scheduled job
  remove <id>          Remove a job
  enable <id>          Enable a job
  disable <id>         Disable a job
  run <id>             Manually run a job
  help                 Show this help`)
}

func cronStorePath() string {
	cfg, err := loadConfig()
	if err != nil {
		cfg = defaultConfig()
	}
	return filepath.Join(cfg.workspacePath(), "cron.json")
}

func cronList() {
	showAll := false
	for _, arg := range os.Args[3:] {
		if arg == "-a" || arg == "--all" {
			showAll = true
		}
	}

	svc := newCronService(cronStorePath(), nil)
	svc.loadStore()
	jobs := svc.listJobs()

	if len(jobs) == 0 {
		fmt.Println("No scheduled jobs.")
		return
	}

	fmt.Printf("%-20s %-20s %-20s %-10s %-20s\n", "ID", "NAME", "SCHEDULE", "STATUS", "NEXT RUN")
	fmt.Println(strings.Repeat("-", 92))
	for _, job := range jobs {
		if !job.Enabled && !showAll {
			continue
		}
		status := "enabled"
		if !job.Enabled {
			status = "disabled"
		}
		nextRun := ""
		if !job.State.NextRunAt.IsZero() {
			nextRun = job.State.NextRunAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%-20s %-20s %-20s %-10s %-20s\n",
			truncStr(job.ID, 20),
			truncStr(job.Name, 20),
			truncStr(job.Schedule.Raw, 20),
			status,
			nextRun,
		)
	}
}

func truncStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func cronAdd() {
	args := os.Args[3:]
	var name, message, schedule, tz, chatID string
	deliver := false
	nextValue := func(i int, flag string) (string, int) {
		if i+1 >= len(args) {
			fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", flag)
			os.Exit(1)
		}
		return args[i+1], i + 1
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n", "--name":
			var value string
			value, i = nextValue(i, args[i])
			name = value
		case "-m", "--message":
			var value string
			value, i = nextValue(i, args[i])
			message = value
		case "-e", "--every":
			var value string
			value, i = nextValue(i, args[i])
			schedule = "every " + value + "s"
		case "-c", "--cron":
			var value string
			value, i = nextValue(i, args[i])
			schedule = value
		case "--at":
			var value string
			value, i = nextValue(i, args[i])
			schedule = "at " + value
		case "--tz":
			var value string
			value, i = nextValue(i, args[i])
			tz = value
		case "-d", "--deliver":
			deliver = true
		case "--to":
			var value string
			value, i = nextValue(i, args[i])
			chatID = value
		case "--help", "-h":
			fmt.Println(`Usage: cody cron add [options]

Options:
  -n, --name NAME       Job name (required)
  -m, --message MSG     Message for agent (required)
  -e, --every N         Run every N seconds
  -c, --cron EXPR       Cron expression (e.g. '0 9 * * *')
  --at TIME             One-time at ISO time (e.g. '2025-06-15T10:00:00Z')
  --tz TIMEZONE         IANA timezone for cron
	  -d, --deliver         Deliver response to chat
	  --to CHAT_ID          Target chat ID for delivery`)
			return
		default:
			fmt.Fprintf(os.Stderr, "Unknown option: %s\n", args[i])
			os.Exit(1)
		}
	}
	if name == "" || message == "" || schedule == "" {
		fmt.Fprintln(os.Stderr, "Error: --name, --message, and a schedule (--every, --cron, or --at) are required")
		os.Exit(1)
	}

	svc := newCronService(cronStorePath(), nil)
	svc.loadStore()
	job, err := svc.addJob(name, schedule, message, deliver, tz, chatID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Added job '%s' (%s)\n", job.Name, job.ID)
}

func cronRemove() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: cody cron remove <job-id>")
		os.Exit(1)
	}
	jobID := os.Args[3]
	svc := newCronService(cronStorePath(), nil)
	svc.loadStore()
	if svc.removeJob(jobID) {
		fmt.Printf("✓ Removed job %s\n", jobID)
	} else {
		fmt.Fprintf(os.Stderr, "Job %s not found\n", jobID)
		os.Exit(1)
	}
}

func cronEnable(enable bool) {
	if len(os.Args) < 4 {
		if enable {
			fmt.Fprintln(os.Stderr, "Usage: cody cron enable <job-id>")
		} else {
			fmt.Fprintln(os.Stderr, "Usage: cody cron disable <job-id>")
		}
		os.Exit(1)
	}
	jobID := os.Args[3]
	svc := newCronService(cronStorePath(), nil)
	svc.loadStore()
	if svc.enableJob(jobID, enable) {
		action := "enabled"
		if !enable {
			action = "disabled"
		}
		fmt.Printf("✓ Job %s %s\n", jobID, action)
	} else {
		fmt.Fprintf(os.Stderr, "Job %s not found\n", jobID)
		os.Exit(1)
	}
}

func cronRun() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: cody cron run <job-id> [-f]")
		os.Exit(1)
	}
	jobID := os.Args[3]
	force := false
	for _, arg := range os.Args[4:] {
		if arg == "-f" || arg == "--force" {
			force = true
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.validateAgent(); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid config: %v\n", err)
		os.Exit(1)
	}

	workspace := cfg.workspacePath()
	if err := syncTemplates(workspace); err != nil {
		slog.Warn("Failed to sync templates", "err", err)
	}
	if cfg.Tools.WebSearchAPIKey != "" && os.Getenv("BRAVE_API_KEY") == "" {
		os.Setenv("BRAVE_API_KEY", cfg.Tools.WebSearchAPIKey)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cronPath := filepath.Join(workspace, "cron.json")
	svc := newCronService(cronPath, nil)
	svc.loadStore()

	// Find job
	var job *CronJob
	for _, j := range svc.listJobs() {
		if j.ID == jobID {
			job = j
			break
		}
	}
	if job == nil {
		fmt.Fprintf(os.Stderr, "Job %s not found\n", jobID)
		os.Exit(1)
	}
	if !job.Enabled && !force {
		fmt.Fprintf(os.Stderr, "Job %s is disabled (use -f to force)\n", jobID)
		os.Exit(1)
	}

	llm := newLLMClientFromConfig(cfg)
	bus := newMessageBus()
	sessions := newSessionManager(workspace)
	tools := newToolRegistry()
	agent := newAgentLoop(cfg, llm, bus, sessions, tools, svc)

	sessionKey := "cron:" + job.ID
	chatID := job.ChatID

	response := agent.processDirect(ctx, job.Message, sessionKey, chatID)
	if response != "" {
		fmt.Println(response)
	}

	// Update job state
	job.State.LastRunAt = time.Now()
	job.State.LastStatus = "ok"
	svc.saveStore()
	fmt.Println("✓ Job executed")
}
