package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// --- Cron types ---

type CronSchedule struct {
	Kind string `json:"kind"`         // "at", "every", "cron"
	Raw  string `json:"raw"`          // original schedule string
	TZ   string `json:"tz,omitempty"` // timezone for cron expressions (e.g., "America/New_York")
}

type CronJob struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Enabled  bool         `json:"enabled"`
	Schedule CronSchedule `json:"schedule"`
	Message  string       `json:"message"`
	Deliver  bool         `json:"deliver"`           // true = send to user, false = agent task
	ChatID   string       `json:"chat_id,omitempty"` // delivery target (captured at creation time)
	State    CronJobState `json:"state"`
	Created  time.Time    `json:"created"`
}

type CronJobState struct {
	NextRunAt  time.Time `json:"next_run_at"`
	LastRunAt  time.Time `json:"last_run_at,omitempty"`
	LastStatus string    `json:"last_status,omitempty"`
}

type cronStore struct {
	Jobs []*CronJob `json:"jobs"`
}

// --- Cron Service ---

type CronService struct {
	storePath string
	onJob     func(message string, deliver bool, sessionKey, chatID string) string
	store     cronStore
	timer     *time.Timer
	mu        sync.Mutex
	running   bool
}

func newCronService(storePath string, onJob func(string, bool, string, string) string) *CronService {
	return &CronService{
		storePath: storePath,
		onJob:     onJob,
	}
}

func (cs *CronService) start(ctx context.Context) {
	cs.mu.Lock()
	cs.loadStore()
	cs.running = true
	cs.mu.Unlock()
	cs.armTimer()

	go func() {
		<-ctx.Done()
		cs.mu.Lock()
		cs.running = false
		if cs.timer != nil {
			cs.timer.Stop()
		}
		cs.mu.Unlock()
	}()
	slog.Info("Cron service started", "jobs", len(cs.store.Jobs))
}

func (cs *CronService) listJobs() []*CronJob {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]*CronJob{}, cs.store.Jobs...)
}

func (cs *CronService) addJob(name, schedule, message string, deliver bool, tz, chatID string) (*CronJob, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	sched, err := parseSchedule(schedule)
	if err != nil {
		return nil, err
	}
	sched.TZ = tz

	nextRun, err := computeNextRun(sched, time.Now())
	if err != nil {
		return nil, err
	}

	job := &CronJob{
		ID:       fmt.Sprintf("cron-%d", time.Now().UnixMilli()),
		Name:     name,
		Enabled:  true,
		Schedule: sched,
		Message:  message,
		Deliver:  deliver,
		ChatID:   chatID,
		State:    CronJobState{NextRunAt: nextRun},
		Created:  time.Now(),
	}

	cs.store.Jobs = append(cs.store.Jobs, job)
	cs.saveStore()
	cs.armTimer()
	return job, nil
}

func (cs *CronService) removeJob(id string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i, j := range cs.store.Jobs {
		if j.ID == id {
			cs.store.Jobs = append(cs.store.Jobs[:i], cs.store.Jobs[i+1:]...)
			cs.saveStore()
			cs.armTimer()
			return true
		}
	}
	return false
}

func (cs *CronService) enableJob(id string, enabled bool) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, j := range cs.store.Jobs {
		if j.ID == id {
			j.Enabled = enabled
			if enabled {
				nextRun, err := computeNextRun(j.Schedule, time.Now())
				if err != nil {
					j.Enabled = false
					j.State.LastStatus = "schedule_error"
				} else {
					j.State.NextRunAt = nextRun
				}
			} else {
				j.State.NextRunAt = time.Time{}
			}
			cs.saveStore()
			cs.armTimer()
			return true
		}
	}
	return false
}

func (cs *CronService) armTimer() {
	if cs.timer != nil {
		cs.timer.Stop()
	}

	var earliest time.Time
	for _, j := range cs.store.Jobs {
		if !j.Enabled {
			continue
		}
		if earliest.IsZero() || j.State.NextRunAt.Before(earliest) {
			earliest = j.State.NextRunAt
		}
	}

	if earliest.IsZero() {
		return
	}

	delay := time.Until(earliest)
	if delay < 0 {
		delay = 0
	}

	cs.timer = time.AfterFunc(delay, func() {
		cs.tick()
	})
}

