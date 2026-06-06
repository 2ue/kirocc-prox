package promptcache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/tokencount"
)

const (
	defaultTTL              = 5 * time.Minute
	hourTTL                 = time.Hour
	defaultMinCacheTokens   = 1024
	extendedMinCacheTokens  = 4096
	haiku35MinCacheTokens   = 2048
	targetReadRatioSpread   = 0.03
	cachePointDefaultMarker = "default"
	defaultPruneInterval    = 30 * time.Second

	// Defaults keep the simulated prompt-cache tracker bounded on small
	// machines while leaving enough headroom for hundreds of accounts/sessions.
	DefaultMaxScopes  = 4096
	DefaultMaxEntries = 65536
)

// Options controls local prompt-cache usage simulation. The tracker does not
// change upstream requests; it only helps report Anthropic-compatible cache
// token fields when the upstream response did not include them.
type Options struct {
	Enabled         bool
	TargetReadRatio float64
	MaxScopes       int
	MaxEntries      int
}

func DefaultOptions() Options {
	return Options{
		Enabled:         true,
		TargetReadRatio: 0.98,
		MaxScopes:       DefaultMaxScopes,
		MaxEntries:      DefaultMaxEntries,
	}
}

func (o Options) normalized() Options {
	if math.IsNaN(o.TargetReadRatio) || math.IsInf(o.TargetReadRatio, 0) {
		o.TargetReadRatio = DefaultOptions().TargetReadRatio
	}
	o.TargetReadRatio = clampFloat(o.TargetReadRatio, 0, 0.99)
	if o.MaxScopes <= 0 {
		o.MaxScopes = DefaultOptions().MaxScopes
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultOptions().MaxEntries
	}
	if o.MaxEntries < o.MaxScopes {
		o.MaxEntries = o.MaxScopes
	}
	return o
}

// Scope isolates prompt-cache entries by selected credential, session, and
// upstream model, matching how real prompt cache hits are account/model scoped.
type Scope struct {
	CredentialID   string
	ConversationID string
	Model          string
}

// Usage is the simulated cache token split for a request.
type Usage struct {
	CacheCreationInputTokens   int
	CacheReadInputTokens       int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
	EffectiveCacheRatio        float64
}

func (u Usage) Empty() bool {
	return u.CacheCreationInputTokens <= 0 && u.CacheReadInputTokens <= 0
}

type entry struct {
	expiresAt    time.Time
	ttl          time.Duration
	cachedTokens int
}

type lookupPoint struct {
	fingerprint      [32]byte
	cumulativeTokens int
}

type breakpoint struct {
	ttl time.Duration
}

type profile struct {
	breakpoints      []breakpoint
	lookupPoints     []lookupPoint
	totalInputTokens int
	model            string
}

// Plan is a computed prompt-cache decision. Commit it only after the upstream
// request has completed successfully and the simulated usage was actually used.
type Plan struct {
	tracker *Tracker
	scope   Scope
	profile profile
	usage   Usage
	opts    planOptions
}

type planOptions struct {
	synthesizeStablePrefix bool
	targetReadRatio        float64
	hasTargetReadRatio     bool
}

// BuildOptions controls prompt-cache profile construction for a single
// request. It is intentionally policy-neutral: callers decide from their route
// config whether stable-prefix synthesis is allowed for that path/profile.
type BuildOptions struct {
	SynthesizeStablePrefix bool
	TargetReadRatio        float64
	hasTargetReadRatio     bool
}

func (p *Plan) Usage() Usage {
	if p == nil {
		return Usage{}
	}
	return p.usage
}

func (p *Plan) Commit() {
	if p == nil || p.tracker == nil || p.usage.Empty() {
		return
	}
	p.tracker.update(p.scope, p.profile, p.targetReadRatio())
}

func (p *Plan) Seed() uint64 {
	if p == nil {
		return 0
	}
	return profileSeed(p.profile)
}

func (p *Plan) targetReadRatio() float64 {
	if p == nil {
		return DefaultOptions().TargetReadRatio
	}
	if p.opts.hasTargetReadRatio {
		return p.opts.targetReadRatio
	}
	return p.tracker.opts.normalized().TargetReadRatio
}

