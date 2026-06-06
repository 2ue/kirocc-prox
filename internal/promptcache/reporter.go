package promptcache

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
)

const (
	FieldModeRaw          = "raw"
	FieldModePreserve     = "preserve"
	FieldModeSampleMax    = "sample-max"
	FieldModeSampleTarget = "sample-target"
	customRoutePrefix     = "/api"

	defaultSampledInputMaxTokens = 256
	defaultHighCacheReadRatio    = 0.95
)

// ReportConfig maps request path prefixes to named usage reporting profiles.
// Profile names are operator-defined labels; the code only interprets the
// profile parameters.
type ReportConfig struct {
	DefaultProfile string                   `json:"default_profile,omitempty"`
	Routes         map[string]string        `json:"routes,omitempty"`
	Profiles       map[string]ReportProfile `json:"profiles,omitempty"`
}

// ReportProfile controls local cache computation and final downstream usage
// projection for one route/profile.
type ReportProfile struct {
	Enabled                 bool        `json:"enabled"`
	SimulateCache           bool        `json:"simulate_cache,omitempty"`
	SynthesizeStablePrefix  bool        `json:"synthesize_stable_prefix,omitempty"`
	TargetReadRatio         float64     `json:"target_read_ratio,omitempty"`
	Input                   FieldPolicy `json:"input,omitempty"`
	Output                  FieldPolicy `json:"output,omitempty"`
	CacheRead               FieldPolicy `json:"cache_read,omitempty"`
	CacheCreation           FieldPolicy `json:"cache_creation,omitempty"`
	EmitCacheTTLFields      bool        `json:"emit_cache_ttl_fields,omitempty"`
	TokenScale              float64     `json:"token_scale,omitempty"`
	MaxSimulatedInputTokens int         `json:"max_simulated_input_tokens,omitempty"`
	CapJitterMinTokens      int         `json:"cap_jitter_min_tokens,omitempty"`
	CapJitterMaxTokens      int         `json:"cap_jitter_max_tokens,omitempty"`
	ScaleMinInputTokens     int         `json:"scale_min_input_tokens,omitempty"`
	targetReadRatioSet      bool
}

type reportProfileJSON struct {
	Enabled                 bool        `json:"enabled"`
	SimulateCache           bool        `json:"simulate_cache,omitempty"`
	SynthesizeStablePrefix  bool        `json:"synthesize_stable_prefix,omitempty"`
	TargetReadRatio         *float64    `json:"target_read_ratio,omitempty"`
	Input                   FieldPolicy `json:"input,omitempty"`
	Output                  FieldPolicy `json:"output,omitempty"`
	CacheRead               FieldPolicy `json:"cache_read,omitempty"`
	CacheCreation           FieldPolicy `json:"cache_creation,omitempty"`
	EmitCacheTTLFields      bool        `json:"emit_cache_ttl_fields,omitempty"`
	TokenScale              float64     `json:"token_scale,omitempty"`
	MaxSimulatedInputTokens int         `json:"max_simulated_input_tokens,omitempty"`
	CapJitterMinTokens      int         `json:"cap_jitter_min_tokens,omitempty"`
	CapJitterMaxTokens      int         `json:"cap_jitter_max_tokens,omitempty"`
	ScaleMinInputTokens     int         `json:"scale_min_input_tokens,omitempty"`
}

func (p ReportProfile) MarshalJSONTo(enc *jsontext.Encoder) error {
	var target *float64
	if p.targetReadRatioSet || (p.TargetReadRatio > 0 && p.TargetReadRatio != DefaultOptions().TargetReadRatio) {
		target = &p.TargetReadRatio
	}
	return json.MarshalEncode(enc, reportProfileJSON{
		Enabled:                 p.Enabled,
		SimulateCache:           p.SimulateCache,
		SynthesizeStablePrefix:  p.SynthesizeStablePrefix,
		TargetReadRatio:         target,
		Input:                   p.Input,
		Output:                  p.Output,
		CacheRead:               p.CacheRead,
		CacheCreation:           p.CacheCreation,
		EmitCacheTTLFields:      p.EmitCacheTTLFields,
		TokenScale:              p.TokenScale,
		MaxSimulatedInputTokens: p.MaxSimulatedInputTokens,
		CapJitterMinTokens:      p.CapJitterMinTokens,
		CapJitterMaxTokens:      p.CapJitterMaxTokens,
		ScaleMinInputTokens:     p.ScaleMinInputTokens,
	})
}