func (cs *CronService) tick() {
	cs.mu.Lock()
	now := time.Now()
	var due []*CronJob
	for _, j := range cs.store.Jobs {
		if j.Enabled && !j.State.NextRunAt.After(now) {
			due = append(due, j)
		}
	}
	cs.mu.Unlock()

	var wg sync.WaitGroup
	for _, j := range due {
		wg.Add(1)
		go func(j *CronJob) {
			defer wg.Done()
			slog.Info("Executing cron job", "id", j.ID, "name", j.Name)
			sessionKey := "cron:" + j.ID

			result := ""
			if cs.onJob != nil {
				result = cs.onJob(j.Message, j.Deliver, sessionKey, j.ChatID)
			}

			cs.mu.Lock()
			j.State.LastRunAt = now
			j.State.LastStatus = cronResultStatus(result)

			nextRun, err := computeNextRun(j.Schedule, now)
			if err != nil {
				j.Enabled = false
				j.State.LastStatus = "schedule_error"
			} else {
				j.State.NextRunAt = nextRun
			}

			// One-shot jobs (at) disable after running.
			if j.Schedule.Kind == "at" {
				j.Enabled = false
			}

			cs.saveStore()
			cs.mu.Unlock()
		}(j)
	}
	wg.Wait()

	cs.mu.Lock()
	cs.armTimer()
	cs.mu.Unlock()
}

func (cs *CronService) loadStore() {
	data, err := os.ReadFile(cs.storePath)
	if err != nil {
		cs.store = cronStore{}
		return
	}
	if err := json.Unmarshal(data, &cs.store); err != nil {
		slog.Warn("Failed to unmarshal cron store", "err", err)
		cs.store = cronStore{}
	}
}

func (cs *CronService) saveStore() {
	data, err := json.MarshalIndent(cs.store, "", "  ")
	if err != nil {
		slog.Warn("Failed to marshal cron store", "err", err)
		return
	}
	dir := filepath.Dir(cs.storePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		slog.Warn("Failed to create cron store directory", "err", err)
		return
	}
	if err := os.Chmod(dir, 0700); err != nil {
		slog.Warn("Failed to harden cron store directory permissions", "err", err)
	}
	tmpPath := cs.storePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		slog.Warn("Failed to write cron tmp store", "err", err)
		return
	}
	if err := os.Rename(tmpPath, cs.storePath); err != nil {
		slog.Warn("Failed to replace cron store", "err", err)
		_ = os.Remove(tmpPath)
	}
}

func parseSchedule(raw string) (CronSchedule, error) {
	raw = strings.TrimSpace(raw)

	// "at <ISO datetime>"
	if strings.HasPrefix(raw, "at ") {
		_, err := time.Parse(time.RFC3339, strings.TrimPrefix(raw, "at "))
		if err != nil {
			return CronSchedule{}, fmt.Errorf("invalid at time: %w", err)
		}
		return CronSchedule{Kind: "at", Raw: raw}, nil
	}

	// "every <duration>" e.g. "every 30m", "every 1h"
	if strings.HasPrefix(raw, "every ") {
		_, err := time.ParseDuration(strings.TrimPrefix(raw, "every "))
		if err != nil {
			return CronSchedule{}, fmt.Errorf("invalid duration: %w", err)
		}
		return CronSchedule{Kind: "every", Raw: raw}, nil
	}

	// Cron expression
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(raw); err != nil {
		return CronSchedule{}, fmt.Errorf("invalid cron expression: %w", err)
	}
	return CronSchedule{Kind: "cron", Raw: raw}, nil
}

func computeNextRun(sched CronSchedule, after time.Time) (time.Time, error) {
	switch sched.Kind {
	case "at":
		t, err := time.Parse(time.RFC3339, strings.TrimPrefix(sched.Raw, "at "))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid at schedule: %w", err)
		}
		return t, nil
	case "every":
		dur, err := time.ParseDuration(strings.TrimPrefix(sched.Raw, "every "))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid every schedule: %w", err)
		}
		return after.Add(dur), nil
	case "cron":
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		schedule, err := parser.Parse(sched.Raw)
		if err != nil {
			return time.Time{}, err
		}
		// Apply timezone if specified.
		ref := after
		if sched.TZ != "" {
			loc, err := time.LoadLocation(sched.TZ)
			if err == nil {
				ref = after.In(loc)
			}
		}
		return schedule.Next(ref).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unknown schedule kind: %s", sched.Kind)
}

