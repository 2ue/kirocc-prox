// [fork] Codex has no documented quota endpoint that ChatGPT desktop /
// codex_cli_rs queries proactively (per CLIProxyAPI analysis). Quota is
// signaled REACTIVELY through 429 / 503 responses from
// chatgpt.com/backend-api/codex/responses, which the executor will
// translate into MarkRateLimit() calls in Phase III.2.
//
// We still satisfy the Provider.FetchQuota method so the registry doesn't
// special-case Codex. The returned snapshot is empty except for
// FetchedAt and PlanName (sourced from the cred's metadata when the
// id_token was decoded at OAuth time).

package codex

import (
	"context"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// FetchQuota returns a placeholder snapshot. Numeric fields stay at zero
// (the admin UI renders this as "—") and the dashboard relies on the
// reactive rate-limit signal for cooldown bookkeeping.
func (p *Provider) FetchQuota(_ context.Context, cred *pool.Credential) (*pool.KiroQuotaSnapshot, error) {
	cred.Mu.RLock()
	plan := ""
	if cred.Metadata != nil {
		plan = cred.Metadata["chatgpt_plan_type"]
	}
	cred.Mu.RUnlock()
	return &pool.KiroQuotaSnapshot{
		FetchedAt: time.Now(),
		PlanName:  formatPlanName(plan),
		PlanTier:  plan,
	}, nil
}

func formatPlanName(plan string) string {
	switch plan {
	case "":
		return ""
	case "free":
		return "ChatGPT Free"
	case "plus":
		return "ChatGPT Plus"
	case "pro":
		return "ChatGPT Pro"
	case "team", "enterprise":
		return "ChatGPT " + plan
	default:
		return "ChatGPT " + plan
	}
}
