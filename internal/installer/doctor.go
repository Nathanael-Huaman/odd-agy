package installer

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Nathanael-Huaman/odd-agy/internal/assets"
	"github.com/Nathanael-Huaman/odd-agy/internal/config"
)

// Doctor runs read-only diagnostics on <workspaceRoot>/.agents/, PATH prerequisites,
// active profile provider readiness, and global ~/.gemini/ gentle-ai collisions.
func Doctor(workspaceRoot string, opts DoctorOptions) (DoctorReport, error) {
	ws, err := validateWorkspaceRoot(workspaceRoot)
	if err != nil {
		return DoctorReport{}, err
	}

	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	homeDir := strings.TrimSpace(opts.HomeDir)
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	lookupEnv := opts.LookupEnv
	if lookupEnv == nil {
		lookupEnv = config.NewSecretsLookupEnv(homeDir, os.LookupEnv)
	} else if strings.TrimSpace(opts.HomeDir) != "" {
		lookupEnv = config.NewSecretsLookupEnv(opts.HomeDir, lookupEnv)
	}

	var checks []Check

	// 1. agy CLI on PATH
	if agyPath, err := lookPath("agy"); err == nil {
		checks = append(checks, Check{
			Name:   "agy-cli",
			Status: CheckOK,
			Detail: fmt.Sprintf("found at %s", agyPath),
		})
	} else {
		checks = append(checks, Check{
			Name:   "agy-cli",
			Status: CheckFail,
			Detail: "agy binary not found on PATH",
		})
	}

	// 2. global engram binary on PATH
	if engramPath, err := lookPath("engram"); err == nil {
		checks = append(checks, Check{
			Name:   "engram-cli",
			Status: CheckOK,
			Detail: fmt.Sprintf("found at %s", engramPath),
		})
	} else {
		checks = append(checks, Check{
			Name:   "engram-cli",
			Status: CheckWarn,
			Detail: "engram binary not found on PATH (global memory MCP disabled)",
		})
	}

	// 3. .agents/odd/config.json validity
	checks = append(checks, checkConfig(ws))

	// 4. .agents/odd/profiles.json validity & load for provider check
	profCheck, profCfg, profOK := checkProfiles(ws)
	checks = append(checks, profCheck)

	// 5. Managed files integrity vs .agents/odd/.managed-manifest.json
	checks = append(checks, checkManagedFiles(ws))

	// 6. .agents/hooks.json validity & odd-agy entry
	checks = append(checks, checkHooksConfig(ws))

	// 7. .agents/mcp_config.json validity & odd-agy entry
	checks = append(checks, checkMCPConfig(ws))

	// 8. Active profile provider readiness (never leaks API key values)
	checks = append(checks, checkProviderReadiness(profCfg, profOK, lookupEnv))

	// 9. Global ~/.gemini/ gentle-ai collision check
	checks = append(checks, checkGlobalCollision(homeDir))

	return DoctorReport{Checks: checks}, nil
}

func checkConfig(ws string) Check {
	p := config.ConfigPath(ws)
	if _, err := os.Stat(p); err != nil {
		return Check{
			Name:   "config",
			Status: CheckFail,
			Detail: "missing .agents/odd/config.json",
		}
	}
	cfg, err := config.LoadConfig(ws)
	if err != nil {
		return Check{
			Name:   "config",
			Status: CheckFail,
			Detail: err.Error(),
		}
	}
	return Check{
		Name:   "config",
		Status: CheckOK,
		Detail: fmt.Sprintf("mode=%s rdd=%t install=%s", cfg.DelegationMode, cfg.RDDEnabled, cfg.InstallMode),
	}
}

