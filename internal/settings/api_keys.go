package settings

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrAPIKeyExists is returned when adding a key with a duplicate id or value.
var ErrAPIKeyExists = errors.New("settings: api key already exists")

// ErrAPIKeyNotFound is returned when an id-based op cannot find a key, OR
// when a supplied bearer token does not match any enabled entry.
var ErrAPIKeyNotFound = errors.New("settings: api key not found")

// ErrAPIKeyExpired means the key exists and is enabled, but its
// ExpiresAt is in the past.
var ErrAPIKeyExpired = errors.New("settings: api key expired")

// ErrAPIKeyOverQuota means the key exists and is enabled, but
// UsedTokens >= QuotaLimit. Cleared by raising QuotaLimit or
// resetting UsedTokens.
var ErrAPIKeyOverQuota = errors.New("settings: api key over quota")

// APIKeyOptions is the create payload for AddAPIKey. KeyValue may be
// empty to auto-generate a fresh `sk-...` secret. ExpiresAt = 0 means
// never expires; QuotaLimit = 0 means unlimited tokens.
type APIKeyOptions struct {
	Label      string
	KeyValue   string
	ExpiresAt  int64 // unix seconds; 0 = never
	QuotaLimit int64 // input+output tokens; 0 = unlimited
}

// AddAPIKey atomically appends a new key. Returns the persisted entry
// (including the generated secret) so the UI can show it once before
// masking.
func (s *Store) AddAPIKey(opts APIKeyOptions) (APIKey, error) {
	if opts.KeyValue == "" {
		opts.KeyValue = newAPIKeyValue()
	}
	if err := validateAPIKeyValue(opts.KeyValue); err != nil {
		return APIKey{}, err
	}
	if opts.ExpiresAt < 0 {
		return APIKey{}, errors.New("expires_at must be >= 0")
	}
	if opts.QuotaLimit < 0 {
		return APIKey{}, errors.New("quota_limit must be >= 0")
	}
	id := newAPIKeyID()
	now := time.Now().Unix()

	var out APIKey
	if _, err := s.Update(func(cur *Settings) error {
		for _, k := range cur.APIKeys {
			if k.ID == id || k.Key == opts.KeyValue {
				return ErrAPIKeyExists
			}
		}
		out = APIKey{
			ID:         id,
			Label:      opts.Label,
			Key:        opts.KeyValue,
			Enabled:    true,
			CreatedAt:  now,
			ExpiresAt:  opts.ExpiresAt,
			QuotaLimit: opts.QuotaLimit,
		}
		cur.APIKeys = append(cur.APIKeys, out)
		return nil
	}); err != nil {
		return APIKey{}, err
	}
	return out, nil
}

// APIKeyPatch carries optional mutations to UpdateAPIKey. Nil pointers
// mean "leave this field untouched."
type APIKeyPatch struct {
	Label      string // empty = unchanged
	Enabled    *bool
	ExpiresAt  *int64 // nil = unchanged; 0 = never expires
	QuotaLimit *int64 // nil = unchanged; 0 = unlimited
}

// UpdateAPIKey applies a patch to an existing key. To rotate the
// secret value itself, use RotateAPIKey.
func (s *Store) UpdateAPIKey(id string, patch APIKeyPatch) error {
	_, err := s.Update(func(cur *Settings) error {
		for i := range cur.APIKeys {
			if cur.APIKeys[i].ID == id {
				if patch.Label != "" {
					cur.APIKeys[i].Label = patch.Label
				}
				if patch.Enabled != nil {
					cur.APIKeys[i].Enabled = *patch.Enabled
				}
				if patch.ExpiresAt != nil {
					if *patch.ExpiresAt < 0 {
						return errors.New("expires_at must be >= 0")
					}
					cur.APIKeys[i].ExpiresAt = *patch.ExpiresAt
				}
				if patch.QuotaLimit != nil {
					if *patch.QuotaLimit < 0 {
						return errors.New("quota_limit must be >= 0")
					}
					cur.APIKeys[i].QuotaLimit = *patch.QuotaLimit
				}
				return nil
			}
		}
		return ErrAPIKeyNotFound
	})
	return err
}