// Tracker keeps short-lived local prompt-cache fingerprints in memory.
type Tracker struct {
	mu          sync.Mutex
	opts        Options
	entries     map[Scope]map[[32]byte]entry
	lastPruneAt time.Time
}

func NewTracker(opts Options) *Tracker {
	return &Tracker{opts: opts.normalized(), entries: make(map[Scope]map[[32]byte]entry)}
}

// BuildPlan computes creation/read tokens for req under scope. It returns nil
// when prompt caching is disabled, no cache breakpoint is present, the scope is
// incomplete, or the prompt is below the model's cacheable-token threshold.
func (t *Tracker) BuildPlan(req *anthropic.Request, scope Scope, totalInputTokens int) *Plan {
	return t.BuildPlanWithOptions(req, scope, totalInputTokens, BuildOptions{})
}

func (t *Tracker) BuildPlanWithOptions(req *anthropic.Request, scope Scope, totalInputTokens int, buildOpts BuildOptions) *Plan {
	if t == nil {
		return nil
	}
	opts := t.opts.normalized()
	if !opts.Enabled || req == nil || scope.CredentialID == "" || scope.ConversationID == "" || scope.Model == "" {
		return nil
	}
	planOpts := normalizePlanOptions(buildOpts, opts)
	prof, ok := buildProfile(req, totalInputTokens, scope.Model, planOpts.synthesizeStablePrefix)
	if !ok {
		return nil
	}
	usage := t.compute(scope, prof, planOpts.targetReadRatio)
	if usage.Empty() {
		return nil
	}
	return &Plan{tracker: t, scope: scope, profile: prof, usage: usage, opts: planOpts}
}

func normalizePlanOptions(buildOpts BuildOptions, trackerOpts Options) planOptions {
	target := buildOpts.TargetReadRatio
	hasTarget := buildOpts.hasTargetReadRatio || target > 0
	if hasTarget && (math.IsNaN(target) || math.IsInf(target, 0) || target < 0) {
		target = trackerOpts.TargetReadRatio
		hasTarget = false
	}
	if !hasTarget {
		target = trackerOpts.TargetReadRatio
	}
	return planOptions{
		synthesizeStablePrefix: buildOpts.SynthesizeStablePrefix,
		targetReadRatio:        clampFloat(target, 0, 0.99),
		hasTargetReadRatio:     hasTarget,
	}
}

func buildProfile(req *anthropic.Request, totalInputTokens int, model string, synthesizeStablePrefix bool) (profile, bool) {
	blocks := flattenCacheBlocks(req)
	if len(blocks) == 0 {
		return profile{}, false
	}

	var breakpoints []breakpoint
	var lookupPoints []lookupPoint
	hasher := sha256.New()
	cumulativeTokens := 0
	var activeTTL time.Duration
	var hasActiveTTL bool

	for _, block := range blocks {
		canonical := canonicalize(block.value)
		hasher.Write([]byte(strconv.Itoa(len(canonical))))
		hasher.Write([]byte{0})
		hasher.Write([]byte(canonical))
		hasher.Write([]byte{0})
		cumulativeTokens += block.tokens

		var fingerprint [32]byte
		sum := hasher.Sum(nil)
		copy(fingerprint[:], sum)
		lookupPoints = append(lookupPoints, lookupPoint{fingerprint: fingerprint, cumulativeTokens: cumulativeTokens})

		breakpointTTL := time.Duration(0)
		hasBreakpoint := false
		if block.ttl > 0 {
			activeTTL = block.ttl
			hasActiveTTL = true
			breakpointTTL = block.ttl
			hasBreakpoint = true
		} else if block.messageEnd && hasActiveTTL {
			breakpointTTL = activeTTL
			hasBreakpoint = true
		}
		if hasBreakpoint {
			breakpoints = append(breakpoints, breakpoint{ttl: breakpointTTL})
		}
	}

	if len(breakpoints) == 0 && synthesizeStablePrefix {
		breakpoints = append(breakpoints, breakpoint{ttl: defaultTTL})
	}
	if len(breakpoints) == 0 || len(lookupPoints) == 0 {
		return profile{}, false
	}
	return profile{
		breakpoints:      breakpoints,
		lookupPoints:     lookupPoints,
		totalInputTokens: max(totalInputTokens, cumulativeTokens),
		model:            model,
	}, true
}

