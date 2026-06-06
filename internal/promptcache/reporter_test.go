package promptcache

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestReportConfig_MatchesLongestPathProfile(t *testing.T) {
	cfg := ReportConfig{
		Routes: map[string]string{
			"custom-a":         "profile-a",
			"custom-a/special": "profile-b",
		},
		Profiles: map[string]ReportProfile{
			"profile-a": {Enabled: true, SimulateCache: true},
			"profile-b": {Enabled: true, SimulateCache: true, SynthesizeStablePrefix: true},
		},
	}

	got, ok := cfg.Match("/api/custom-a/special/v1/messages")
	if !ok {
		t.Fatal("profile not matched")
	}
	if got.Name != "profile-b" {
		t.Fatalf("profile = %q, want profile-b", got.Name)
	}
}

func TestReportConfig_NormalizesCustomRoutesUnderAPIPrefix(t *testing.T) {
	cfg := ReportConfig{
		Routes: map[string]string{
			"custom-a":                  "profile-a",
			"custom-b":                  "profile-b",
			"api/custom-c":              "profile-c",
			"/api/custom-d":             "profile-d",
			"/v1/messages":              "default",
			"/v1/messages/count_tokens": "count",
		},
		Profiles: map[string]ReportProfile{
			"default":   {Enabled: true},
			"count":     {Enabled: true},
			"profile-a": {Enabled: true},
			"profile-b": {Enabled: true},
			"profile-c": {Enabled: true},
			"profile-d": {Enabled: true},
		},
	}

	normalized := cfg.Normalized()
	for _, want := range []string{"/api/custom-a", "/api/custom-b", "/api/custom-c", "/api/custom-d", "/v1/messages", "/v1/messages/count_tokens"} {
		if _, ok := normalized.Routes[want]; !ok {
			t.Fatalf("normalized routes missing %q: %+v", want, normalized.Routes)
		}
	}
}