// AddAPIKeyUsage atomically bumps UsedTokens on the matching key.
// Returns ErrAPIKeyNotFound when id is unknown.
func (s *Store) AddAPIKeyUsage(id string, tokens int64) error {
	if tokens <= 0 {
		return nil
	}
	_, err := s.Update(func(cur *Settings) error {
		for i := range cur.APIKeys {
			if cur.APIKeys[i].ID == id {
				cur.APIKeys[i].UsedTokens += tokens
				return nil
			}
		}
		return ErrAPIKeyNotFound
	})
	return err
}

// RotateAPIKey replaces the key value for the given id and returns
// the new value. The id (label, enabled, created_at) is preserved.
func (s *Store) RotateAPIKey(id string) (string, error) {
	newVal := newAPIKeyValue()
	_, err := s.Update(func(cur *Settings) error {
		for i := range cur.APIKeys {
			if cur.APIKeys[i].ID == id {
				cur.APIKeys[i].Key = newVal
				return nil
			}
		}
		return ErrAPIKeyNotFound
	})
	if err != nil {
		return "", err
	}
	return newVal, nil
}

// DeleteAPIKey removes a key by id.
func (s *Store) DeleteAPIKey(id string) error {
	_, err := s.Update(func(cur *Settings) error {
		for i := range cur.APIKeys {
			if cur.APIKeys[i].ID == id {
				cur.APIKeys = append(cur.APIKeys[:i], cur.APIKeys[i+1:]...)
				return nil
			}
		}
		return ErrAPIKeyNotFound
	})
	return err
}

// ValidateAPIKey returns the matching APIKey if the supplied secret
// matches an enabled entry AND is not expired and not over quota.
// Constant-time comparison avoids leaking timing info across keys.
//
// Returns:
//   - ErrAPIKeyNotFound: token matches no enabled key
//   - ErrAPIKeyExpired:  token matches a key whose ExpiresAt is past
//   - ErrAPIKeyOverQuota: token matches a key whose UsedTokens >= QuotaLimit
//
// Callers should still check ValidateAPIKey alongside the legacy
// single -api-key flag for backwards compatibility.
func (s *Store) ValidateAPIKey(supplied string) (APIKey, error) {
	if supplied == "" {
		return APIKey{}, ErrAPIKeyNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	suppliedB := []byte(supplied)
	now := time.Now().Unix()
	for _, k := range s.cur.APIKeys {
		if !k.Enabled {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(k.Key), suppliedB) != 1 {
			continue
		}
		if k.ExpiresAt > 0 && k.ExpiresAt <= now {
			return k, ErrAPIKeyExpired
		}
		if k.QuotaLimit > 0 && k.UsedTokens >= k.QuotaLimit {
			return k, ErrAPIKeyOverQuota
		}
		return k, nil
	}
	return APIKey{}, ErrAPIKeyNotFound
}

// MaskAPIKey returns "sk-***...XXXX" for UI display where XXXX is
// the last 4 chars of the key.
func MaskAPIKey(key string) string {
	if len(key) <= 6 {
		return strings.Repeat("*", len(key))
	}
	prefix := key[:2]
	if !strings.HasPrefix(key, "sk-") {
		prefix = key[:2]
	} else {
		prefix = "sk"
	}
	return fmt.Sprintf("%s******%s", prefix, key[len(key)-2:])
}

// newAPIKeyID returns a 12-hex-char id (≈48 bits of entropy — enough
// for unique-ID purposes when paired with the key value itself).
func newAPIKeyID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// newAPIKeyValue returns a fresh OpenAI-style "sk-..." secret. We
// emit 32 url-safe-ish chars (hex) for compatibility with anything
// that has alphanumeric expectations.
func newAPIKeyValue() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "sk-" + hex.EncodeToString(b[:])
}

// validateAPIKeyValue rejects keys that would be unsafe to store or
// trivial to brute-force.
func validateAPIKeyValue(v string) error {
	if len(v) < 8 {
		return errors.New("api key must be at least 8 characters")
	}
	if strings.ContainsAny(v, " \t\n\r") {
		return errors.New("api key must not contain whitespace")
	}
	return nil
}
