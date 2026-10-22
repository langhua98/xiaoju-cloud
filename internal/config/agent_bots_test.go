package config

import (
	"slices"
	"testing"
)

// TG_AGENT_BOT_TOKENS 的 Bot 算进额外的 Bot，并且只给 AI 用；TG_AGENT_BOTS 写 ID 也行。
func TestAgentBots(t *testing.T) {
	t.Setenv("TG_EXTRA_BOT_TOKENS", "111:aaa,222:bbb")
	t.Setenv("TG_AGENT_BOTS", "222")
	t.Setenv("TG_AGENT_BOT_TOKENS", "333:ccc")
	c, err := Load()
	if err != nil {
		t.Skip("config needs other variables:", err)
	}
	if !slices.Equal(c.Telegram.ExtraBotTokens, []string{"111:aaa", "222:bbb", "333:ccc"}) {
		t.Fatalf("extra: %v", c.Telegram.ExtraBotTokens)
	}
	if !slices.Equal(c.Telegram.AgentBots, []string{"222", "333"}) {
		t.Fatalf("agent: %v", c.Telegram.AgentBots)
	}
}
