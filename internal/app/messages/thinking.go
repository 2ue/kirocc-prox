package messages

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/logging"
)

// formatContextWindow renders a context window size as a human label like
// "200k" or "1M" for log output.
func formatContextWindow(size int) string {
	if size >= 1_000_000 {
		return fmt.Sprintf("%dM", size/1_000_000)
	}
	return fmt.Sprintf("%dk", size/1000)
}

// [fork] Modified from upstream d-kuro/kirocc: added KIROCC_FORCE_THINKING_BUDGET
// env-var floor injection (fix #8) — Claude Code's MAX_THINKING_TOKENS doesn't
// always make it to the Kiro upstream, so the proxy enforces it server-side.
//
// resolveThinkingBudget returns the explicit budget_tokens on the request, or
// derives one from the effort level. KIROCC_FORCE_THINKING_BUDGET acts as a
// floor, not a hard override: higher client-requested budgets are preserved.
// Unknown effort levels warn and fall back to medium.
func resolveThinkingBudget(ctx context.Context, req *anthropic.Request) int {
	budget := 0
	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		budget = req.Thinking.BudgetTokens
	} else {
		effort := req.Effort()
		switch effort {
		case anthropic.EffortMax:
			budget = anthropic.ThinkingBudgetMax
		case anthropic.EffortXHigh:
			budget = anthropic.ThinkingBudgetXHigh
		case anthropic.EffortHigh:
			budget = anthropic.ThinkingBudgetHigh
		case anthropic.EffortLow:
			budget = anthropic.ThinkingBudgetLow
		case anthropic.EffortMedium, "":
			budget = anthropic.ThinkingBudgetMedium
		default:
			_, short := logging.TraceIDs(ctx)
			slog.WarnContext(ctx, "unknown effort level, falling back to medium",
				"trace_id", short, "effort", effort)
			budget = anthropic.ThinkingBudgetMedium
		}
	}

	if forced := os.Getenv("KIROCC_FORCE_THINKING_BUDGET"); forced != "" {
		floor, err := strconv.Atoi(forced)
		if err == nil && floor > 0 {
			if budget < floor {
				return floor
			}
			return budget
		}
		_, short := logging.TraceIDs(ctx)
		slog.WarnContext(ctx, "invalid forced thinking budget, ignoring",
			"trace_id", short, "value", forced)
	}
	return budget
}
