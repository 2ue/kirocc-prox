package messages

import (
	"errors"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/usage"
)

func TestMarkPoolOutcome_UpstreamRateLimitHonorsRetryAfter(t *testing.T) {
	sched := pool.NewDefaultScheduler()
	cred := &pool.Credential{ID: "cred-a", Priority: 100}
	sched.Register([]*pool.Credential{cred})
	svc := &Service{scheduler: sched}

	retryAfter := 2 * time.Minute
	svc.markPoolOutcome("cred-a", "claude-sonnet", &kiroclient.UpstreamError{
		Status:     429,
		Exception:  "ThrottlingException",
		Body:       `{"message":"slow down"}`,
		RetryAfter: retryAfter,
	}, newMetricsResponseWriter(nil), 50*time.Millisecond)

	cred.Mu.RLock()
	defer cred.Mu.RUnlock()
	if !cred.Quota.Exceeded {
		t.Fatal("account quota should be exceeded")
	}
	remaining := time.Until(cred.Quota.NextRecoverAt)
	if remaining < retryAfter-5*time.Second || remaining > retryAfter+5*time.Second {
		t.Fatalf("cooldown remaining = %s, want about %s", remaining, retryAfter)
	}
	if cred.ModelStates["claude-sonnet"] == nil || !cred.ModelStates["claude-sonnet"].Quota.Exceeded {
		t.Fatalf("model cooldown not recorded: %+v", cred.ModelStates["claude-sonnet"])
	}
}

func TestUsageStatusFor_TextQuotaWithoutRateLimitIsUpstreamError(t *testing.T) {
	err := errors.New("quota refresh failed: status 500")
	if got := usageStatusFor(err); got != usage.StatusUpstreamError {
		t.Fatalf("usageStatusFor() = %q, want %q", got, usage.StatusUpstreamError)
	}
}

func TestUsageStatusFor_UpstreamError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "rate limit",
			err:  &kiroclient.UpstreamError{Status: 429},
			want: usage.StatusRateLimited,
		},
		{
			name: "auth",
			err:  &kiroclient.UpstreamError{Status: 403},
			want: usage.StatusAuthError,
		},
		{
			name: "server",
			err:  &kiroclient.UpstreamError{Status: 500},
			want: usage.StatusUpstreamError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageStatusFor(tc.err); got != tc.want {
				t.Fatalf("usageStatusFor() = %q, want %q", got, tc.want)
			}
		})
	}
}