func TestReportConfig_ValidateRejectsDuplicateNormalizedRoutes(t *testing.T) {
	cfg := ReportConfig{
		Routes: map[string]string{
			"custom-a":      "profile-a",
			"/api/custom-a": "profile-b",
		},
		Profiles: map[string]ReportProfile{
			"profile-a": {Enabled: true},
			"profile-b": {Enabled: true},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate normalized route error")
	}
}

func TestReportConfig_ValidateRejectsUnknownProfile(t *testing.T) {
	cfg := ReportConfig{
		Routes:   map[string]string{"custom-a": "missing"},
		Profiles: map[string]ReportProfile{"profile-a": {Enabled: true}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unknown profile error")
	}
}

func TestDefaultReportConfig_MatchesKiroRSCompatibleDefaults(t *testing.T) {
	cfg := DefaultReportConfig().Normalized()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	for path, wantName := range map[string]string{
		"/v1/messages":                     "default",
		"/v1/messages/count_tokens":        "default",
		"/api/cc/v1/models":                "cc",
		"/api/cc/v1/messages":              "cc",
		"/api/cc/v1/messages/count_tokens": "cc",
		"/api/ha/v1/messages":              "ha",
		"/api/na/v1/messages":              "na",
	} {
		t.Run(path, func(t *testing.T) {
			got, ok := cfg.Match(path)
			if !ok {
				t.Fatalf("path %q did not match", path)
			}
			if got.Name != wantName {
				t.Fatalf("profile = %q, want %q", got.Name, wantName)
			}
		})
	}

	base := cfg.Profiles["default"]
	assertHighCacheDefaults(t, "default", base)
	if base.Input.Mode != FieldModeRaw || base.Output.Mode != FieldModeRaw {
		t.Fatalf("default input/output = %q/%q, want raw/raw", base.Input.Mode, base.Output.Mode)
	}
	if base.CacheRead.Mode != FieldModePreserve || base.CacheCreation.Mode != FieldModePreserve {
		t.Fatalf("default cache policies = %q/%q, want preserve/preserve", base.CacheRead.Mode, base.CacheCreation.Mode)
	}

	cc := cfg.Profiles["cc"]
	assertHighCacheDefaults(t, "cc", cc)
	if cc.Input.Mode != FieldModeSampleMax || cc.Input.MaxTokens != defaultSampledInputMaxTokens || !cc.Input.MoveDeltaToCacheRead {
		t.Fatalf("cc input policy = %+v, want sample-max %d with move delta", cc.Input, defaultSampledInputMaxTokens)
	}
	if cc.CacheCreation.Mode != FieldModeSampleTarget || cc.CacheCreation.TargetTokens != 3000 || cc.CacheCreation.NormalMaxMultiplier != 1.2 {
		t.Fatalf("cc cache creation policy = %+v, want sample-target 3000 x1.2", cc.CacheCreation)
	}

	ha := cfg.Profiles["ha"]
	assertHighCacheDefaults(t, "ha", ha)
	if ha.Input.Mode != FieldModeSampleMax || ha.Input.MaxTokens != defaultSampledInputMaxTokens || !ha.Input.MoveDeltaToCacheRead {
		t.Fatalf("ha input policy = %+v, want sample-max %d with move delta", ha.Input, defaultSampledInputMaxTokens)
	}
	if ha.CacheCreation.Mode != FieldModePreserve {
		t.Fatalf("ha cache creation policy = %+v, want preserve", ha.CacheCreation)
	}

	na := cfg.Profiles["na"]
	if na.Enabled || na.SimulateCache {
		t.Fatalf("na enabled/simulate_cache = %v/%v, want false/false", na.Enabled, na.SimulateCache)
	}
}

func assertHighCacheDefaults(t *testing.T, name string, p ReportProfile) {
	t.Helper()
	if !p.Enabled || !p.SimulateCache || !p.SynthesizeStablePrefix {
		t.Fatalf("%s enabled/simulate/stable = %v/%v/%v, want true/true/true", name, p.Enabled, p.SimulateCache, p.SynthesizeStablePrefix)
	}
	if p.TargetReadRatio != defaultHighCacheReadRatio {
		t.Fatalf("%s target_read_ratio = %g, want %g", name, p.TargetReadRatio, defaultHighCacheReadRatio)
	}
	if p.TokenScale != 1.6 {
		t.Fatalf("%s token_scale = %g, want 1.6", name, p.TokenScale)
	}
	if p.MaxSimulatedInputTokens != 300000 {
		t.Fatalf("%s max_simulated_input_tokens = %d, want 300000", name, p.MaxSimulatedInputTokens)
	}
	if p.CapJitterMinTokens != 12000 || p.CapJitterMaxTokens != 24000 {
		t.Fatalf("%s cap jitter = %d/%d, want 12000/24000", name, p.CapJitterMinTokens, p.CapJitterMaxTokens)
	}
	if p.ScaleMinInputTokens != 20000 {
		t.Fatalf("%s scale_min_input_tokens = %d, want 20000", name, p.ScaleMinInputTokens)
	}
}

func TestApplyReportProfile_DisabledReportsRaw(t *testing.T) {
	raw := ReportUsage{
		InputTokens:              100,
		OutputTokens:             10,
		CacheReadInputTokens:     50,
		CacheCreationInputTokens: 20,
	}
	sim := Simulation{Profile: ReportProfile{Enabled: false}}

	got, used := ApplyReportProfile(raw, sim, true)
	if used {
		t.Fatal("simulation should not be used")
	}
	if got.CacheReadInputTokens != 50 || got.CacheCreationInputTokens != 20 {
		t.Fatalf("cache = %d/%d, want raw 50/20", got.CacheReadInputTokens, got.CacheCreationInputTokens)
	}
	if got.InputTokens != 100 || got.OutputTokens != 10 {
		t.Fatalf("usage = %+v, want raw input/output", got)
	}
}

func TestApplyReportProfile_SimulatesAndProjectsFields(t *testing.T) {
	raw := ReportUsage{InputTokens: 10_000, OutputTokens: 100}
	profile := ReportProfile{
		Enabled:       true,
		SimulateCache: true,
		Input:         FieldPolicy{Mode: FieldModeSampleMax, MaxTokens: 96, MoveDeltaToCacheRead: true},
		Output:        FieldPolicy{Mode: FieldModeRaw},
		CacheRead:     FieldPolicy{Mode: FieldModePreserve},
		CacheCreation: FieldPolicy{Mode: FieldModeSampleTarget, TargetTokens: 3_000, NormalMaxMultiplier: 1.2},
	}
	sim := Simulation{
		Usage:       Usage{CacheCreationInputTokens: 9_000, CacheCreation5mInputTokens: 9_000},
		TargetRatio: 0.90,
		Seed:        123,
		Profile:     profile,
	}

	got, used := ApplyReportProfile(raw, sim, true)
	if !used {
		t.Fatal("simulation should be used")
	}
	if got.OutputTokens != 100 {
		t.Fatalf("output_tokens = %d, want raw 100", got.OutputTokens)
	}
	if got.InputTokens <= 0 || got.InputTokens > 96 {
		t.Fatalf("input_tokens = %d, want sampled <= 96", got.InputTokens)
	}
	if got.CacheCreationInputTokens <= 0 || got.CacheCreationInputTokens > 3_600 {
		t.Fatalf("cache_creation = %d, want sampled <= 3600", got.CacheCreationInputTokens)
	}
	if got.CacheReadInputTokens <= 0 {
		t.Fatalf("cache_read = %d, want moved delta > 0", got.CacheReadInputTokens)
	}
}

func TestApplyReportProfile_EnabledWithoutSimulationPreservesRawCache(t *testing.T) {
	raw := ReportUsage{
		InputTokens:              100,
		OutputTokens:             10,
		CacheReadInputTokens:     50,
		CacheCreationInputTokens: 20,
	}
	sim := Simulation{Profile: ReportProfile{Enabled: true, SimulateCache: true}}

	got, used := ApplyReportProfile(raw, sim, false)
	if used {
		t.Fatal("simulation should not be used")
	}
	if got.CacheReadInputTokens != 50 || got.CacheCreationInputTokens != 20 {
		t.Fatalf("cache = %d/%d, want raw 50/20", got.CacheReadInputTokens, got.CacheCreationInputTokens)
	}
	if got.InputTokens != 100 || got.OutputTokens != 10 {
		t.Fatalf("usage = %+v, want raw input/output", got)
	}
}

func TestReportProfile_ExplicitZeroTargetReadRatioSurvivesJSONNormalization(t *testing.T) {
	var cfg ReportConfig
	err := json.Unmarshal([]byte(`{
		"routes": {"/zero": "zero"},
		"profiles": {
			"zero": {
				"enabled": true,
				"simulate_cache": true,
				"synthesize_stable_prefix": true,
				"target_read_ratio": 0
			}
		}
	}`), &cfg)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	normalized := cfg.Normalized()
	profile := normalized.Profiles["zero"]
	if profile.TargetReadRatio != 0 {
		t.Fatalf("target_read_ratio = %g, want explicit 0", profile.TargetReadRatio)
	}
	opts := BuildOptionsForProfile(profile)
	if !opts.hasTargetReadRatio || opts.TargetReadRatio != 0 {
		t.Fatalf("build opts = %+v, want explicit zero ratio", opts)
	}

	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"target_read_ratio":0`) {
		t.Fatalf("encoded config %s does not preserve explicit zero target_read_ratio", encoded)
	}
}

func TestReportProfile_DefaultTargetReadRatioCanStayImplicit(t *testing.T) {
	profile := ReportProfile{Enabled: true, SimulateCache: true}.Normalized()
	if profile.TargetReadRatio != DefaultOptions().TargetReadRatio {
		t.Fatalf("target_read_ratio = %g, want default %g", profile.TargetReadRatio, DefaultOptions().TargetReadRatio)
	}
	opts := BuildOptionsForProfile(profile)
	if opts.hasTargetReadRatio {
		t.Fatalf("build opts = %+v, want implicit tracker default", opts)
	}

	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "target_read_ratio") {
		t.Fatalf("encoded implicit profile should not include target_read_ratio: %s", encoded)
	}
}
