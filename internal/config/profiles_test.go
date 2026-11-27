package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultProfilesConfig(t *testing.T) {
	cfg := DefaultProfilesConfig()
	if cfg.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", cfg.SchemaVersion)
	}
	if cfg.ActiveProfile != "agy-oauth" {
		t.Fatalf("ActiveProfile = %q, want %q", cfg.ActiveProfile, "agy-oauth")
	}
	if err := ValidateProfiles(cfg); err != nil {
		t.Fatalf("ValidateProfiles(DefaultProfilesConfig()) error: %v", err)
	}

	for _, pname := range []string{"deepseek", "9router", "omniroute", "openai-compatible"} {
		if _, ok := cfg.Providers[pname]; !ok {
			t.Fatalf("missing default provider %q", pname)
		}
	}

	if got := cfg.Providers["deepseek"]; got.BaseURL != "https://api.deepseek.com/v1" || got.Compatibility != CompatibilityDeepSeekThinking || got.DefaultModel != "deepseek-v4-pro" || got.FastModel != "deepseek-flash" {
		t.Fatalf("unexpected deepseek provider: %+v", got)
	}
	if got := cfg.Providers["9router"]; got.BaseURL != "http://localhost:20128/v1" || got.BaseURLEnv != "NINEROUTER_BASE_URL" || !got.AllowLoopbackWithoutKey || got.DefaultModel != "kr/claude-sonnet-4.5" {
		t.Fatalf("unexpected 9router provider: %+v", got)
	}
	if got := cfg.Providers["omniroute"]; got.BaseURL != "http://localhost:20128/v1" || got.BaseURLEnv != "OMNIROUTE_BASE_URL" || !got.AllowLoopbackWithoutKey || got.PlaceholderKeyOnLoopback != "omniroute-local" || got.DefaultModel != "cc/claude-opus-4-6" {
		t.Fatalf("unexpected omniroute provider: %+v", got)
	}

	for _, profName := range []string{"agy-oauth", "deepseek-hybrid", "9router-hybrid", "omniroute-hybrid"} {
		prof, ok := cfg.Profiles[profName]
		if !ok {
			t.Fatalf("missing default profile %q", profName)
		}
		for _, role := range AllRoles {
			if _, ok := prof.Roles[role]; !ok {
				t.Fatalf("profile %q missing role %q", profName, role)
			}
		}
	}

	ds := cfg.Profiles["deepseek-hybrid"]
	if ds.Roles["explore"].Runtime != RuntimeAgyNative || ds.Roles["worker"].Runtime != RuntimeAgyNative {
		t.Fatalf("deepseek-hybrid explore/worker must stay agy-native: %+v", ds.Roles)
	}
	for _, extRole := range []string{"verify", "jd-judge-b", "review-risk", "review-resilience", "review-readability", "review-reliability", "review-refuter"} {
		r := ds.Roles[extRole]
		if r.Runtime != RuntimeExternalMCP || r.Provider != "deepseek" || r.Model != "deepseek-v4-pro" {
			t.Fatalf("deepseek-hybrid role %q = %+v", extRole, r)
		}
		if len(r.Fallback) != 1 || r.Fallback[0].Runtime != RuntimeAgyNative || r.Fallback[0].Model != "pro" {
			t.Fatalf("deepseek-hybrid role %q fallback = %+v", extRole, r.Fallback)
		}
	}
}

func TestValidateProfilesRejectsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ProfilesConfig)
	}{
		{"unknown activeProfile", func(c *ProfilesConfig) { c.ActiveProfile = "does-not-exist" }},
		{"empty provider baseUrl", func(c *ProfilesConfig) {
			p := c.Providers["deepseek"]
			p.BaseURL = ""
			c.Providers["deepseek"] = p
		}},
		{"invalid provider compatibility", func(c *ProfilesConfig) {
			p := c.Providers["deepseek"]
			p.Compatibility = "weird"
			c.Providers["deepseek"] = p
		}},
		{"unknown provider reference in role", func(c *ProfilesConfig) {
			prof := c.Profiles["deepseek-hybrid"]
			r := prof.Roles["verify"]
			r.Provider = "nonexistent-provider"
			prof.Roles["verify"] = r
			c.Profiles["deepseek-hybrid"] = prof
		}},
		{"unknown provider reference in fallback", func(c *ProfilesConfig) {
			prof := c.Profiles["deepseek-hybrid"]
			r := prof.Roles["verify"]
			r.Fallback = []RoleTarget{{Runtime: RuntimeExternalMCP, Provider: "ghost", Model: "m"}}
			prof.Roles["verify"] = r
			c.Profiles["deepseek-hybrid"] = prof
		}},
		{"missing role", func(c *ProfilesConfig) {
			prof := c.Profiles["agy-oauth"]
			delete(prof.Roles, "review-refuter")
			c.Profiles["agy-oauth"] = prof
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultProfilesConfig()
			tc.mutate(&cfg)
			if err := ValidateProfiles(cfg); err == nil {
				t.Fatalf("ValidateProfiles(%s) expected error, got nil", tc.name)
			}
		})
	}
}

