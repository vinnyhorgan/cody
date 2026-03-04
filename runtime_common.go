package main

import (
	"context"
	"strings"
)

const version = "0.1.0"

func executeCronJob(ctx context.Context, agent *AgentLoop, bus *MessageBus, message string, deliver bool, sessionKey, chatID string) string {
	trimmed := strings.TrimSpace(message)
	if deliver {
		if bus == nil || chatID == "" || trimmed == "" {
			return ""
		}
		bus.Outbound <- &OutboundMessage{ChatID: chatID, Content: trimmed}
		return trimmed
	}
	if agent == nil {
		return ""
	}
	return agent.processDirect(ctx, trimmed, sessionKey, chatID)
}
