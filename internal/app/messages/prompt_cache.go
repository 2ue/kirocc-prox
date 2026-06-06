package messages

import (
	"log/slog"

	"github.com/niuma/kirocc-pro/internal/promptcache"
	"github.com/niuma/kirocc-pro/internal/respconv"
)

type promptCacheAttempt struct {
	plan          *promptcache.Plan
	simulation    promptcache.Simulation
	hasSimulation bool
	profile       promptcache.ReportProfile
	applied       bool
}

func (s *Service) promptCacheAttempt(inv *invocation, totalInputTokens int) *promptCacheAttempt {
	if inv == nil || inv.req == nil || inv.reportProfile == nil {
		return nil
	}
	profile := inv.reportProfile.Profile.Normalized()
	if !profile.Enabled {
		return nil
	}
	var plan *promptcache.Plan
	var simulation promptcache.Simulation
	var hasSimulation bool
	if profile.SimulateCache && s.promptCache != nil {
		plan = s.promptCache.BuildPlanWithOptions(inv.req, promptcache.Scope{
			CredentialID:   inv.credID,
			ConversationID: inv.conversationID,
			Model:          inv.model,
		}, totalInputTokens, promptcache.BuildOptionsForProfile(profile))
		simulation, hasSimulation = promptcache.SimulationFromPlan(plan, profile)
	}
	return &promptCacheAttempt{
		plan:          plan,
		simulation:    simulation,
		hasSimulation: hasSimulation,
		profile:       profile,
	}
}

func (s *Service) adjustedPromptCacheUsage(inv *invocation, totalInputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int) (respconv.Usage, *promptCacheAttempt) {
	usage := respconv.Usage{
		InputTokens:           totalInputTokens,
		OutputTokens:          outputTokens,
		CacheReadInputTokens:  cacheReadTokens,
		CacheWriteInputTokens: cacheWriteTokens,
	}
	attempt := s.promptCacheAttempt(inv, totalInputTokens)
	if adjuster := attempt.usageAdjuster(); adjuster != nil {
		usage = adjuster(usage)
	}
	return usage, attempt
}

func reportUsageMap(usage respconv.Usage) map[string]any {
	m := map[string]any{
		"input_tokens":                max(usage.InputTokens, 0),
		"output_tokens":               max(usage.OutputTokens, 0),
		"cache_read_input_tokens":     max(usage.CacheReadInputTokens, 0),
		"cache_creation_input_tokens": max(usage.CacheWriteInputTokens, 0),
	}
	if usage.CacheCreation5mInputTokens > 0 || usage.CacheCreation1hInputTokens > 0 {
		m["cache_creation_5m_input_tokens"] = max(usage.CacheCreation5mInputTokens, 0)
		m["cache_creation_1h_input_tokens"] = max(usage.CacheCreation1hInputTokens, 0)
	}
	return m
}

func (a *promptCacheAttempt) usageAdjuster() respconv.UsageAdjuster {
	if a == nil {
		return nil
	}
	return func(usage respconv.Usage) respconv.Usage {
		simulation := a.simulation
		if !a.hasSimulation {
			simulation.Profile = a.profile
		}
		raw := promptcache.ReportUsage{
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
			CacheCreationInputTokens: usage.CacheWriteInputTokens,
			CacheCreation5mTokens:    usage.CacheCreation5mInputTokens,
			CacheCreation1hTokens:    usage.CacheCreation1hInputTokens,
		}
		reported, usedSimulation := promptcache.ApplyReportProfile(raw, simulation, a.hasSimulation)
		usage.InputTokens = reported.InputTokens
		usage.OutputTokens = reported.OutputTokens
		usage.CacheReadInputTokens = reported.CacheReadInputTokens
		usage.CacheWriteInputTokens = reported.CacheCreationInputTokens
		usage.CacheCreation5mInputTokens = reported.CacheCreation5mTokens
		usage.CacheCreation1hInputTokens = reported.CacheCreation1hTokens
		if usedSimulation {
			a.applied = true
			slog.Debug("prompt cache usage simulated",
				"input_tokens", usage.InputTokens,
				"cache_read_input_tokens", usage.CacheReadInputTokens,
				"cache_creation_input_tokens", usage.CacheWriteInputTokens,
			)
		}
		return usage
	}
}

func (a *promptCacheAttempt) commitIfApplied() {
	if a != nil && a.applied {
		a.plan.Commit()
	}
}