func TestSaveProfilesPreservesStoredOnRejection(t *testing.T) {
	dir := t.TempDir()
	wantPath := filepath.Join(dir, ".agents", "odd", "profiles.json")
	if ProfilesPath(dir) != wantPath {
		t.Fatalf("ProfilesPath = %q, want %q", ProfilesPath(dir), wantPath)
	}

	cfg := DefaultProfilesConfig()
	cfg.ActiveProfile = "omniroute-hybrid"
	if err := SaveProfiles(dir, cfg); err != nil {
		t.Fatalf("SaveProfiles valid error: %v", err)
	}
	before, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	bad := cfg
	bad.ActiveProfile = "unknown-profile"
	if err := SaveProfiles(dir, bad); err == nil {
		t.Fatalf("SaveProfiles invalid expected error, got nil")
	}
	after, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("ReadFile after failed save: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("profiles.json modified on rejected save")
	}
}

func TestResolveProviderEndpoint(t *testing.T) {
	defaults := DefaultProfilesConfig()

	mapEnv := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			v, ok := m[k]
			return v, ok
		}
	}

	t.Run("9router loopback without key", func(t *testing.T) {
		ep, err := ResolveProviderEndpoint(defaults.Providers["9router"], mapEnv(nil))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.BaseURL != "http://localhost:20128/v1" || !ep.IsLoopback || ep.MissingAPIKey || ep.APIKey != "" {
			t.Fatalf("unexpected 9router endpoint: %+v", ep)
		}
	})

	t.Run("9router baseUrlEnv remote override requires key", func(t *testing.T) {
		ep, err := ResolveProviderEndpoint(defaults.Providers["9router"], mapEnv(map[string]string{
			"NINEROUTER_BASE_URL": "https://router.example.com/v1/",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.BaseURL != "https://router.example.com/v1" || ep.IsLoopback || !ep.MissingAPIKey {
			t.Fatalf("unexpected overridden 9router endpoint: %+v", ep)
		}
	})

	t.Run("omniroute loopback placeholder vs explicit key", func(t *testing.T) {
		ep, err := ResolveProviderEndpoint(defaults.Providers["omniroute"], mapEnv(nil))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.APIKey != "omniroute-local" || ep.MissingAPIKey || !ep.IsLoopback {
			t.Fatalf("unexpected omniroute placeholder endpoint: %+v", ep)
		}

		ep2, err := ResolveProviderEndpoint(defaults.Providers["omniroute"], mapEnv(map[string]string{
			"OMNIROUTE_API_KEY": "real-secret",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep2.APIKey != "real-secret" || ep2.MissingAPIKey {
			t.Fatalf("unexpected omniroute explicit key endpoint: %+v", ep2)
		}
	})

	t.Run("deepseek requires key", func(t *testing.T) {
		ep, err := ResolveProviderEndpoint(defaults.Providers["deepseek"], mapEnv(nil))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ep.MissingAPIKey || ep.IsLoopback || ep.Compatibility != CompatibilityDeepSeekThinking {
			t.Fatalf("unexpected deepseek endpoint: %+v", ep)
		}
	})

	t.Run("empty or invalid URL returns error", func(t *testing.T) {
		if _, err := ResolveProviderEndpoint(ProviderConfig{BaseURL: ""}, mapEnv(nil)); err == nil {
			t.Fatalf("expected error for empty BaseURL")
		}
		if _, err := ResolveProviderEndpoint(ProviderConfig{BaseURL: "not-a-url"}, mapEnv(nil)); err == nil {
			t.Fatalf("expected error for invalid BaseURL")
		}
	})
}
