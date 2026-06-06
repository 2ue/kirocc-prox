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
	t.Setenv("KIROCC_ADMIN_KEY", "admin-secret")
	t.Setenv("KIROCC_POSTGRES_DSN", "postgres://kirocc:pg-secret@postgres:5432/kirocc_pro?sslmode=disable")
	t.Setenv("KIROCC_REDIS_KEY_PREFIX", "kirocc:test:")
	t.Setenv("NOT_KIROCC", "visible")

	got := collectKiroEnv()
	if got["KIROCC_FORCE_THINKING_BUDGET"] != "100000" {
		t.Fatalf("budget env = %q", got["KIROCC_FORCE_THINKING_BUDGET"])
	}
	if got["KIROCC_API_KEY"] != "<redacted>" {
		t.Fatalf("api key should be redacted, got %q", got["KIROCC_API_KEY"])
	}
	if got["KIROCC_ADMIN_KEY"] != "<redacted>" {
		t.Fatalf("admin key should be redacted, got %q", got["KIROCC_ADMIN_KEY"])
	}
	if got["KIROCC_POSTGRES_DSN"] != "postgres://kirocc:xxxxx@postgres:5432/kirocc_pro?sslmode=disable" {
		t.Fatalf("postgres dsn should redact only the password, got %q", got["KIROCC_POSTGRES_DSN"])
	}
	if got["KIROCC_REDIS_KEY_PREFIX"] != "kirocc:test:" {
		t.Fatalf("redis key prefix should not be redacted, got %q", got["KIROCC_REDIS_KEY_PREFIX"])
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
