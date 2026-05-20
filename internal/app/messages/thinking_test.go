package messages

import (
	"context"
	"testing"

	"github.com/niuma/kirocc-pro/internal/anthropic"
)

func TestResolveThinkingBudget_EnvIsFloor(t *testing.T) {
	t.Setenv("KIROCC_FORCE_THINKING_BUDGET", "100000")

	got := resolveThinkingBudget(context.Background(), &anthropic.Request{
		Thinking: &anthropic.ThinkingConfig{Type: anthropic.ThinkingTypeEnabled, BudgetTokens: 31999},
	})
	if got != 100000 {
		t.Fatalf("floor should raise small explicit budget, got %d", got)
	}

	got = resolveThinkingBudget(context.Background(), &anthropic.Request{
		OutputConfig: &anthropic.OutputConfig{Effort: anthropic.EffortMax},
	})
	if got != anthropic.ThinkingBudgetMax {
		t.Fatalf("floor should not lower max effort budget, got %d", got)
	}
}

func TestResolveThinkingBudget_InvalidEnvDoesNotOverride(t *testing.T) {
	t.Setenv("KIROCC_FORCE_THINKING_BUDGET", "not-a-number")
	got := resolveThinkingBudget(context.Background(), &anthropic.Request{
		Thinking: &anthropic.ThinkingConfig{Type: anthropic.ThinkingTypeEnabled, BudgetTokens: 31999},
	})
	if got != 31999 {
		t.Fatalf("invalid env should be ignored, got %d", got)
	}
}
