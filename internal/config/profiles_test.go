package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	wantRoles := []string{
		RoleOrchestrator,
		"explore",
		"worker",
		"verify",
		"jd-judge-a",
		"jd-judge-b",
		"jd-fix-agent",
		"review-risk",
		"review-resilience",
		"review-readability",
		"review-reliability",
		"review-refuter",
	}
	if !slices.Equal(AllRoles, wantRoles) {
		t.Fatalf("AllRoles = %v, want %v", AllRoles, wantRoles)
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
		orch := prof.Roles[RoleOrchestrator]
		if orch.Runtime != RuntimeAgyNative || orch.Model != "inherit" {
			t.Fatalf("profile %q orchestrator = %+v, want agy-native/inherit", profName, orch)
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
		{"missing orchestrator role", func(c *ProfilesConfig) {
			prof := c.Profiles["agy-oauth"]
			delete(prof.Roles, RoleOrchestrator)
			c.Profiles["agy-oauth"] = prof
		}},
		{"orchestrator external-mcp rejected", func(c *ProfilesConfig) {
			prof := c.Profiles["deepseek-hybrid"]
			prof.Roles[RoleOrchestrator] = RoleRoute{
				Runtime:  RuntimeExternalMCP,
				Provider: "deepseek",
				Model:    "deepseek-v4-pro",
			}
			c.Profiles["deepseek-hybrid"] = prof
		}},
		{"orchestrator fallback external-mcp rejected", func(c *ProfilesConfig) {
			prof := c.Profiles["deepseek-hybrid"]
			prof.Roles[RoleOrchestrator] = RoleRoute{
				Runtime: RuntimeAgyNative,
				Model:   "inherit",
				Fallback: []RoleTarget{{
					Runtime:  RuntimeExternalMCP,
					Provider: "deepseek",
					Model:    "deepseek-v4-pro",
				}},
			}
			c.Profiles["deepseek-hybrid"] = prof
		}},
		{"model newline rejected", func(c *ProfilesConfig) {
			prof := c.Profiles["agy-oauth"]
			r := prof.Roles["worker"]
			r.Model = "pro\ntools: [run_command]"
			prof.Roles["worker"] = r
			c.Profiles["agy-oauth"] = prof
		}},
		{"model carriage return rejected", func(c *ProfilesConfig) {
			prof := c.Profiles["agy-oauth"]
			r := prof.Roles["worker"]
			r.Model = "pro\rinjected"
			prof.Roles["worker"] = r
			c.Profiles["agy-oauth"] = prof
		}},
		{"model null byte rejected", func(c *ProfilesConfig) {
			prof := c.Profiles["agy-oauth"]
			r := prof.Roles["worker"]
			r.Model = "pro\x00"
			prof.Roles["worker"] = r
			c.Profiles["agy-oauth"] = prof
		}},
		{"fallback model control char rejected", func(c *ProfilesConfig) {
			prof := c.Profiles["deepseek-hybrid"]
			r := prof.Roles["verify"]
			r.Fallback = []RoleTarget{{Runtime: RuntimeAgyNative, Model: "pro\nfoo"}}
			prof.Roles["verify"] = r
			c.Profiles["deepseek-hybrid"] = prof
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

func TestLoadProfilesBackfillsMissingOrchestrator(t *testing.T) {
	dir := t.TempDir()
	// Simulate a v0.1.0 profiles.json omitting "orchestrator" in every profile.
	v010JSON := `{
  "schemaVersion": 1,
  "activeProfile": "deepseek-hybrid",
  "providers": {
    "deepseek": {
      "baseUrl": "https://api.deepseek.com/v1",
      "apiKeyEnv": "DEEPSEEK_API_KEY",
      "compatibility": "deepseek-thinking",
      "defaultModel": "deepseek-v4-pro"
    }
  },
  "profiles": {
    "agy-oauth": {
      "roles": {
        "explore": {"runtime": "agy-native", "model": "flash"},
        "worker": {"runtime": "agy-native", "model": "pro"},
        "verify": {"runtime": "agy-native", "model": "pro"},
        "jd-judge-a": {"runtime": "agy-native", "model": "pro"},
        "jd-judge-b": {"runtime": "agy-native", "model": "pro"},
        "jd-fix-agent": {"runtime": "agy-native", "model": "pro"},
        "review-risk": {"runtime": "agy-native", "model": "pro"},
        "review-resilience": {"runtime": "agy-native", "model": "pro"},
        "review-readability": {"runtime": "agy-native", "model": "pro"},
        "review-reliability": {"runtime": "agy-native", "model": "pro"},
        "review-refuter": {"runtime": "agy-native", "model": "pro"}
      }
    },
    "deepseek-hybrid": {
      "roles": {
        "explore": {"runtime": "agy-native", "model": "flash"},
        "worker": {"runtime": "agy-native", "model": "pro"},
        "verify": {"runtime": "external-mcp", "provider": "deepseek", "model": "deepseek-v4-pro"},
        "jd-judge-a": {"runtime": "agy-native", "model": "pro"},
        "jd-judge-b": {"runtime": "external-mcp", "provider": "deepseek", "model": "deepseek-v4-pro"},
        "jd-fix-agent": {"runtime": "agy-native", "model": "pro"},
        "review-risk": {"runtime": "external-mcp", "provider": "deepseek", "model": "deepseek-v4-pro"},
        "review-resilience": {"runtime": "external-mcp", "provider": "deepseek", "model": "deepseek-v4-pro"},
        "review-readability": {"runtime": "external-mcp", "provider": "deepseek", "model": "deepseek-v4-pro"},
        "review-reliability": {"runtime": "external-mcp", "provider": "deepseek", "model": "deepseek-v4-pro"},
        "review-refuter": {"runtime": "external-mcp", "provider": "deepseek", "model": "deepseek-v4-pro"}
      }
    }
  }
}`
	p := ProfilesPath(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(v010JSON), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles v0.1.0 failed: %v", err)
	}
	for _, pname := range []string{"agy-oauth", "deepseek-hybrid"} {
		orch, ok := loaded.Profiles[pname].Roles[RoleOrchestrator]
		if !ok {
			t.Fatalf("profile %q missing backfilled orchestrator", pname)
		}
		if orch.Runtime != RuntimeAgyNative || orch.Model != "inherit" {
			t.Fatalf("profile %q backfilled orchestrator = %+v, want agy-native/inherit", pname, orch)
		}
	}

	// Preserve: explicit custom orchestrator model is NOT overwritten by backfill, and incomplete v0.1.0 profiles missing other roles still fail.
	customDir := t.TempDir()
	customP := ProfilesPath(customDir)
	_ = os.MkdirAll(filepath.Dir(customP), 0o755)
	customJSON := strings.Replace(v010JSON, `"explore": {"runtime": "agy-native", "model": "flash"},`, `"orchestrator": {"runtime": "agy-native", "model": "gemini-2.5-pro"}, "explore": {"runtime": "agy-native", "model": "flash"},`, 1)
	if err := os.WriteFile(customP, []byte(customJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	loadedCustom, err := LoadProfiles(customDir)
	if err != nil {
		t.Fatalf("LoadProfiles custom orchestrator failed: %v", err)
	}
	if got := loadedCustom.Profiles["agy-oauth"].Roles[RoleOrchestrator].Model; got != "gemini-2.5-pro" {
		t.Fatalf("explicit orchestrator model overwritten: got %q, want gemini-2.5-pro", got)
	}

	incompleteDir := t.TempDir()
	incompleteP := ProfilesPath(incompleteDir)
	_ = os.MkdirAll(filepath.Dir(incompleteP), 0o755)
	incompleteJSON := strings.Replace(v010JSON, `"worker": {"runtime": "agy-native", "model": "pro"},`, ``, 1)
	if err := os.WriteFile(incompleteP, []byte(incompleteJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfiles(incompleteDir); err == nil {
		t.Fatalf("expected LoadProfiles to reject incomplete profile missing worker")
	}
}

func TestRoleAgentRelPath(t *testing.T) {
	want := map[string]string{
		RoleOrchestrator:     ".agents/agents/odd-orchestrator.md",
		"explore":            ".agents/agents/gentle-ai-explore.md",
		"worker":             ".agents/agents/gentle-ai-worker.md",
		"verify":             ".agents/agents/gentle-ai-verify.md",
		"jd-judge-a":         ".agents/agents/jd-judge-a.md",
		"jd-judge-b":         ".agents/agents/jd-judge-b.md",
		"jd-fix-agent":       ".agents/agents/jd-fix-agent.md",
		"review-risk":        ".agents/agents/review-risk.md",
		"review-resilience":  ".agents/agents/review-resilience.md",
		"review-readability": ".agents/agents/review-readability.md",
		"review-reliability": ".agents/agents/review-reliability.md",
		"review-refuter":     ".agents/agents/review-refuter.md",
	}
	for _, role := range AllRoles {
		got, ok := RoleAgentRelPath(role)
		if !ok || got != want[role] {
			t.Errorf("RoleAgentRelPath(%q) = (%q, %t), want (%q, true)", role, got, ok, want[role])
		}
		if !strings.HasPrefix(got, ".agents/agents/") || !strings.HasSuffix(got, ".md") {
			t.Errorf("unexpected agent path %q", got)
		}
	}
	if got, ok := RoleAgentRelPath("unknown-role"); ok || got != "" {
		t.Errorf("RoleAgentRelPath(unknown) = (%q, %t), want (\"\", false)", got, ok)
	}
}