func (p *ReportProfile) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var raw reportProfileJSON
	if err := json.UnmarshalDecode(dec, &raw); err != nil {
		return err
	}
	*p = ReportProfile{
		Enabled:                 raw.Enabled,
		SimulateCache:           raw.SimulateCache,
		SynthesizeStablePrefix:  raw.SynthesizeStablePrefix,
		Input:                   raw.Input,
		Output:                  raw.Output,
		CacheRead:               raw.CacheRead,
		CacheCreation:           raw.CacheCreation,
		EmitCacheTTLFields:      raw.EmitCacheTTLFields,
		TokenScale:              raw.TokenScale,
		MaxSimulatedInputTokens: raw.MaxSimulatedInputTokens,
		CapJitterMinTokens:      raw.CapJitterMinTokens,
		CapJitterMaxTokens:      raw.CapJitterMaxTokens,
		ScaleMinInputTokens:     raw.ScaleMinInputTokens,
	}
	if raw.TargetReadRatio != nil {
		p.TargetReadRatio = *raw.TargetReadRatio
		p.targetReadRatioSet = true
	}
	return nil
}

// FieldPolicy controls how a single reported usage field is projected.
//
// Modes:
//   - raw: use the upstream/raw value.
//   - preserve: keep the computed local value.
//   - sample-max: sample a value up to max_tokens.
//   - sample-target: sample around target_tokens with normal_max_multiplier.
type FieldPolicy struct {
	Mode                 string  `json:"mode,omitempty"`
	MaxTokens            int     `json:"max_tokens,omitempty"`
	TargetTokens         int     `json:"target_tokens,omitempty"`
	NormalMaxMultiplier  float64 `json:"normal_max_multiplier,omitempty"`
	MoveDeltaToCacheRead bool    `json:"move_delta_to_cache_read,omitempty"`
}

type MatchedProfile struct {
	Name    string
	Prefix  string
	Profile ReportProfile
}

type ReportUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	CacheCreation5mTokens    int
	CacheCreation1hTokens    int
}

type Simulation struct {
	Usage       Usage
	TargetRatio float64
	Seed        uint64
	Profile     ReportProfile
}

func DefaultReportConfig() ReportConfig {
	base := defaultHighCacheProfile()
	cc := base
	cc.Input = FieldPolicy{Mode: FieldModeSampleMax, MaxTokens: defaultSampledInputMaxTokens, MoveDeltaToCacheRead: true}
	cc.CacheCreation = FieldPolicy{Mode: FieldModeSampleTarget, TargetTokens: 3000, NormalMaxMultiplier: 1.2}
	ha := base
	ha.Input = FieldPolicy{Mode: FieldModeSampleMax, MaxTokens: defaultSampledInputMaxTokens, MoveDeltaToCacheRead: true}
	na := base
	na.Enabled = false
	na.SimulateCache = false

	return ReportConfig{
		Routes: map[string]string{
			"/v1/messages": "default",
			"/api/cc":      "cc",
			"/api/ha":      "ha",
			"/api/na":      "na",
		},
		Profiles: map[string]ReportProfile{
			"default": base,
			"cc":      cc,
			"ha":      ha,
			"na":      na,
		},
	}
}

func defaultHighCacheProfile() ReportProfile {
	return ReportProfile{
		Enabled:                 true,
		SimulateCache:           true,
		SynthesizeStablePrefix:  true,
		TargetReadRatio:         defaultHighCacheReadRatio,
		Input:                   FieldPolicy{Mode: FieldModeRaw},
		Output:                  FieldPolicy{Mode: FieldModeRaw},
		CacheRead:               FieldPolicy{Mode: FieldModePreserve},
		CacheCreation:           FieldPolicy{Mode: FieldModePreserve},
		TokenScale:              1.6,
		MaxSimulatedInputTokens: 300000,
		CapJitterMinTokens:      12000,
		CapJitterMaxTokens:      24000,
		ScaleMinInputTokens:     20000,
		targetReadRatioSet:      true,
	}
}

