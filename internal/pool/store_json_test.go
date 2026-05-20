package pool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoadFromJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := `[
		{
			"id": "kiro-alice-001",
			"label": "alice@example.com (Pro)",
			"priority": 100,
			"disabled": false,
			"disable_cooling": false,
			"kiro_auth_token_raw": {
				"accessToken": "AT-1",
				"refreshToken": "RT-1",
				"expiresAt": "2026-05-20T10:00:00Z",
				"profileArn": "arn:aws:codewhisperer:us-east-1:000000000000:profile/EXAMPLE",
				"authMethod": "Social",
				"region": "us-east-1"
			}
		},
		{
			"id": "kiro-bob-002",
			"label": "bob",
			"priority": 50,
			"disable_cooling": true,
			"kiro_auth_token_raw": {
				"accessToken": "AT-2",
				"refreshToken": "RT-2",
				"expiresAt": "2027-01-01T00:00:00Z",
				"profileArn": "arn:bob",
				"authMethod": "IDC",
				"region": "us-west-2",
				"ssoRegion": "us-west-2",
				"clientId": "cid",
				"clientSecret": "csec"
			}
		}
	]`
	in := writeFile(t, dir, "creds.json", src)

	creds, err := LoadFromJSON(in)
	if err != nil {
		t.Fatalf("LoadFromJSON: %v", err)
	}
	if len(creds) != 2 {
		t.Fatalf("got %d creds want 2", len(creds))
	}

	a := creds[0]
	if a.ID != "kiro-alice-001" {
		t.Errorf("id: %s", a.ID)
	}
	if a.AccessToken != "AT-1" || a.RefreshToken != "RT-1" {
		t.Errorf("tokens: %+v", a.Credentials)
	}
	wantExp, _ := time.Parse(time.RFC3339, "2026-05-20T10:00:00Z")
	if a.ExpiresAt != wantExp.Unix() {
		t.Errorf("expiresAt: %d want %d", a.ExpiresAt, wantExp.Unix())
	}
	if a.AuthType != "social" {
		t.Errorf("authType: %s", a.AuthType)
	}
	if a.Priority != 100 {
		t.Errorf("priority: %d", a.Priority)
	}

	b := creds[1]
	if b.AuthType != "idc" {
		t.Errorf("b.authType: %s", b.AuthType)
	}
	if !b.DisableCooling {
		t.Error("b.DisableCooling lost")
	}
	if b.ClientID != "cid" || b.ClientSecret != "csec" {
		t.Errorf("device reg lost: %+v", b.Credentials)
	}

	// Round trip: save and reload.
	out := filepath.Join(dir, "out.json")
	if err := SaveToJSON(out, creds); err != nil {
		t.Fatalf("SaveToJSON: %v", err)
	}
	reloaded, err := LoadFromJSON(out)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded) != 2 {
		t.Fatalf("reload len = %d", len(reloaded))
	}
	if reloaded[0].AccessToken != a.AccessToken {
		t.Errorf("round-trip token: %q vs %q", reloaded[0].AccessToken, a.AccessToken)
	}
	if reloaded[0].ExpiresAt != a.ExpiresAt {
		t.Errorf("round-trip expiresAt: %d vs %d", reloaded[0].ExpiresAt, a.ExpiresAt)
	}
	if reloaded[1].AuthType != "idc" {
		t.Errorf("round-trip authType: %s", reloaded[1].AuthType)
	}
	if !reloaded[1].DisableCooling {
		t.Error("round-trip disable_cooling lost")
	}
}

func TestLoadFromJSON_DefaultPriority(t *testing.T) {
	dir := t.TempDir()
	src := `[
		{
			"id": "x",
			"kiro_auth_token_raw": {
				"accessToken": "a",
				"refreshToken": "r",
				"expiresAt": "2026-05-20T10:00:00Z",
				"authMethod": "Social"
			}
		}
	]`
	p := writeFile(t, dir, "creds.json", src)
	creds, err := LoadFromJSON(p)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if creds[0].Priority != 100 {
		t.Errorf("default priority: %d want 100", creds[0].Priority)
	}
}

func TestLoadFromJSON_DuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	src := `[
		{"id":"x","kiro_auth_token_raw":{"accessToken":"a","refreshToken":"r","expiresAt":"2026-05-20T10:00:00Z","authMethod":"Social"}},
		{"id":"x","kiro_auth_token_raw":{"accessToken":"b","refreshToken":"r","expiresAt":"2026-05-20T10:00:00Z","authMethod":"Social"}}
	]`
	p := writeFile(t, dir, "creds.json", src)
	_, err := LoadFromJSON(p)
	if err == nil {
		t.Fatal("expected duplicate ID error")
	}
	if !strings.Contains(err.Error(), "duplicate credential id") {
		t.Errorf("error message: %v", err)
	}
}

func TestLoadFromJSON_Empty(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "creds.json", `[]`)
	_, err := LoadFromJSON(p)
	if !errors.Is(err, ErrEmpty) {
		t.Errorf("got %v want ErrEmpty", err)
	}
}

func TestLoadFromJSON_BadFile(t *testing.T) {
	_, err := LoadFromJSON("/no/such/path/creds.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestSaveToJSON_Atomic(t *testing.T) {
	dir := t.TempDir()
	c := &Credential{
		ID:       "x",
		Priority: 100,
		Credentials: auth.Credentials{
			AccessToken:  "AT",
			RefreshToken: "RT",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
			AuthType:     "social",
			Region:       "us-east-1",
		},
	}
	out := filepath.Join(dir, "creds.json")
	if err := SaveToJSON(out, []*Credential{c}); err != nil {
		t.Fatalf("SaveToJSON: %v", err)
	}

	// Verify no .tmp file remains in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasPrefix(name, ".creds-") {
			t.Errorf("temp file left behind: %s", name)
		}
	}
	if len(entries) != 1 || entries[0].Name() != "creds.json" {
		t.Errorf("expected only creds.json, got %v", entries)
	}
}
