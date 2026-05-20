package pool

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
)

// ErrEmpty is returned by LoadFromJSON when the file parses successfully but
// contains zero credentials.
var ErrEmpty = errors.New("pool: credential file is empty")

// LoadFromJSON reads a top-level JSON array of CredentialFile entries and
// returns the corresponding *Credential slice. Default Priority is 100 when
// the file specifies zero. Duplicate IDs produce a wrapped error.
func LoadFromJSON(path string) ([]*Credential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pool: read creds file: %w", err)
	}

	var files []CredentialFile
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, fmt.Errorf("pool: parse creds file: %w", err)
	}
	if len(files) == 0 {
		return nil, ErrEmpty
	}

	seen := make(map[string]struct{}, len(files))
	out := make([]*Credential, 0, len(files))
	for i, f := range files {
		if f.ID == "" {
			return nil, fmt.Errorf("pool: creds file: entry %d missing id", i)
		}
		if _, dup := seen[f.ID]; dup {
			return nil, fmt.Errorf("pool: duplicate credential id %q", f.ID)
		}
		seen[f.ID] = struct{}{}

		c, err := credentialFromFile(&f)
		if err != nil {
			return nil, fmt.Errorf("pool: creds entry %q: %w", f.ID, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// SaveToJSON writes creds to path atomically (temp file in same directory,
// then os.Rename).
func SaveToJSON(path string, creds []*Credential) error {
	files := make([]CredentialFile, 0, len(creds))
	for _, c := range creds {
		files = append(files, credentialToFile(c))
	}

	data, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("pool: marshal creds: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".creds-*.json.tmp")
	if err != nil {
		return fmt.Errorf("pool: create temp creds file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("pool: write temp creds file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("pool: sync temp creds file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("pool: close temp creds file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("pool: rename temp creds file: %w", err)
	}
	return nil
}

// credentialFromFile constructs a *Credential from on-disk shape.
func credentialFromFile(f *CredentialFile) (*Credential, error) {
	tok := f.KiroAuthTokenRaw

	var expiresUnix int64
	if tok.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, tok.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("parse expiresAt: %w", err)
		}
		expiresUnix = t.Unix()
	}

	authType := strings.ToLower(strings.TrimSpace(tok.AuthMethod))
	switch authType {
	case "social":
		authType = "social"
	case "idc", "iam", "oidc":
		// upstream "IAM" exports are IDC tokens; normalize.
		authType = "idc"
	case "":
		authType = "idc"
	default:
		authType = strings.ToLower(authType)
	}

	priority := f.Priority
	if priority == 0 {
		priority = 100
	}

	provider := strings.ToLower(strings.TrimSpace(f.Provider))
	if provider == "" {
		provider = "kiro"
	}
	c := &Credential{
		ID:             f.ID,
		Label:          f.Label,
		Provider:       provider,
		Priority:       priority,
		Disabled:       f.Disabled,
		DisableCooling: f.DisableCooling,
		ProxyURL:       strings.TrimSpace(f.ProxyURL),
		Credentials: auth.Credentials{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    expiresUnix,
			Region:       tok.Region,
			SSORegion:    tok.SSORegion,
			ClientID:     tok.ClientID,
			ClientSecret: tok.ClientSecret,
			ProfileARN:   tok.ProfileARN,
			AuthType:     authType,
		},
	}
	return c, nil
}

// credentialToFile converts a *Credential back to the on-disk shape.
func credentialToFile(c *Credential) CredentialFile {
	c.Mu.RLock()
	defer c.Mu.RUnlock()

	authMethod := "Social"
	if c.AuthType != "social" {
		authMethod = "IDC"
	}

	expires := ""
	if c.ExpiresAt > 0 {
		expires = time.Unix(c.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}

	provider := c.Provider
	if provider == "" {
		provider = "kiro"
	}
	return CredentialFile{
		ID:             c.ID,
		Label:          c.Label,
		Provider:       provider,
		Priority:       c.Priority,
		Disabled:       c.Disabled,
		DisableCooling: c.DisableCooling,
		ProxyURL:       c.ProxyURL,
		KiroAuthTokenRaw: KiroAuthTokenRaw{
			AccessToken:  c.AccessToken,
			RefreshToken: c.RefreshToken,
			ExpiresAt:    expires,
			ProfileARN:   c.ProfileARN,
			AuthMethod:   authMethod,
			Region:       c.Region,
			SSORegion:    c.SSORegion,
			ClientID:     c.ClientID,
			ClientSecret: c.ClientSecret,
		},
	}
}
