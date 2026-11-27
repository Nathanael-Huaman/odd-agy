package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	RuntimeAgyNative   = "agy-native"
	RuntimeExternalMCP = "external-mcp"

	CompatibilityStandard         = "standard"
	CompatibilityDeepSeekThinking = "deepseek-thinking"
)

// AllRoles enumerates every subagent/review role supported by odd-agy profiles.
var AllRoles = []string{
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

// ProviderConfig defines an OpenAI-compatible or DeepSeek-thinking provider template.
type ProviderConfig struct {
	BaseURL                  string            `json:"baseUrl"`
	BaseURLEnv               string            `json:"baseUrlEnv,omitempty"`
	APIKeyEnv                string            `json:"apiKeyEnv,omitempty"`
	Compatibility            string            `json:"compatibility"`
	AllowLoopbackWithoutKey  bool              `json:"allowLoopbackWithoutKey,omitempty"`
	PlaceholderKeyOnLoopback string            `json:"placeholderKeyOnLoopback,omitempty"`
	DefaultModel             string            `json:"defaultModel"`
	FastModel                string            `json:"fastModel,omitempty"`
	Headers                  map[string]string `json:"headers,omitempty"`
}

// RoleTarget describes a concrete runtime/provider/model destination.
type RoleTarget struct {
	Runtime  string `json:"runtime"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
}

// RoleRoute describes the primary route and ordered fallback chain for a role.
type RoleRoute struct {
	Runtime  string       `json:"runtime"`
	Provider string       `json:"provider,omitempty"`
	Model    string       `json:"model"`
	Fallback []RoleTarget `json:"fallback,omitempty"`
}

// ProfileConfig maps each role to its routing policy.
type ProfileConfig struct {
	Description string               `json:"description,omitempty"`
	Roles       map[string]RoleRoute `json:"roles"`
}

// ProfilesConfig is stored at .agents/odd/profiles.json.
type ProfilesConfig struct {
	SchemaVersion int                       `json:"schemaVersion"`
	ActiveProfile string                    `json:"activeProfile"`
	Providers     map[string]ProviderConfig `json:"providers"`
	Profiles      map[string]ProfileConfig  `json:"profiles"`
}

// ResolvedEndpoint contains the runtime-resolved endpoint URL, API key, and loopback status.
type ResolvedEndpoint struct {
	BaseURL       string
	APIKey        string
	MissingAPIKey bool
	IsLoopback    bool
	KeySource     string
	Compatibility string
	Headers       map[string]string
}

// ProfilesPath returns the path to .agents/odd/profiles.json under cwd.
func ProfilesPath(cwd string) string {
	return filepath.Join(cwd, ".agents", "odd", "profiles.json")
}

// DefaultProfilesConfig returns the built-in provider templates and routing profiles.
func DefaultProfilesConfig() ProfilesConfig {
	providers := map[string]ProviderConfig{
		"deepseek": {
			BaseURL:       "https://api.deepseek.com/v1",
			BaseURLEnv:    "DEEPSEEK_BASE_URL",
			APIKeyEnv:     "DEEPSEEK_API_KEY",
			Compatibility: CompatibilityDeepSeekThinking,
			DefaultModel:  "deepseek-v4-pro",
			FastModel:     "deepseek-flash",
		},
		"9router": {
			BaseURL:                 "http://localhost:20128/v1",
			BaseURLEnv:              "NINEROUTER_BASE_URL",
			APIKeyEnv:               "NINEROUTER_API_KEY",
			Compatibility:           CompatibilityStandard,
			AllowLoopbackWithoutKey: true,
			DefaultModel:            "kr/claude-sonnet-4.5",
		},
		"omniroute": {
			BaseURL:                  "http://localhost:20128/v1",
			BaseURLEnv:               "OMNIROUTE_BASE_URL",
			APIKeyEnv:                "OMNIROUTE_API_KEY",
			Compatibility:            CompatibilityStandard,
			AllowLoopbackWithoutKey:  true,
			PlaceholderKeyOnLoopback: "omniroute-local",
			DefaultModel:             "cc/claude-opus-4-6",
		},
		"openai-compatible": {
			BaseURL:       "https://api.openai.com/v1",
			BaseURLEnv:    "OPENAI_BASE_URL",
			APIKeyEnv:     "OPENAI_API_KEY",
			Compatibility: CompatibilityStandard,
			DefaultModel:  "gpt-4.1",
		},
	}

	profiles := map[string]ProfileConfig{
		"agy-oauth":        buildNativeProfile(),
		"deepseek-hybrid":  buildHybridProfile("DeepSeek for verify, second judge, and 4R/refuter reviews; native Antigravity OAuth for explore/worker.", "deepseek", "deepseek-v4-pro"),
		"9router-hybrid":   buildHybridProfile("9Router local proxy for verify, second judge, and 4R/refuter reviews; native Antigravity OAuth for explore/worker.", "9router", "kr/claude-sonnet-4.5"),
		"omniroute-hybrid": buildHybridProfile("OmniRoute local proxy for verify, second judge, and 4R/refuter reviews; native Antigravity OAuth for explore/worker.", "omniroute", "cc/claude-opus-4-6"),
	}

	return ProfilesConfig{
		SchemaVersion: SchemaVersion,
		ActiveProfile: "agy-oauth",
		Providers:     providers,
		Profiles:      profiles,
	}
}

func buildNativeProfile() ProfileConfig {
	roles := make(map[string]RoleRoute, len(AllRoles))
	for _, role := range AllRoles {
		model := "pro"
		if role == "explore" {
			model = "flash"
		}
		roles[role] = RoleRoute{
			Runtime: RuntimeAgyNative,
			Model:   model,
		}
	}
	return ProfileConfig{
		Description: "All roles run on native Antigravity OAuth models (zero external subprocesses).",
		Roles:       roles,
	}
}

func buildHybridProfile(description, provider, model string) ProfileConfig {
	ext := RoleRoute{
		Runtime:  RuntimeExternalMCP,
		Provider: provider,
		Model:    model,
		Fallback: []RoleTarget{{
			Runtime: RuntimeAgyNative,
			Model:   "pro",
		}},
	}
	return ProfileConfig{
		Description: description,
		Roles: map[string]RoleRoute{
			"explore":            {Runtime: RuntimeAgyNative, Model: "flash"},
			"worker":             {Runtime: RuntimeAgyNative, Model: "pro"},
			"verify":             cloneRoute(ext),
			"jd-judge-a":         {Runtime: RuntimeAgyNative, Model: "pro"},
			"jd-judge-b":         cloneRoute(ext),
			"jd-fix-agent":       {Runtime: RuntimeAgyNative, Model: "pro"},
			"review-risk":        cloneRoute(ext),
			"review-resilience":  cloneRoute(ext),
			"review-readability": cloneRoute(ext),
			"review-reliability": cloneRoute(ext),
			"review-refuter":     cloneRoute(ext),
		},
	}
}

func cloneRoute(r RoleRoute) RoleRoute {
	out := r
	if len(r.Fallback) > 0 {
		out.Fallback = append([]RoleTarget(nil), r.Fallback...)
	}
	return out
}

// ValidateProfiles checks providers, profiles, activeProfile, and all role routes.
func ValidateProfiles(cfg ProfilesConfig) error {
	if cfg.SchemaVersion != SchemaVersion {
		return fmt.Errorf("invalid schemaVersion %d: must be %d", cfg.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(cfg.ActiveProfile) == "" {
		return errors.New("invalid activeProfile: must not be empty")
	}
	if len(cfg.Profiles) == 0 {
		return errors.New("invalid profiles: must define at least one profile")
	}
	if _, ok := cfg.Profiles[cfg.ActiveProfile]; !ok {
		return fmt.Errorf("unknown activeProfile %q", cfg.ActiveProfile)
	}

	for name, p := range cfg.Providers {
		if strings.TrimSpace(name) == "" {
			return errors.New("provider name must not be empty")
		}
		if _, err := parseHTTPURL(p.BaseURL); err != nil {
			return fmt.Errorf("provider %q invalid baseUrl: %w", name, err)
		}
		switch p.Compatibility {
		case CompatibilityStandard, CompatibilityDeepSeekThinking:
		default:
			return fmt.Errorf("provider %q invalid compatibility %q: must be %q or %q", name, p.Compatibility, CompatibilityStandard, CompatibilityDeepSeekThinking)
		}
		if strings.TrimSpace(p.DefaultModel) == "" {
			return fmt.Errorf("provider %q defaultModel must not be empty", name)
		}
	}

	for profName, prof := range cfg.Profiles {
		if strings.TrimSpace(profName) == "" {
			return errors.New("profile name must not be empty")
		}
		for _, reqRole := range AllRoles {
			if _, ok := prof.Roles[reqRole]; !ok {
				return fmt.Errorf("profile %q missing required role %q", profName, reqRole)
			}
		}
		for roleName, route := range prof.Roles {
			if err := validateTarget(cfg.Providers, RoleTarget{
				Runtime:  route.Runtime,
				Provider: route.Provider,
				Model:    route.Model,
			}); err != nil {
				return fmt.Errorf("profile %q role %q: %w", profName, roleName, err)
			}
			for i, fb := range route.Fallback {
				if err := validateTarget(cfg.Providers, fb); err != nil {
					return fmt.Errorf("profile %q role %q fallback[%d]: %w", profName, roleName, i, err)
				}
			}
		}
	}

	return nil
}

func validateTarget(providers map[string]ProviderConfig, t RoleTarget) error {
	if strings.TrimSpace(t.Model) == "" {
		return errors.New("model must not be empty")
	}
	switch t.Runtime {
	case RuntimeAgyNative:
		if strings.TrimSpace(t.Provider) != "" {
			return fmt.Errorf("provider %q must be empty for runtime %q", t.Provider, RuntimeAgyNative)
		}
	case RuntimeExternalMCP:
		if strings.TrimSpace(t.Provider) == "" {
			return fmt.Errorf("provider is required for runtime %q", RuntimeExternalMCP)
		}
		if _, ok := providers[t.Provider]; !ok {
			return fmt.Errorf("unknown provider %q", t.Provider)
		}
	default:
		return fmt.Errorf("invalid runtime %q: must be %q or %q", t.Runtime, RuntimeAgyNative, RuntimeExternalMCP)
	}
	return nil
}

// LoadProfiles reads .agents/odd/profiles.json under cwd, returning DefaultProfilesConfig if missing.
func LoadProfiles(cwd string) (ProfilesConfig, error) {
	path := ProfilesPath(cwd)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultProfilesConfig(), nil
		}
		return ProfilesConfig{}, fmt.Errorf("read profiles %s: %w", path, err)
	}

	var cfg ProfilesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ProfilesConfig{}, fmt.Errorf("parse profiles %s: %w", path, err)
	}
	if err := ValidateProfiles(cfg); err != nil {
		return ProfilesConfig{}, fmt.Errorf("validate profiles %s: %w", path, err)
	}
	return cfg, nil
}

// SaveProfiles validates cfg and atomically writes .agents/odd/profiles.json under cwd.
func SaveProfiles(cwd string, cfg ProfilesConfig) error {
	if err := ValidateProfiles(cfg); err != nil {
		return err
	}
	return writeJSONAtomic(ProfilesPath(cwd), cfg)
}

// ResolveProviderEndpoint applies baseUrlEnv overrides and loopback API-key rules.
func ResolveProviderEndpoint(provider ProviderConfig, lookupEnv func(string) (string, bool)) (ResolvedEndpoint, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	rawURL := strings.TrimSpace(provider.BaseURL)
	if envName := strings.TrimSpace(provider.BaseURLEnv); envName != "" {
		if val, ok := lookupEnv(envName); ok && strings.TrimSpace(val) != "" {
			rawURL = strings.TrimSpace(val)
		}
	}

	u, err := parseHTTPURL(rawURL)
	if err != nil {
		return ResolvedEndpoint{}, err
	}
	normalizedURL := strings.TrimRight(u.String(), "/")
	loopback := isLoopbackHost(u.Hostname())

	var apiKey string
	var keySource string
	var missing bool

	if keyEnv := strings.TrimSpace(provider.APIKeyEnv); keyEnv != "" {
		if val, ok := lookupEnv(keyEnv); ok && strings.TrimSpace(val) != "" {
			apiKey = strings.TrimSpace(val)
			keySource = "env"
		}
	}

	if keySource == "" {
		if loopback && provider.AllowLoopbackWithoutKey {
			if provider.PlaceholderKeyOnLoopback != "" {
				apiKey = provider.PlaceholderKeyOnLoopback
				keySource = "loopback-placeholder"
			} else {
				apiKey = ""
				keySource = "loopback-optional"
			}
		} else {
			keySource = "missing"
			missing = true
		}
	}

	var headers map[string]string
	if len(provider.Headers) > 0 {
		headers = make(map[string]string, len(provider.Headers))
		for k, v := range provider.Headers {
			headers[k] = v
		}
	}

	return ResolvedEndpoint{
		BaseURL:       normalizedURL,
		APIKey:        apiKey,
		MissingAPIKey: missing,
		IsLoopback:    loopback,
		KeySource:     keySource,
		Compatibility: provider.Compatibility,
		Headers:       headers,
	}, nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("URL must not be empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse URL %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("URL %q must be a valid http or https URL with host", raw)
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
