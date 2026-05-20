package codex

import (
	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/pool"
)

func makeCred(access, refresh string, expiresAt int64) *pool.Credential {
	return &pool.Credential{
		ID:       "test-cred",
		Provider: ID,
		Priority: 100,
		Credentials: auth.Credentials{
			AccessToken:  access,
			RefreshToken: refresh,
			ExpiresAt:    expiresAt,
			AuthType:     "social",
		},
	}
}