type cacheBlock struct {
	value      any
	tokens     int
	ttl        time.Duration
	messageEnd bool
}

func flattenCacheBlocks(req *anthropic.Request) []cacheBlock {
	var blocks []cacheBlock
	appendBlock(&blocks, map[string]any{
		"kind":  "request_prelude",
		"model": req.Model,
	}, false)

	for _, tool := range req.Tools {
		value := map[string]any{
			"kind":         "tool",
			"type":         tool.Type,
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.InputSchema,
		}
		if tool.CacheControl != nil {
			value["cache_control"] = cacheControlValue(tool.CacheControl)
		}
		appendBlock(&blocks, value, false)
	}

	if req.System.Text != "" {
		appendBlock(&blocks, map[string]any{
			"kind":  "system",
			"block": map[string]any{"type": anthropic.BlockTypeText, "text": req.System.Text},
		}, false)
	}
	for _, block := range req.System.Blocks {
		blockValue := map[string]any{
			"type": block.Type,
			"text": block.Text,
		}
		if block.CacheControl != nil {
			blockValue["cache_control"] = cacheControlValue(block.CacheControl)
		}
		value := map[string]any{
			"kind":  "system",
			"block": blockValue,
		}
		appendBlock(&blocks, value, false)
	}

	for _, msg := range req.Messages {
		if msg.Content.IsString() {
			appendBlock(&blocks, map[string]any{
				"kind": "message",
				"role": msg.Role,
				"block": map[string]any{
					"type": anthropic.BlockTypeText,
					"text": msg.Content.Text,
				},
			}, true)
			continue
		}
		last := len(msg.Content.Blocks) - 1
		for i, block := range msg.Content.Blocks {
			appendBlock(&blocks, map[string]any{
				"kind":  "message",
				"role":  msg.Role,
				"block": contentBlockValue(block),
			}, i == last)
		}
	}
	return blocks
}

func appendBlock(blocks *[]cacheBlock, value any, messageEnd bool) {
	blockValue := value
	if obj, ok := value.(map[string]any); ok {
		if nested, ok := obj["block"]; ok {
			blockValue = nested
		}
	}
	if isBillingHeaderBlock(blockValue) {
		return
	}
	canonical := canonicalize(value)
	tokens, err := tokencount.CountBytes([]byte(canonical))
	if err != nil || tokens <= 0 {
		tokens = estimateTokens(canonical)
	}
	*blocks = append(*blocks, cacheBlock{
		value:      value,
		tokens:     tokens,
		ttl:        promptCacheTTL(blockValue),
		messageEnd: messageEnd,
	})
}

func cacheControlValue(cc *anthropic.CacheControl) map[string]any {
	value := map[string]any{"type": cc.Type}
	if cc.TTL != nil {
		value["ttl"] = cc.TTL
	}
	return value
}

func contentBlockValue(block anthropic.ContentBlock) map[string]any {
	value := map[string]any{"type": block.Type}
	putString(value, "text", block.Text)
	putString(value, "thinking", block.Thinking)
	putString(value, "signature", block.Signature)
	putString(value, "id", block.ID)
	putString(value, "name", block.Name)
	if len(block.Input) > 0 {
		value["input"] = block.Input
	}
	putString(value, "tool_use_id", block.ToolUseID)
	if !block.Content.IsString() || block.Content.Text != "" {
		value["content"] = messageContentValue(block.Content)
	}
	if block.IsError {
		value["is_error"] = true
	}
	if block.Source != nil {
		value["source"] = map[string]any{
			"type":       block.Source.Type,
			"media_type": block.Source.MediaType,
			"data":       block.Source.Data,
		}
	}
	putString(value, "tool_name", block.ToolName)
	if len(block.ToolReferences) > 0 {
		refs := make([]any, 0, len(block.ToolReferences))
		for _, ref := range block.ToolReferences {
			refs = append(refs, contentBlockValue(ref))
		}
		value["tool_references"] = refs
	}
	if block.CacheControl != nil {
		value["cache_control"] = cacheControlValue(block.CacheControl)
	}
	return value
}

