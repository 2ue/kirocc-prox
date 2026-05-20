// Package georegion maps a client IP address to a preferred AWS-style
// region (e.g. "us-east-1"), backed by a MaxMind GeoLite2-Country MMDB
// file. The resolver is optional; when no path is supplied the resolver
// is disabled and all lookups return "".
//
// Country → region table is intentionally coarse — Kiro only serves a
// handful of regions and the mapping is "pick the geographically closest
// available region", not "exact match".
package georegion

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/oschwald/maxminddb-golang"
)

// Resolver maps IPs to a coarse AWS-style region. The zero value is
// disabled — RegionForIP always returns "". Use New to load an MMDB
// file.
type Resolver struct {
	db   *maxminddb.Reader
	path string
	mu   sync.RWMutex
	// metadata cache for the runtime status display
	loaded     bool
	dbType     string
	buildEpoch int64
	nodeCount  uint64
}

// Disabled returns a Resolver that returns "" for every IP. Useful when
// no MMDB is configured so callers don't need nil-checks.
func Disabled() *Resolver { return &Resolver{} }

// New opens the MMDB at path and returns a ready Resolver. Empty path
// returns a Disabled resolver.
func New(path string) (*Resolver, error) {
	if strings.TrimSpace(path) == "" {
		return Disabled(), nil
	}
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("georegion: open %s: %w", path, err)
	}
	return &Resolver{
		db:         r,
		path:       path,
		loaded:     true,
		dbType:     r.Metadata.DatabaseType,
		buildEpoch: int64(r.Metadata.BuildEpoch),
		nodeCount:  uint64(r.Metadata.NodeCount),
	}, nil
}

// Close releases the underlying MMDB handle.
func (r *Resolver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	r.loaded = false
	return err
}

// Enabled reports whether the resolver has a database loaded.
func (r *Resolver) Enabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.db != nil
}

// Status is a snapshot of the loaded MMDB metadata for UI display.
type Status struct {
	Loaded     bool   `json:"loaded"`
	Path       string `json:"path,omitempty"`
	DBType     string `json:"db_type,omitempty"`     // e.g. "GeoLite2-Country"
	BuildEpoch int64  `json:"build_epoch,omitempty"` // unix seconds; lets the UI show "data 32 days old"
	Nodes      uint64 `json:"nodes,omitempty"`
}

// Status returns the current loaded metadata (zero values if disabled).
func (r *Resolver) Status() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Status{
		Loaded:     r.loaded,
		Path:       r.path,
		DBType:     r.dbType,
		BuildEpoch: r.buildEpoch,
		Nodes:      r.nodeCount,
	}
}

// countryRecord is the minimal MMDB shape we read. MaxMind GeoLite2-Country
// has a nested "country" → "iso_code" path.
type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	Continent struct {
		Code string `maxminddb:"code"`
	} `maxminddb:"continent"`
}

// RegionForIP returns the coarse Kiro region this IP should prefer.
// Returns "" when the resolver is disabled, the IP is private/unknown,
// or the country has no mapping.
func (r *Resolver) RegionForIP(ip net.IP) string {
	if ip == nil {
		return ""
	}
	r.mu.RLock()
	db := r.db
	r.mu.RUnlock()
	if db == nil {
		return ""
	}
	// MMDB silently returns empty record for private IPs; skip them
	// up-front so we don't waste a lookup.
	if isPrivate(ip) {
		return ""
	}
	var rec countryRecord
	if err := db.Lookup(ip, &rec); err != nil {
		return ""
	}
	if rec.Country.ISOCode == "" {
		return continentToRegion(rec.Continent.Code)
	}
	return countryToRegion(rec.Country.ISOCode)
}

// RegionForIPString is a convenience wrapper that parses ip first.
func (r *Resolver) RegionForIPString(ip string) string {
	if i := net.ParseIP(strings.TrimSpace(ip)); i != nil {
		return r.RegionForIP(i)
	}
	return ""
}

// countryToRegion maps a 2-letter ISO country code to the closest Kiro
// region. Coarse on purpose — Kiro currently runs in a handful of AWS
// regions and we just want "is this user closer to us-east-1 or us-west-2".
func countryToRegion(iso string) string {
	switch strings.ToUpper(iso) {
	// === North America – east coast / central ===
	case "US", "CA", "MX":
		// Subdivision granularity (US-CA → us-west-2) would need
		// GeoLite2-City; with City-less data we default the whole
		// continent to us-east-1. Operators can override per request
		// with the settings.Network.PreferredRegion fallback.
		return "us-east-1"
	// === South America ===
	case "BR", "AR", "CL", "CO", "PE", "VE", "UY", "PY", "BO", "EC", "CR", "PA":
		return "sa-east-1"
	// === Western Europe / UK / Nordics ===
	case "GB", "IE", "FR", "DE", "NL", "BE", "LU", "ES", "PT", "IT", "CH", "AT",
		"SE", "NO", "FI", "DK", "IS":
		return "eu-west-1"
	// === Central / Eastern Europe ===
	case "PL", "CZ", "SK", "HU", "RO", "BG", "GR", "TR", "UA", "RU", "BY", "LT", "LV", "EE":
		return "eu-central-1"
	// === Middle East ===
	case "IL", "AE", "SA", "QA", "KW", "BH", "OM", "JO", "EG":
		return "me-south-1"
	// === East Asia ===
	case "JP", "KR":
		return "ap-northeast-1"
	case "CN", "HK", "TW", "MO":
		return "ap-east-1"
	// === SE Asia / Oceania ===
	case "SG", "MY", "ID", "TH", "VN", "PH":
		return "ap-southeast-1"
	case "AU", "NZ":
		return "ap-southeast-2"
	case "IN", "PK", "BD":
		return "ap-south-1"
	// === Africa ===
	case "ZA", "NG", "KE":
		return "af-south-1"
	}
	return ""
}

// continentToRegion is the fallback when a country is recognized by
// continent but not by name (rare, but happens for tiny territories).
func continentToRegion(continent string) string {
	switch strings.ToUpper(continent) {
	case "NA":
		return "us-east-1"
	case "SA":
		return "sa-east-1"
	case "EU":
		return "eu-west-1"
	case "AS":
		return "ap-northeast-1"
	case "OC":
		return "ap-southeast-2"
	case "AF":
		return "af-south-1"
	}
	return ""
}

// isPrivate matches RFC1918 and loopback ranges; we never bother
// looking these up.
func isPrivate(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	return false
}

// ErrDisabled is returned by package-level convenience callers that
// expect an active resolver.
var ErrDisabled = errors.New("georegion: resolver disabled")
