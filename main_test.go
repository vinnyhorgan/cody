package main

import (
	"context"
	"testing"
)

func TestMaskAPIKey(t *testing.T) {
	if got := maskAPIKey("short"); got != "***" {
		t.Fatalf("maskAPIKey(short) = %q, want ***", got)
	}
	if got := maskAPIKey("1234567890"); got != "1234...7890" {
		t.Fatalf("maskAPIKey(1234567890) = %q, want 1234...7890", got)
	}
}

func TestExecuteCronJobDeliverSendsRawMessage(t *testing.T) {
	bus := newMessageBus()
	got := executeCronJob(context.Background(), nil, bus, "You're cool!", true, "cron:1", "123")
	if got != "You're cool!" {
		t.Fatalf("executeCronJob() = %q, want %q", got, "You're cool!")
	}

	select {
	case out := <-bus.Outbound:
		if out.ChatID != "123" {
			t.Fatalf("chatID = %q, want %q", out.ChatID, "123")
		}
		if out.Content != "You're cool!" {
			t.Fatalf("content = %q, want %q", out.Content, "You're cool!")
		}
	default:
		t.Fatal("expected outbound reminder message")
	}
}

func TestExecuteCronJobDeliverMissingChatID(t *testing.T) {
	bus := newMessageBus()
	got := executeCronJob(context.Background(), nil, bus, "hello", true, "cron:1", "")
	if got != "" {
		t.Fatalf("executeCronJob() = %q, want empty result", got)
	}

	select {
	case out := <-bus.Outbound:
		t.Fatalf("unexpected outbound message: %+v", out)
	default:
	}
}

func TestCronResultStatus(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{name: "empty", input: "   ", expect: "empty"},
		{name: "llm_unavailable", input: llmUnavailableMessage, expect: "error"},
		{name: "prefixed_error", input: "Error: failed", expect: "error"},
		{name: "ok_text", input: "all good", expect: "ok"},
	}
	for _, tt := range tests {
		if got := cronResultStatus(tt.input); got != tt.expect {
			t.Fatalf("%s: cronResultStatus(%q)=%q, want %q", tt.name, tt.input, got, tt.expect)
		}
	}
}