func LegacyReportConfig(opts Options) ReportConfig {
	return ReportConfig{
		DefaultProfile: "default",
		Routes: map[string]string{
			"/v1/messages": "default",
		},
		Profiles: map[string]ReportProfile{
			"default": {
				Enabled:            opts.Enabled,
				SimulateCache:      opts.Enabled,
				TargetReadRatio:    opts.TargetReadRatio,
				Input:              FieldPolicy{Mode: FieldModePreserve},
				Output:             FieldPolicy{Mode: FieldModePreserve},
				CacheRead:          FieldPolicy{Mode: FieldModePreserve},
				CacheCreation:      FieldPolicy{Mode: FieldModePreserve},
				targetReadRatioSet: true,
			},
		},
	}
}

func (c ReportConfig) Normalized() ReportConfig {
	out := ReportConfig{
		DefaultProfile: strings.TrimSpace(c.DefaultProfile),
		Routes:         make(map[string]string, len(c.Routes)),
		Profiles:       make(map[string]ReportProfile, len(c.Profiles)),
	}
	for path, name := range c.Routes {
		path = normalizePathPrefix(path)
		name = strings.TrimSpace(name)
		if path == "" || name == "" {
			continue
		}
		out.Routes[path] = name
	}
	for name, profile := range c.Profiles {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out.Profiles[name] = profile.Normalized()
	}
	if out.DefaultProfile != "" {
		if _, ok := out.Profiles[out.DefaultProfile]; !ok {
			out.DefaultProfile = ""
		}
	}
	return out
}

func (c ReportConfig) Empty() bool {
	c = c.Normalized()
	return c.DefaultProfile == "" && len(c.Routes) == 0 && len(c.Profiles) == 0
}

func (c ReportConfig) Validate() error {
	seen := make(map[string]string, len(c.Routes))
	for rawPath := range c.Routes {
		normalized := normalizePathPrefix(rawPath)
		if normalized == "" {
			continue
		}
		if prev, ok := seen[normalized]; ok && prev != rawPath {
			return fmt.Errorf("route %q conflicts with %q after normalization to %q", rawPath, prev, normalized)
		}
		seen[normalized] = rawPath
	}
	normalized := c.Normalized()
	for path, name := range normalized.Routes {
		if _, ok := normalized.Profiles[name]; !ok {
			return fmt.Errorf("route %q references unknown profile %q", path, name)
		}
	}
	if normalized.DefaultProfile != "" {
		if _, ok := normalized.Profiles[normalized.DefaultProfile]; !ok {
			return fmt.Errorf("default_profile references unknown profile %q", normalized.DefaultProfile)
		}
	}
	return nil
}