func checkProfiles(ws string) (Check, config.ProfilesConfig, bool) {
	p := config.ProfilesPath(ws)
	if _, err := os.Stat(p); err != nil {
		return Check{
			Name:   "profiles",
			Status: CheckFail,
			Detail: "missing .agents/odd/profiles.json",
		}, config.ProfilesConfig{}, false
	}
	cfg, err := config.LoadProfiles(ws)
	if err != nil {
		return Check{
			Name:   "profiles",
			Status: CheckFail,
			Detail: err.Error(),
		}, config.ProfilesConfig{}, false
	}
	return Check{
		Name:   "profiles",
		Status: CheckOK,
		Detail: fmt.Sprintf("activeProfile=%s (%d profiles)", cfg.ActiveProfile, len(cfg.Profiles)),
	}, cfg, true
}

func checkManagedFiles(ws string) Check {
	if err := assets.ValidateAll(); err != nil {
		return Check{
			Name:   "managed-files",
			Status: CheckFail,
			Detail: fmt.Sprintf("embedded asset contract error: %v", err),
		}
	}

	manifest, err := LoadManifest(ws)
	if err != nil {
		return Check{
			Name:   "managed-files",
			Status: CheckFail,
			Detail: fmt.Sprintf("missing or invalid .agents/odd/.managed-manifest.json: %v", err),
		}
	}

	var missing []string
	var drifted []string

	keys := make([]string, 0, len(manifest.Files))
	for k := range manifest.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, rel := range keys {
		expectedHash := manifest.Files[rel]
		abs, err := resolveContainedPath(ws, rel)
		if err != nil {
			missing = append(missing, rel)
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			missing = append(missing, rel)
			continue
		}
		if sha256PrefixedHex(data) != expectedHash {
			drifted = append(drifted, rel)
		}
	}

	if allAssets, err := assets.All(); err == nil {
		for _, af := range allAssets {
			rel := ".agents/" + af.RelPath
			if _, ok := manifest.Files[rel]; !ok {
				missing = append(missing, rel)
			}
		}
	}
	if _, ok := manifest.Files[SessionSettingsRelPath]; !ok {
		missing = append(missing, SessionSettingsRelPath)
	}

	if len(missing) > 0 {
		return Check{
			Name:   "managed-files",
			Status: CheckFail,
			Detail: fmt.Sprintf("%d managed file(s) missing: %s", len(missing), strings.Join(missing, ", ")),
		}
	}
	if len(drifted) > 0 {
		return Check{
			Name:   "managed-files",
			Status: CheckWarn,
			Detail: fmt.Sprintf("%d managed file(s) modified locally: %s", len(drifted), strings.Join(drifted, ", ")),
		}
	}
	return Check{
		Name:   "managed-files",
		Status: CheckOK,
		Detail: fmt.Sprintf("%d managed file(s) verified", len(manifest.Files)),
	}
}

func checkHooksConfig(ws string) Check {
	p := HooksPath(ws)
	doc, exists, err := readJSONObject(p)
	if err != nil {
		return Check{Name: "hooks-config", Status: CheckFail, Detail: err.Error()}
	}
	if !exists {
		return Check{Name: "hooks-config", Status: CheckFail, Detail: "missing .agents/hooks.json"}
	}
	if _, ok := doc[ManagedHookKey]; !ok {
		return Check{Name: "hooks-config", Status: CheckFail, Detail: ".agents/hooks.json missing \"odd-agy\" entry"}
	}
	return Check{Name: "hooks-config", Status: CheckOK, Detail: ".agents/hooks.json valid with odd-agy hooks"}
}

func checkMCPConfig(ws string) Check {
	p := MCPConfigPath(ws)
	doc, exists, err := readJSONObject(p)
	if err != nil {
		return Check{Name: "mcp-config", Status: CheckFail, Detail: err.Error()}
	}
	if !exists {
		return Check{Name: "mcp-config", Status: CheckFail, Detail: "missing .agents/mcp_config.json"}
	}
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		return Check{Name: "mcp-config", Status: CheckFail, Detail: ".agents/mcp_config.json missing mcpServers object"}
	}
	if _, ok := servers[ManagedMCPKey]; !ok {
		return Check{Name: "mcp-config", Status: CheckFail, Detail: ".agents/mcp_config.json missing \"odd-agy\" server"}
	}
	return Check{Name: "mcp-config", Status: CheckOK, Detail: ".agents/mcp_config.json valid with odd-agy server"}
}

