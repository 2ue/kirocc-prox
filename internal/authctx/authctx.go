// Package authctx carries auth-resolution facts (matched API key id,
// device fingerprint) through the request context so downstream handlers
// can attribute usage without re-parsing the Authorization header.
package authctx

import "context"

type apiKeyIDKey struct{}
type deviceIDKey struct{}

// WithAPIKeyID returns a derived context carrying the supplied key id.
func WithAPIKeyID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, apiKeyIDKey{}, id)
}

// APIKeyIDFrom returns the matched dynamic API key id stashed by the
// proxy auth middleware. Empty when the request was authenticated via
// the legacy single -api-key flag or when no auth was required.
func APIKeyIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(apiKeyIDKey{}).(string)
	return v
}

// WithDeviceID returns a derived context carrying a device fingerprint.
func WithDeviceID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, deviceIDKey{}, id)
}

// DeviceIDFrom returns the device fingerprint stashed by the proxy auth
// middleware. Empty when no fingerprint was attached.
func DeviceIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(deviceIDKey{}).(string)
	return v
}
