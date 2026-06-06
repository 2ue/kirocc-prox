package promptcache

import (
	"strings"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/anthropic"
)

func TestTracker_FirstCreationThenRead(t *testing.T) {
	tracker := NewTracker(Options{Enabled: true, TargetReadRatio: 0.90})
	req := &anthropic.Request{
		Model: "claude-sonnet-4-6",
		System: anthropic.SystemPrompt{Blocks: []anthropic.SystemBlock{{
			Type:         anthropic.BlockTypeText,
			Text:         strings.Repeat("cacheable prompt ", 700),
			CacheControl: &anthropic.CacheControl{Type: "ephemeral"},
		}}},
		Messages: []anthropic.Message{{Role: "user", Content: anthropic.MessageContent{Text: "Hello"}}},
	}
	scope := Scope{CredentialID: "cred-a", ConversationID: "session-a", Model: "claude-sonnet-4.6"}

	first := tracker.BuildPlan(req, scope, 10_000)
	if first == nil {
		t.Fatal("first plan is nil")
	}
	if got := first.Usage(); got.CacheCreationInputTokens <= 0 || got.CacheReadInputTokens != 0 {
		t.Fatalf("first usage = %+v, want creation-only", got)
	}
	first.Commit()

	second := tracker.BuildPlan(req, scope, 10_000)
	if second == nil {
		t.Fatal("second plan is nil")
	}
	if got := second.Usage(); got.CacheReadInputTokens <= 0 || got.CacheCreationInputTokens != 0 {
		t.Fatalf("second usage = %+v, want read-only", got)
	}
}

func TestTracker_IgnoresRequestsWithoutCacheControl(t *testing.T) {
	tracker := NewTracker(DefaultOptions())
	req := &anthropic.Request{
		Model:    "claude-sonnet-4-6",
		Messages: []anthropic.Message{{Role: "user", Content: anthropic.MessageContent{Text: "Hello"}}},
	}
	scope := Scope{CredentialID: "cred-a", ConversationID: "session-a", Model: "claude-sonnet-4.6"}
	if plan := tracker.BuildPlan(req, scope, 10_000); plan != nil {
		t.Fatalf("plan = %+v, want nil", plan.Usage())
	}
}

func TestTracker_SynthesizesStablePrefixWhenConfigured(t *testing.T) {
	tracker := NewTracker(DefaultOptions())
	req := &anthropic.Request{
		Model: "claude-sonnet-4-6",
		System: anthropic.SystemPrompt{Blocks: []anthropic.SystemBlock{{
			Type: anthropic.BlockTypeText,
			Text: strings.Repeat("stable system prompt ", 700),
		}}},
		Messages: []anthropic.Message{{Role: "user", Content: anthropic.MessageContent{Text: "Hello"}}},
	}
	scope := Scope{CredentialID: "cred-a", ConversationID: "session-a", Model: "claude-sonnet-4.6"}

	first := tracker.BuildPlanWithOptions(req, scope, 10_000, BuildOptions{
		SynthesizeStablePrefix: true,
		TargetReadRatio:        0.90,
	})
	if first == nil {
		t.Fatal("first plan is nil")
	}
	if got := first.Usage(); got.CacheCreationInputTokens <= 0 || got.CacheReadInputTokens != 0 {
		t.Fatalf("first usage = %+v, want creation-only", got)
	}
	first.Commit()

	second := tracker.BuildPlanWithOptions(req, scope, 10_000, BuildOptions{
		SynthesizeStablePrefix: true,
		TargetReadRatio:        0.90,
	})
	if second == nil {
		t.Fatal("second plan is nil")
	}
	if got := second.Usage(); got.CacheReadInputTokens <= 0 || got.CacheCreationInputTokens != 0 {
		t.Fatalf("second usage = %+v, want read-only", got)
	}
}