func checkProviderReadiness(cfg config.ProfilesConfig, profOK bool, lookupEnv func(string) (string, bool)) Check {
	if !profOK {
		return Check{
			Name:   "provider-readiness",
			Status: CheckFail,
			Detail: "skipped because profiles.json is invalid or missing",
		}
	}

	active, ok := cfg.Profiles[cfg.ActiveProfile]
	if !ok {
		return Check{
			Name:   "provider-readiness",
			Status: CheckFail,
			Detail: fmt.Sprintf("active profile %q not found", cfg.ActiveProfile),
		}
	}

	usedProviders := map[string]bool{}
	for _, route := range active.Roles {
		if route.Runtime == config.RuntimeExternalMCP && route.Provider != "" {
			usedProviders[route.Provider] = true
		}
	}

	if len(usedProviders) == 0 {
		return Check{
			Name:   "provider-readiness",
			Status: CheckOK,
			Detail: fmt.Sprintf("profile %q uses native Antigravity OAuth for all roles", cfg.ActiveProfile),
		}
	}

	names := make([]string, 0, len(usedProviders))
	for name := range usedProviders {
		names = append(names, name)
	}
	sort.Strings(names)

	var missing []string
	var summaries []string
	for _, name := range names {
		prov, exists := cfg.Providers[name]
		if !exists {
			missing = append(missing, name+"(unknown)")
			continue
		}
		ep, err := config.ResolveProviderEndpoint(prov, lookupEnv)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s(invalid url)", name))
			continue
		}
		if ep.MissingAPIKey {
			envName := prov.APIKeyEnv
			if envName == "" {
				envName = "API_KEY"
			}
			missing = append(missing, fmt.Sprintf("%s(%s unset)", name, envName))
			continue
		}
		if ep.IsLoopback && ep.KeySource != "env" {
			summaries = append(summaries, fmt.Sprintf("%s=loopback(%s)", name, ep.KeySource))
		} else {
			summaries = append(summaries, fmt.Sprintf("%s=ready(%s set, redacted)", name, prov.APIKeyEnv))
		}
	}

	if len(missing) > 0 {
		return Check{
			Name:   "provider-readiness",
			Status: CheckWarn,
			Detail: fmt.Sprintf("profile %q external provider(s) not ready: %s", cfg.ActiveProfile, strings.Join(missing, ", ")),
		}
	}

	return Check{
		Name:   "provider-readiness",
		Status: CheckOK,
		Detail: fmt.Sprintf("profile %q ready: %s", cfg.ActiveProfile, strings.Join(summaries, ", ")),
	}
}

func checkGlobalCollision(homeDir string) Check {
	if homeDir == "" {
		return Check{
			Name:   "global-collision",
			Status: CheckOK,
			Detail: "home directory unavailable; skipped global ~/.gemini check",
		}
	}

	var collisions []string
	pattern := filepath.Join(homeDir, ".gemini", "config", "rules", "gentle-ai-*.md")
	matches, _ := filepath.Glob(pattern)
	sort.Strings(matches)
	collisions = append(collisions, matches...)

	geminiMD := filepath.Join(homeDir, ".gemini", "GEMINI.md")
	if data, err := os.ReadFile(geminiMD); err == nil {
		if strings.Contains(strings.ToLower(string(data)), "gentle-ai") {
			collisions = append(collisions, geminiMD)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Check{
			Name:   "global-collision",
			Status: CheckWarn,
			Detail: fmt.Sprintf("could not inspect %s: %v", geminiMD, err),
		}
	}

	if len(collisions) > 0 {
		return Check{
			Name:   "global-collision",
			Status: CheckWarn,
			Detail: fmt.Sprintf("global gentle-ai files may collide with project .agents/rules/: %s", strings.Join(collisions, ", ")),
		}
	}
	return Check{
		Name:   "global-collision",
		Status: CheckOK,
		Detail: "no global ~/.gemini gentle-ai collisions detected",
	}
}
