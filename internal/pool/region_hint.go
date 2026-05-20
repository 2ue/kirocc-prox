package pool

import "context"

// regionHintKey is the context key used to carry a preferred region hint
// from the upstream handler down into Conductor.Acquire. Empty hint =
// no preference; falls back to the configured selector strategy.
type regionHintKey struct{}

// WithRegionHint returns a child context that asks the conductor to
// prefer credentials in the named region. The hint is an AWS-style
// region string (e.g. "us-east-1") matched against Credential.Region.
// Empty region returns ctx unchanged.
func WithRegionHint(ctx context.Context, region string) context.Context {
	if region == "" {
		return ctx
	}
	return context.WithValue(ctx, regionHintKey{}, region)
}

// RegionHintFrom returns the preferred region attached by WithRegionHint,
// or "" when none is set.
func RegionHintFrom(ctx context.Context) string {
	v, _ := ctx.Value(regionHintKey{}).(string)
	return v
}

// FilterByRegion returns the subset of creds whose Region exactly
// matches region. If the exact-match set is empty, falls back to a
// prefix-based "broader continent" match (us-east-1 → all us-*), and
// finally falls back to the full input. This means Conductor always
// has at least one candidate to hand to the selector, but biases
// toward the user's nearest region when available.
func FilterByRegion(creds []*Credential, region string) []*Credential {
	if region == "" || len(creds) == 0 {
		return creds
	}
	exact := make([]*Credential, 0, len(creds))
	for _, c := range creds {
		c.Mu.RLock()
		match := c.Region == region
		c.Mu.RUnlock()
		if match {
			exact = append(exact, c)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	// Broader fallback: any cred whose region shares the same continent
	// prefix (us-, eu-, ap-, sa-, af-, me-). "us-east-1" → keep "us-west-2"
	// before giving up.
	prefix := regionContinent(region)
	if prefix == "" {
		return creds
	}
	broad := make([]*Credential, 0, len(creds))
	for _, c := range creds {
		c.Mu.RLock()
		match := regionContinent(c.Region) == prefix
		c.Mu.RUnlock()
		if match {
			broad = append(broad, c)
		}
	}
	if len(broad) > 0 {
		return broad
	}
	return creds
}

// regionContinent extracts the AWS-style continent prefix ("us", "eu",
// "ap", etc.) from a region string. Returns "" for unparseable inputs.
func regionContinent(region string) string {
	if i := indexByte(region, '-'); i > 0 {
		return region[:i]
	}
	return ""
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