func messageContentValue(content anthropic.MessageContent) any {
	if content.IsString() {
		return content.Text
	}
	items := make([]any, 0, len(content.Blocks))
	for _, block := range content.Blocks {
		items = append(items, contentBlockValue(block))
	}
	return items
}

func putString(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

func promptCacheTTL(value any) time.Duration {
	obj, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	cache, ok := obj["cache_control"].(map[string]any)
	if !ok {
		return 0
	}
	cacheType, _ := cache["type"].(string)
	if !strings.EqualFold(cacheType, "ephemeral") {
		return 0
	}
	ttl := parseTTL(cache["ttl"])
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if ttl > defaultTTL {
		return hourTTL
	}
	return defaultTTL
}

func parseTTL(value any) time.Duration {
	switch v := value.(type) {
	case nil:
		return 0
	case int:
		return time.Duration(v) * time.Second
	case int64:
		return time.Duration(v) * time.Second
	case float64:
		return time.Duration(v) * time.Second
	case string:
		s := strings.TrimSpace(strings.ToLower(v))
		if s == "" {
			return 0
		}
		mult := time.Second
		if last := s[len(s)-1]; last < '0' || last > '9' {
			switch last {
			case 'h':
				mult = time.Hour
			case 'm':
				mult = time.Minute
			case 's':
				mult = time.Second
			default:
				return 0
			}
			s = strings.TrimSpace(s[:len(s)-1])
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0
		}
		return time.Duration(n) * mult
	default:
		return 0
	}
}

func (t *Tracker) compute(scope Scope, prof profile, targetReadRatio float64) Usage {
	minTokens := minCacheableTokensForModel(prof.model)
	effectiveRatio := effectiveCacheReadRatio(prof, targetReadRatio)
	targetTokens := targetCacheTokens(prof.totalInputTokens, effectiveRatio, minTokens)
	if targetTokens <= 0 {
		return Usage{}
	}

	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredIfDueLocked(now)

	entries, ok := t.entries[scope]
	if !ok {
		cache5m, cache1h := targetTTLBreakdown(prof, targetTokens)
		return Usage{
			CacheCreationInputTokens:   targetTokens,
			CacheCreation5mInputTokens: cache5m,
			CacheCreation1hInputTokens: cache1h,
			EffectiveCacheRatio:        effectiveRatio,
		}
	}

	matchedTokens := 0
	for i := len(prof.lookupPoints) - 1; i >= 0; i-- {
		point := prof.lookupPoints[i]
		if point.cumulativeTokens < minTokens {
			continue
		}
		entry, ok := entries[point.fingerprint]
		if !ok || !entry.expiresAt.After(now) {
			continue
		}
		entry.expiresAt = now.Add(entry.ttl)
		entries[point.fingerprint] = entry
		matchedTokens = min(entry.cachedTokens, targetTokens)
		break
	}

	creation := max(targetTokens-matchedTokens, 0)
	cache5m, cache1h := targetTTLBreakdown(prof, creation)
	return Usage{
		CacheCreationInputTokens:   creation,
		CacheReadInputTokens:       max(matchedTokens, 0),
		CacheCreation5mInputTokens: cache5m,
		CacheCreation1hInputTokens: cache1h,
		EffectiveCacheRatio:        effectiveRatio,
	}
}

func (t *Tracker) update(scope Scope, prof profile, targetReadRatio float64) {
	minTokens := minCacheableTokensForModel(prof.model)
	effectiveRatio := effectiveCacheReadRatio(prof, targetReadRatio)
	targetTokens := targetCacheTokens(prof.totalInputTokens, effectiveRatio, minTokens)
	if targetTokens <= 0 || len(prof.lookupPoints) == 0 {
		return
	}
	flatTotal := prof.lookupPoints[len(prof.lookupPoints)-1].cumulativeTokens
	if flatTotal <= 0 {
		return
	}
	ttl := defaultTTL
	if len(prof.breakpoints) > 0 {
		ttl = prof.breakpoints[len(prof.breakpoints)-1].ttl
	}

	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredIfDueLocked(now)
	entries := t.entries[scope]
	if entries == nil {
		entries = make(map[[32]byte]entry)
		t.entries[scope] = entries
	}

	for _, point := range prof.lookupPoints {
		scaled := int(math.Round(float64(point.cumulativeTokens) / float64(flatTotal) * float64(targetTokens)))
		cachedTokens := min(max(scaled, 0), targetTokens)
		if cachedTokens < minTokens {
			continue
		}
		entries[point.fingerprint] = entry{
			expiresAt:    now.Add(ttl),
			ttl:          ttl,
			cachedTokens: cachedTokens,
		}
	}
	t.enforceLimitsLocked()
}

func targetTTLBreakdown(prof profile, creation int) (cache5m, cache1h int) {
	if creation <= 0 {
		return 0, 0
	}
	ttl := defaultTTL
	if len(prof.breakpoints) > 0 {
		ttl = prof.breakpoints[len(prof.breakpoints)-1].ttl
	}
	if ttl >= hourTTL {
		return 0, creation
	}
	return creation, 0
}

func (t *Tracker) pruneExpiredLocked(now time.Time) {
	for scope, entries := range t.entries {
		for fp, entry := range entries {
			if !entry.expiresAt.After(now) {
				delete(entries, fp)
			}
		}
		if len(entries) == 0 {
			delete(t.entries, scope)
		}
	}
}

func (t *Tracker) pruneExpiredIfDueLocked(now time.Time) {
	if !t.lastPruneAt.IsZero() && now.Sub(t.lastPruneAt) < defaultPruneInterval {
		return
	}
	t.pruneExpiredLocked(now)
	t.lastPruneAt = now
}

// Sweep removes expired tracker entries and reapplies memory bounds.
func (t *Tracker) Sweep() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	before := t.entryCountLocked()
	now := time.Now()
	t.pruneExpiredLocked(now)
	t.lastPruneAt = now
	t.enforceLimitsLocked()
	return before - t.entryCountLocked()
}