func TestTracker_ExplicitZeroTargetReadRatioDoesNotBuildPlan(t *testing.T) {
	tracker := NewTracker(DefaultOptions())
	req := &anthropic.Request{
		Model: "claude-sonnet-4-6",
		System: anthropic.SystemPrompt{Blocks: []anthropic.SystemBlock{{
			Type: anthropic.BlockTypeText,
			Text: strings.Repeat("stable system prompt ", 700),
		}}},
		Messages: []anthropic.Message{{Role: "user", Content: anthropic.MessageContent{Text: "Hello"}}},
	}
	scope := Scope{CredentialID: "cred-a", ConversationID: "session-a", Model: "claude-sonnet-4.6"}

	plan := tracker.BuildPlanWithOptions(req, scope, 10_000, BuildOptions{
		SynthesizeStablePrefix: true,
		TargetReadRatio:        0,
		hasTargetReadRatio:     true,
	})
	if plan != nil {
		t.Fatalf("plan = %+v, want nil for explicit zero target read ratio", plan.Usage())
	}
}

func TestTracker_SweepAppliesMemoryBounds(t *testing.T) {
	tracker := NewTracker(Options{
		Enabled:    true,
		MaxScopes:  2,
		MaxEntries: 3,
	})
	now := time.Now()
	oldScope := Scope{CredentialID: "old", ConversationID: "old", Model: "model"}
	midScope := Scope{CredentialID: "mid", ConversationID: "mid", Model: "model"}
	newScope := Scope{CredentialID: "new", ConversationID: "new", Model: "model"}

	tracker.entries[oldScope] = map[[32]byte]entry{
		[32]byte{1}: {expiresAt: now.Add(time.Minute)},
		[32]byte{2}: {expiresAt: now.Add(2 * time.Minute)},
	}
	tracker.entries[midScope] = map[[32]byte]entry{
		[32]byte{3}: {expiresAt: now.Add(3 * time.Minute)},
		[32]byte{4}: {expiresAt: now.Add(4 * time.Minute)},
	}
	tracker.entries[newScope] = map[[32]byte]entry{
		[32]byte{5}: {expiresAt: now.Add(5 * time.Minute)},
		[32]byte{6}: {expiresAt: now.Add(6 * time.Minute)},
	}

	if removed := tracker.Sweep(); removed != 3 {
		t.Fatalf("Sweep removed %d entries, want 3", removed)
	}
	if _, ok := tracker.entries[oldScope]; ok {
		t.Fatal("oldest scope was not evicted")
	}
	if got := tracker.entryCountLocked(); got != 3 {
		t.Fatalf("entry count = %d, want 3", got)
	}
	if got := len(tracker.entries); got != 2 {
		t.Fatalf("scope count = %d, want 2", got)
	}
}

func TestTracker_ExpiredEntriesDoNotReadWhenHotPathPruneThrottled(t *testing.T) {
	tracker := NewTracker(Options{Enabled: true, TargetReadRatio: 0.90})
	req := &anthropic.Request{
		Model: "claude-sonnet-4-6",
		System: anthropic.SystemPrompt{Blocks: []anthropic.SystemBlock{{
			Type:         anthropic.BlockTypeText,
			Text:         strings.Repeat("cacheable prompt ", 700),
			CacheControl: &anthropic.CacheControl{Type: "ephemeral"},
		}}},
		Messages: []anthropic.Message{{Role: "user", Content: anthropic.MessageContent{Text: "Hello"}}},
	}
	scope := Scope{CredentialID: "cred-a", ConversationID: "session-a", Model: "claude-sonnet-4.6"}

	first := tracker.BuildPlan(req, scope, 10_000)
	if first == nil {
		t.Fatal("first plan is nil")
	}
	first.Commit()

	tracker.mu.Lock()
	tracker.lastPruneAt = time.Now()
	for fp, entry := range tracker.entries[scope] {
		entry.expiresAt = time.Now().Add(-time.Second)
		tracker.entries[scope][fp] = entry
	}
	tracker.mu.Unlock()

	second := tracker.BuildPlan(req, scope, 10_000)
	if second == nil {
		t.Fatal("second plan is nil")
	}
	if got := second.Usage(); got.CacheReadInputTokens != 0 || got.CacheCreationInputTokens <= 0 {
		t.Fatalf("second usage = %+v, want creation-only when cached entries expired", got)
	}

	tracker.mu.Lock()
	beforeSweep := tracker.entryCountLocked()
	tracker.mu.Unlock()
	if beforeSweep == 0 {
		t.Fatal("expired entries were pruned on throttled hot path")
	}
	if removed := tracker.Sweep(); removed != beforeSweep {
		t.Fatalf("Sweep removed %d entries, want %d", removed, beforeSweep)
	}
}