func (c ReportConfig) Match(path string) (MatchedProfile, bool) {
	c = c.Normalized()
	path = normalizeRequestPath(path)
	if path == "" {
		return MatchedProfile{}, false
	}
	var bestPrefix, bestName string
	var prefixes []string
	for prefix := range c.Routes {
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
	for _, prefix := range prefixes {
		if pathMatches(prefix, path) {
			bestPrefix = prefix
			bestName = c.Routes[prefix]
			break
		}
	}
	if bestName == "" {
		bestName = c.DefaultProfile
	}
	if bestName == "" {
		return MatchedProfile{}, false
	}
	profile, ok := c.Profiles[bestName]
	if !ok {
		return MatchedProfile{}, false
	}
	return MatchedProfile{Name: bestName, Prefix: bestPrefix, Profile: profile.Normalized()}, true
}

func (p ReportProfile) Normalized() ReportProfile {
	targetReadRatioSet := reportProfileHasExplicitTargetReadRatio(p)
	p.TargetReadRatio = normalizeRatio(p.TargetReadRatio, DefaultOptions().TargetReadRatio, targetReadRatioSet)
	p.targetReadRatioSet = targetReadRatioSet
	p.Input = p.Input.normalized(FieldModePreserve)
	p.Output = p.Output.normalized(FieldModePreserve)
	p.CacheRead = p.CacheRead.normalized(FieldModePreserve)
	p.CacheCreation = p.CacheCreation.normalized(FieldModePreserve)
	p.TokenScale = normalizeTokenScale(p.TokenScale)
	p.MaxSimulatedInputTokens = max(p.MaxSimulatedInputTokens, 0)
	p.CapJitterMinTokens = max(p.CapJitterMinTokens, 0)
	p.CapJitterMaxTokens = max(p.CapJitterMaxTokens, 0)
	if p.CapJitterMinTokens > p.CapJitterMaxTokens {
		p.CapJitterMinTokens, p.CapJitterMaxTokens = p.CapJitterMaxTokens, p.CapJitterMinTokens
	}
	p.ScaleMinInputTokens = max(p.ScaleMinInputTokens, 0)
	return p
}

func (p ReportProfile) ReportsCache() bool {
	if !p.Enabled {
		return false
	}
	return p.SimulateCache || p.CacheRead.Mode != FieldModeRaw || p.CacheCreation.Mode != FieldModeRaw
}

func (p FieldPolicy) normalized(defaultMode string) FieldPolicy {
	p.Mode = strings.TrimSpace(strings.ToLower(p.Mode))
	if p.Mode == "" {
		p.Mode = defaultMode
	}
	switch p.Mode {
	case FieldModeRaw, FieldModePreserve, FieldModeSampleMax, FieldModeSampleTarget:
	default:
		p.Mode = defaultMode
	}
	p.MaxTokens = max(p.MaxTokens, 0)
	p.TargetTokens = max(p.TargetTokens, 0)
	if !isFinite(p.NormalMaxMultiplier) || p.NormalMaxMultiplier < 1 {
		p.NormalMaxMultiplier = 1.1
	}
	return p
}

func BuildOptionsForProfile(profile ReportProfile) BuildOptions {
	targetReadRatioSet := reportProfileHasExplicitTargetReadRatio(profile)
	profile = profile.Normalized()
	targetReadRatio := profile.TargetReadRatio
	if !targetReadRatioSet {
		targetReadRatio = 0
	}
	return BuildOptions{
		SynthesizeStablePrefix: profile.SynthesizeStablePrefix,
		TargetReadRatio:        targetReadRatio,
		hasTargetReadRatio:     targetReadRatioSet,
	}
}

func reportProfileHasExplicitTargetReadRatio(p ReportProfile) bool {
	return p.targetReadRatioSet || (p.TargetReadRatio > 0 && p.TargetReadRatio != DefaultOptions().TargetReadRatio)
}

func SimulationFromPlan(plan *Plan, profile ReportProfile) (Simulation, bool) {
	if plan == nil || plan.Usage().Empty() {
		return Simulation{}, false
	}
	profile = profile.Normalized()
	return Simulation{
		Usage:       plan.Usage(),
		TargetRatio: plan.targetReadRatio(),
		Seed:        plan.Seed(),
		Profile:     profile,
	}, true
}

func ApplyReportProfile(raw ReportUsage, sim Simulation, hasSim bool) (ReportUsage, bool) {
	profile := sim.Profile.Normalized()
	raw = raw.normalized()
	if !profile.Enabled {
		return raw, false
	}
	computed := raw
	usedSimulation := false
	if hasSim && raw.CacheReadInputTokens <= 0 && raw.CacheCreationInputTokens <= 0 && !sim.Usage.Empty() {
		computed = sim.toReportUsage(raw)
		usedSimulation = computed.hasCache()
	}
	if !computed.hasCache() && !profile.ReportsCache() {
		return raw.withoutCache(), false
	}
	if !computed.hasCache() {
		return raw, false
	}
	seed := sim.Seed
	if seed == 0 {
		seed = usageSeed(raw, computed)
	}
	usage := computed
	usage.InputTokens = inputBaseValue(profile.Input, computed.InputTokens, raw.InputTokens)
	usage.OutputTokens = fieldBaseValue(profile.Output, computed.OutputTokens, raw.OutputTokens)
	usage.CacheReadInputTokens = fieldBaseValue(profile.CacheRead, computed.CacheReadInputTokens, raw.CacheReadInputTokens)
	if profile.CacheCreation.Mode == FieldModeRaw {
		usage.CacheCreationInputTokens = raw.CacheCreationInputTokens
		usage.CacheCreation5mTokens = raw.CacheCreation5mTokens
		usage.CacheCreation1hTokens = raw.CacheCreation1hTokens
	}
	if v, ok := sampleField(profile.Output, usage, usage.OutputTokens, seed, 0x6d2b79f5aa5421d1); ok {
		usage.OutputTokens = v
	}
	if v, ok := sampleField(profile.CacheRead, usage, usage.CacheReadInputTokens, seed, 0x94d049bb133111eb); ok {
		usage.CacheReadInputTokens = v
	}
	if v, ok := sampleCacheCreation(profile.CacheCreation, usage, seed); ok {
		usage.CacheCreationInputTokens = v
		usage.CacheCreation5mTokens, usage.CacheCreation1hTokens = capCacheCreationBreakdown(
			usage.CacheCreation5mTokens,
			usage.CacheCreation1hTokens,
			v,
		)
	}
	currentInput := max(usage.InputTokens, 0)
	if currentInput > 0 {
		if v, ok := sampleField(profile.Input, usage, currentInput, seed, 0xa24baed4963ee407); ok {
			v = min(max(v, 1), currentInput)
			delta := currentInput - v
			usage.InputTokens = v
			if profile.Input.MoveDeltaToCacheRead {
				usage.CacheReadInputTokens = max(usage.CacheReadInputTokens, 0) + delta
			}
		}
	}
	usage = usage.normalized()
	if !profile.EmitCacheTTLFields {
		usage.CacheCreation5mTokens = 0
		usage.CacheCreation1hTokens = 0
	}
	return usage, usedSimulation
}

func (s Simulation) toReportUsage(raw ReportUsage) ReportUsage {
	profile := s.Profile.Normalized()
	raw = raw.normalized()
	total := raw.InputTokens
	if total <= 0 {
		total = raw.reportedTotalInputTokens()
	}
	basis := applyAmplification(total, profile, s.Seed)
	target := int(math.Round(float64(max(basis, 0)) * clampFloat(s.TargetRatio, 0, 0.99)))
	target = max(target, 0)
	hasRead := s.Usage.CacheReadInputTokens > 0
	hasCreation := s.Usage.CacheCreationInputTokens > 0
	read, creation := 0, 0
	switch {
	case hasRead && hasCreation:
		read = min(s.Usage.CacheReadInputTokens, target)
		creation = max(target-read, 0)
	case hasRead:
		read = target
	case hasCreation:
		creation = target
	}
	cache1h := 0
	if s.Usage.CacheCreation1hInputTokens > 0 {
		cache1h = creation
	}
	cache5m := max(creation-cache1h, 0)
	return ReportUsage{
		InputTokens:              total,
		OutputTokens:             raw.OutputTokens,
		CacheReadInputTokens:     read,
		CacheCreationInputTokens: creation,
		CacheCreation5mTokens:    cache5m,
		CacheCreation1hTokens:    cache1h,
	}.normalized()
}

func (u ReportUsage) normalized() ReportUsage {
	u.InputTokens = max(u.InputTokens, 0)
	u.OutputTokens = max(u.OutputTokens, 0)
	u.CacheReadInputTokens = max(u.CacheReadInputTokens, 0)
	u.CacheCreationInputTokens = max(u.CacheCreationInputTokens, 0)
	u.CacheCreation5mTokens = max(u.CacheCreation5mTokens, 0)
	u.CacheCreation1hTokens = max(u.CacheCreation1hTokens, 0)
	u.CacheCreation5mTokens = min(u.CacheCreation5mTokens, u.CacheCreationInputTokens)
	u.CacheCreation1hTokens = min(u.CacheCreation1hTokens, max(u.CacheCreationInputTokens-u.CacheCreation5mTokens, 0))
	return u
}

func (u ReportUsage) withoutCache() ReportUsage {
	u = u.normalized()
	u.CacheReadInputTokens = 0
	u.CacheCreationInputTokens = 0
	u.CacheCreation5mTokens = 0
	u.CacheCreation1hTokens = 0
	return u
}

func (u ReportUsage) hasCache() bool {
	return u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0
}

func (u ReportUsage) reportedTotalInputTokens() int {
	u = u.normalized()
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

func inputBaseValue(field FieldPolicy, computed, raw int) int {
	switch field.normalized(FieldModePreserve).Mode {
	case FieldModeRaw, FieldModeSampleMax, FieldModeSampleTarget:
		return max(raw, 0)
	default:
		return max(computed, 0)
	}
}

func fieldBaseValue(field FieldPolicy, computed, raw int) int {
	switch field.normalized(FieldModePreserve).Mode {
	case FieldModeRaw:
		return max(raw, 0)
	default:
		return max(computed, 0)
	}
}

func sampleCacheCreation(field FieldPolicy, usage ReportUsage, seed uint64) (int, bool) {
	field = field.normalized(FieldModePreserve)
	rawCreation := max(usage.CacheCreationInputTokens, 0)
	if rawCreation <= 0 {
		return 0, false
	}
	target, multiplier, ok := samplePolicyTarget(field)
	if !ok || target <= 0 {
		return 0, false
	}
	normalMax := normalMaxTokens(target, multiplier)
	effectiveMax := min(rawCreation, normalMax)
	if effectiveMax <= 0 {
		return 0, false
	}
	random := randomForUsage(seed, usage, 0xd6e8feb86659fd93)
	bucketRoll := int(random % 100)
	valueRoll := splitmix64(random ^ 0x9e3779b97f4a7c15)
	if usage.CacheReadInputTokens > 0 && bucketRoll < 20 {
		return 0, true
	}
	var buckets [][3]int
	if usage.CacheReadInputTokens > 0 {
		buckets = [][3]int{{45, 1, 10}, {75, 11, 45}, {93, 46, 85}, {100, 86, 100}}
	} else {
		buckets = [][3]int{{35, 1, 12}, {70, 13, 50}, {92, 51, 88}, {100, 89, 100}}
	}
	lowPct, highPct := bucketRange(bucketRoll, buckets)
	low := max(percentOf(normalMax, lowPct), 1)
	high := max(percentOf(normalMax, highPct), low)
	return sampleInRange(valueRoll, low, high, effectiveMax), true
}

func sampleField(field FieldPolicy, usage ReportUsage, currentValue int, seed, salt uint64) (int, bool) {
	field = field.normalized(FieldModePreserve)
	currentValue = max(currentValue, 0)
	if currentValue <= 0 {
		return 0, false
	}
	target, multiplier, ok := samplePolicyTarget(field)
	if !ok || target <= 0 {
		return 0, false
	}
	maxTokens := min(normalMaxTokens(target, multiplier), currentValue)
	if maxTokens <= 0 {
		return 0, false
	}
	random := randomForUsage(seed, usage, salt)
	bucketRoll := int(random % 100)
	valueRoll := splitmix64(random ^ salt)
	lowPct, highPct := bucketRange(bucketRoll, [][3]int{{70, 2, 25}, {95, 26, 70}, {100, 71, 100}})
	low := max(percentOf(maxTokens, lowPct), 1)
	high := max(percentOf(maxTokens, highPct), low)
	return sampleInRange(valueRoll, low, high, maxTokens), true
}

func samplePolicyTarget(field FieldPolicy) (target int, multiplier float64, ok bool) {
	switch field.Mode {
	case FieldModeSampleMax:
		return field.MaxTokens, 1, true
	case FieldModeSampleTarget:
		return field.TargetTokens, field.NormalMaxMultiplier, true
	default:
		return 0, 0, false
	}
}

func normalMaxTokens(target int, multiplier float64) int {
	if multiplier < 1 || !isFinite(multiplier) {
		multiplier = 1
	}
	v := math.Round(float64(max(target, 0)) * multiplier)
	if v < 1 {
		return 1
	}
	if v > float64(math.MaxInt) {
		return math.MaxInt
	}
	return int(v)
}

func bucketRange(roll int, buckets [][3]int) (lowPct, highPct int) {
	for _, bucket := range buckets {
		if roll < bucket[0] {
			return bucket[1], bucket[2]
		}
	}
	last := buckets[len(buckets)-1]
	return last[1], last[2]
}

func percentOf(value, percent int) int {
	return int((int64(value) * int64(percent)) / 100)
}

func sampleInRange(random uint64, low, high, effectiveMax int) int {
	effectiveMax = max(effectiveMax, 1)
	low = min(max(low, 1), effectiveMax)
	high = min(max(high, 1), effectiveMax)
	if low > high {
		low = 1
		high = effectiveMax
	}
	span := uint64(high - low + 1)
	return low + int(random%span)
}

func capCacheCreationBreakdown(cache5m, cache1h, limit int) (int, int) {
	limit = max(limit, 0)
	cache5m = max(cache5m, 0)
	cache1h = max(cache1h, 0)
	total := cache5m + cache1h
	if limit <= 0 || total <= 0 {
		return 0, 0
	}
	if total <= limit {
		return cache5m, cache1h
	}
	capped5m := int((int64(cache5m) * int64(limit)) / int64(total))
	return capped5m, limit - capped5m
}

func applyAmplification(base int, profile ReportProfile, seed uint64) int {
	base = max(base, 0)
	if base <= 1 || base < profile.ScaleMinInputTokens {
		return base
	}
	scaled := int(math.Round(float64(base) * profile.TokenScale))
	scaled = max(scaled, base)
	if profile.MaxSimulatedInputTokens <= 1 || scaled <= profile.MaxSimulatedInputTokens {
		return scaled
	}
	jitter := capJitter(profile, seed)
	softCap := min(max(profile.MaxSimulatedInputTokens-jitter, 1), profile.MaxSimulatedInputTokens)
	return min(scaled, softCap)
}

func capJitter(profile ReportProfile, seed uint64) int {
	if profile.CapJitterMaxTokens <= 0 || profile.MaxSimulatedInputTokens <= 1 {
		return 0
	}
	relativeMax := int(math.Round(float64(profile.MaxSimulatedInputTokens) * 0.08))
	maxJitter := min(min(profile.CapJitterMaxTokens, relativeMax), profile.MaxSimulatedInputTokens-1)
	maxJitter = max(maxJitter, 0)
	if maxJitter <= 0 {
		return 0
	}
	minJitter := min(max(profile.CapJitterMinTokens, 0), maxJitter)
	return minJitter + int(seed%uint64(maxJitter-minJitter+1))
}

func usageSeed(raw, computed ReportUsage) uint64 {
	h := fnv.New64a()
	for _, v := range []int{
		raw.InputTokens, raw.OutputTokens, raw.CacheReadInputTokens, raw.CacheCreationInputTokens,
		computed.InputTokens, computed.OutputTokens, computed.CacheReadInputTokens, computed.CacheCreationInputTokens,
	} {
		var buf [8]byte
		x := uint64(max(v, 0))
		for i := 0; i < 8; i++ {
			buf[i] = byte(x >> (8 * i))
		}
		_, _ = h.Write(buf[:])
	}
	return h.Sum64()
}

func randomForUsage(seed uint64, usage ReportUsage, salt uint64) uint64 {
	state := seed ^ salt
	for _, value := range []int{
		usage.reportedTotalInputTokens(),
		usage.InputTokens,
		usage.OutputTokens,
		usage.CacheCreationInputTokens,
		usage.CacheReadInputTokens,
		usage.CacheCreation5mTokens,
		usage.CacheCreation1hTokens,
	} {
		state = splitmix64(state ^ uint64(max(value, 0)))
	}
	return state
}

func splitmix64(value uint64) uint64 {
	value += 0x9e3779b97f4a7c15
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func normalizePathPrefix(path string) string {
	path = normalizeRequestPath(path)
	if path == "" || path == "/" {
		return ""
	}
	if path == "/v1" || strings.HasPrefix(path, "/v1/") {
		return strings.TrimRight(path, "/")
	}
	if path == customRoutePrefix {
		return ""
	}
	if strings.HasPrefix(path, customRoutePrefix+"/") {
		return strings.TrimRight(path, "/")
	}
	return customRoutePrefix + strings.TrimRight(path, "/")
}

func normalizeRequestPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

func pathMatches(prefix, path string) bool {
	prefix = normalizePathPrefix(prefix)
	path = normalizeRequestPath(path)
	if prefix == "" || path == "" {
		return false
	}
	if path == prefix || strings.HasPrefix(path, prefix+"/") {
		return true
	}
	for _, suffix := range []string{"/v1/messages/count_tokens", "/v1/messages"} {
		if strings.HasSuffix(path, suffix) && strings.TrimSuffix(path, suffix) == prefix {
			return true
		}
	}
	return false
}

func normalizeRatio(value, fallback float64, explicit bool) float64 {
	if !isFinite(value) || value < 0 || (value == 0 && !explicit) {
		value = fallback
	}
	return clampFloat(value, 0, 0.99)
}

func normalizeTokenScale(value float64) float64 {
	if !isFinite(value) || value <= 0 {
		return 1
	}
	return clampFloat(value, 1, 3)
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