// RunJanitor periodically removes expired or over-cap entries until ctx ends.
func (t *Tracker) RunJanitor(ctx context.Context, interval time.Duration) {
	if t == nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = t.Sweep()
		}
	}
}

func (t *Tracker) enforceLimitsLocked() {
	opts := t.opts.normalized()
	if len(t.entries) > opts.MaxScopes {
		scopes := make([]scopeExpiry, 0, len(t.entries))
		for scope, entries := range t.entries {
			scopes = append(scopes, scopeExpiry{scope: scope, expiresAt: scopeEarliestExpiry(entries)})
		}
		sort.Slice(scopes, func(i, j int) bool {
			return scopes[i].expiresAt.Before(scopes[j].expiresAt)
		})
		remove := len(t.entries) - opts.MaxScopes
		for i := 0; i < remove; i++ {
			delete(t.entries, scopes[i].scope)
		}
	}

	total := t.entryCountLocked()
	if total <= opts.MaxEntries {
		return
	}
	entries := make([]entryRef, 0, total)
	for scope, scopeEntries := range t.entries {
		for fp, entry := range scopeEntries {
			entries = append(entries, entryRef{scope: scope, fingerprint: fp, expiresAt: entry.expiresAt})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].expiresAt.Before(entries[j].expiresAt)
	})
	remove := total - opts.MaxEntries
	for i := 0; i < remove; i++ {
		ref := entries[i]
		scopeEntries := t.entries[ref.scope]
		delete(scopeEntries, ref.fingerprint)
		if len(scopeEntries) == 0 {
			delete(t.entries, ref.scope)
		}
	}
}

func (t *Tracker) entryCountLocked() int {
	total := 0
	for _, entries := range t.entries {
		total += len(entries)
	}
	return total
}