// --- Heartbeat Service ---

type HeartbeatService struct {
	workspace  string
	llm        *LLMClient
	config     *Config
	sessions   *SessionManager
	onExecute  func(ctx context.Context, content, sessionKey, chatID string) string
	interval   time.Duration
	cancelFunc context.CancelFunc
}

func newHeartbeatService(workspace string, llm *LLMClient, cfg *Config, sessions *SessionManager,
	onExecute func(context.Context, string, string, string) string) *HeartbeatService {
	interval := time.Duration(cfg.Heartbeat.IntervalMinutes) * time.Minute
	if interval < time.Minute {
		interval = 30 * time.Minute
	}
	return &HeartbeatService{
		workspace: workspace,
		llm:       llm,
		config:    cfg,
		sessions:  sessions,
		onExecute: onExecute,
		interval:  interval,
	}
}

func (hs *HeartbeatService) start(ctx context.Context) {
	if !hs.config.Heartbeat.Enabled {
		slog.Info("Heartbeat disabled")
		return
	}

	ctx, hs.cancelFunc = context.WithCancel(ctx)
	go hs.loop(ctx)
	slog.Info("Heartbeat started", "interval", hs.interval)
}

func (hs *HeartbeatService) stop() {
	if hs.cancelFunc != nil {
		hs.cancelFunc()
	}
}

func (hs *HeartbeatService) loop(ctx context.Context) {
	ticker := time.NewTicker(hs.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hs.tick(ctx)
		}
	}
}

func (hs *HeartbeatService) tick(ctx context.Context) {
	heartbeatFile := filepath.Join(hs.workspace, "HEARTBEAT.md")
	data, err := os.ReadFile(heartbeatFile)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return
	}

	content := string(data)
	slog.Info("Heartbeat: checking tasks")

	heartbeatTool := ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        "heartbeat_decision",
			Description: "Decide whether to run or skip heartbeat tasks",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"skip", "run"},
						"description": "Whether to run or skip the pending tasks",
					},
					"tasks": map[string]any{
						"type":        "string",
						"description": "Natural-language summary of active tasks (required for run)",
					},
				},
				"required": []string{"action"},
			},
		},
	}

	decidePrompt := fmt.Sprintf(
		`Review this task list and decide if any tasks need execution right now.
Use the heartbeat_decision tool: action="run" if tasks are due, or action="skip" if not.

Task list:
%s

Current time: %s`, content, time.Now().UTC().Format(time.RFC3339))

	resp, err := hs.llm.chat(ctx, []ChatMessage{
		{Role: "system", Content: "You are a task scheduler. Use the heartbeat_decision tool to indicate your decision."},
		{Role: "user", Content: decidePrompt},
	}, []ToolDef{heartbeatTool}, 200, 0.1, "")
	if err != nil {
		slog.Error("Heartbeat decide failed", "err", err)
		return
	}

	// Parse tool call response
	shouldRun := false
	tasks := ""
	for _, tc := range resp.ToolCalls {
		if tc.Function.Name == "heartbeat_decision" {
			var args struct {
				Action string `json:"action"`
				Tasks  string `json:"tasks"`
			}
			if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil {
				shouldRun = args.Action == "run"
				tasks = strings.TrimSpace(args.Tasks)
			}
			break
		}
	}

	if !shouldRun {
		slog.Debug("Heartbeat: no tasks due")
		return
	}

	// Phase 2: Execute via agent, targeting the most recent Telegram chat for delivery.
	slog.Info("Heartbeat: executing tasks")
	if hs.onExecute != nil {
		chatID := ""
		sessionKey := "heartbeat"
		if hs.sessions != nil {
			if cid := hs.sessions.mostRecentTelegramChatID(); cid != "" {
				chatID = cid
			}
		}
		hs.onExecute(ctx, tasks, sessionKey, chatID)
	}
}
