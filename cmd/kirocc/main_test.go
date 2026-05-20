package main

import (
	"context"
	"testing"
)

func TestRun_HelpFlagReturnsNoError(t *testing.T) {
	if err := run(context.Background(), []string{"-h"}); err != nil {
		t.Errorf("run with -h: got err %v; want nil", err)
	}
}

func TestCollectKiroEnvFiltersAndRedacts(t *testing.T) {
	t.Setenv("KIROCC_FORCE_THINKING_BUDGET", "100000")
	t.Setenv("KIROCC_API_KEY", "secret")
	t.Setenv("NOT_KIROCC", "visible")

	got := collectKiroEnv()
	if got["KIROCC_FORCE_THINKING_BUDGET"] != "100000" {
		t.Fatalf("budget env = %q", got["KIROCC_FORCE_THINKING_BUDGET"])
	}
	if got["KIROCC_API_KEY"] != "<redacted>" {
		t.Fatalf("api key should be redacted, got %q", got["KIROCC_API_KEY"])
	}
	if _, ok := got["NOT_KIROCC"]; ok {
		t.Fatal("non-KIROCC env should not be included")
	}
}

func TestThinkingPrefixModeDefault(t *testing.T) {
	t.Setenv("KIROCC_EXPERIMENT_THINKING_PROMPT", "")
	if got := thinkingPrefixMode(); got != "default" {
		t.Fatalf("mode = %q", got)
	}
	t.Setenv("KIROCC_EXPERIMENT_THINKING_PROMPT", "minimal")
	if got := thinkingPrefixMode(); got != "minimal" {
		t.Fatalf("mode = %q", got)
	}
}