type scopeExpiry struct {
	scope     Scope
	expiresAt time.Time
}

type entryRef struct {
	scope       Scope
	fingerprint [32]byte
	expiresAt   time.Time
}

func scopeEarliestExpiry(entries map[[32]byte]entry) time.Time {
	var earliest time.Time
	for _, entry := range entries {
		if earliest.IsZero() || entry.expiresAt.Before(earliest) {
			earliest = entry.expiresAt
		}
	}
	return earliest
}

func targetCacheTokens(totalInputTokens int, targetReadRatio float64, minTokens int) int {
	if totalInputTokens <= 1 {
		return 0
	}
	target := int(math.Round(float64(totalInputTokens) * clampFloat(targetReadRatio, 0, 0.99)))
	target = min(max(target, 0), totalInputTokens-1)
	if target < minTokens {
		return 0
	}
	return target
}

func effectiveCacheReadRatio(prof profile, targetReadRatio float64) float64 {
	target := clampFloat(targetReadRatio, 0, 0.99)
	if target <= 0 {
		return 0
	}
	low := maxFloat(target-targetReadRatioSpread, 0)
	high := minFloat(target+targetReadRatioSpread, 0.99)
	if high <= low {
		return low
	}
	return low + deterministicRatioUnit(prof)*(high-low)
}

func deterministicRatioUnit(prof profile) float64 {
	raw := profileSeed(prof)
	if raw == 0 {
		return 0.5
	}
	return float64(raw) / float64(^uint64(0))
}

func profileSeed(prof profile) uint64 {
	if len(prof.lookupPoints) == 0 {
		return 0
	}
	fp := prof.lookupPoints[len(prof.lookupPoints)-1].fingerprint
	var raw uint64
	for _, b := range fp[:8] {
		raw = raw<<8 | uint64(b)
	}
	return raw
}

func minCacheableTokensForModel(model string) int {
	m := strings.ReplaceAll(strings.ToLower(model), "_", "-")
	if strings.Contains(m, "haiku") {
		if strings.Contains(m, "3-5") || strings.Contains(m, "3.5") {
			return haiku35MinCacheTokens
		}
		return extendedMinCacheTokens
	}
	if strings.Contains(m, "opus") &&
		(m == "opus" || m == "opusplan" || strings.Contains(m, "4-5") || strings.Contains(m, "4.5") ||
			strings.Contains(m, "4-6") || strings.Contains(m, "4.6") || strings.Contains(m, "4-7") ||
			strings.Contains(m, "4.7")) {
		return extendedMinCacheTokens
	}
	return defaultMinCacheTokens
}

func isBillingHeaderBlock(value any) bool {
	obj, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if block, ok := obj["block"]; ok {
		return isBillingHeaderBlock(block)
	}
	if typ, _ := obj["type"].(string); typ != "" && typ != anthropic.BlockTypeText {
		return false
	}
	text, _ := obj["text"].(string)
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "x-anthropic-billing-header:")
}

func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return max(1, len([]rune(s))/4)
}

func canonicalize(value any) string {
	var b strings.Builder
	writeCanonical(&b, value)
	return b.String()
}

func writeCanonical(b *strings.Builder, value any) {
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(strconv.Quote(v))
	case int:
		b.WriteString(strconv.Itoa(v))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case float64:
		b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			if key == "cache_control" {
				continue
			}
			keys = append(keys, key)
		}
		slices.Sort(keys)
		b.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(key))
			b.WriteByte(':')
			if isCachePositionKey(key) {
				b.WriteString("null")
			} else {
				writeCanonical(b, v[key])
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, item)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, item)
		}
		b.WriteByte(']')
	default:
		b.WriteString(strconv.Quote(fmt.Sprint(v)))
	}
}

func isCachePositionKey(key string) bool {
	switch key {
	case "tool_index", "system_index", "message_index", "block_index":
		return true
	default:
		return false
	}
}

func clampFloat(v, low, high float64) float64 {
	return minFloat(maxFloat(v, low), high)
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func CachePointType() string {
	return cachePointDefaultMarker
}
