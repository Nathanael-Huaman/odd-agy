package installer

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/Nathanael-Huaman/odd-agy/internal/assets"
	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

// Init installs odd-agy assets, configuration, hooks, MCP servers, and manifest into <workspaceRoot>/.agents/.
func Init(workspaceRoot string, opts InitOptions) (InitResult, error) {
	ws, err := validateWorkspaceRoot(workspaceRoot)
	if err != nil {
		return InitResult{}, err
	}

	if err := assets.ValidateAll(); err != nil {
		return InitResult{}, fmt.Errorf("validate embedded assets: %w", err)
	}
	allAssets, err := assets.All()
	if err != nil {
		return InitResult{}, fmt.Errorf("load embedded assets: %w", err)
	}

	installMode := strings.TrimSpace(opts.InstallMode)
	if installMode == "" {
		installMode = config.InstallModeLocal
	}
	delegationMode := strings.TrimSpace(opts.DelegationMode)
	if delegationMode == "" {
		delegationMode = config.DelegationModeFull
	}
	profileName := strings.TrimSpace(opts.Profile)
	if profileName == "" {
		profileName = "agy-oauth"
	}

	binCmd := strings.TrimSpace(opts.BinaryCommand)
	if binCmd == "" {
		if installMode == config.InstallModeLocal {
			binCmd = "./node_modules/.bin/odd-agy"
		} else {
			binCmd = config.DefaultBinaryCommand
		}
	}

	// Validate config and profile selections before writing anything.
	desiredCfg := config.DefaultConfig()
	desiredCfg.InstallMode = installMode
	desiredCfg.DelegationMode = delegationMode
	desiredCfg.BinaryCommand = binCmd
	if err := config.ValidateConfig(desiredCfg); err != nil {
		return InitResult{}, err
	}

	desiredProfiles := config.DefaultProfilesConfig()
	if _, ok := desiredProfiles.Profiles[profileName]; !ok {
		return InitResult{}, fmt.Errorf("unknown profile %q", profileName)
	}
	desiredProfiles.ActiveProfile = profileName
	if err := config.ValidateProfiles(desiredProfiles); err != nil {
		return InitResult{}, err
	}

	// Preflight existing config, profiles, hooks.json, and mcp_config.json so errors abort before mutating disk.
	var configExists bool
	if _, err := os.Stat(config.ConfigPath(ws)); err == nil {
		configExists = true
		if !opts.Force {
			existingCfg, err := config.LoadConfig(ws)
			if err != nil {
				return InitResult{}, fmt.Errorf("existing config invalid (use Force to overwrite): %w", err)
			}
			if strings.TrimSpace(opts.BinaryCommand) == "" && strings.TrimSpace(existingCfg.BinaryCommand) != "" {
				binCmd = existingCfg.BinaryCommand
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return InitResult{}, fmt.Errorf("stat config: %w", err)
	}

	var profilesExists bool
	activeProfilesCfg := desiredProfiles
	if _, err := os.Stat(config.ProfilesPath(ws)); err == nil {
		profilesExists = true
		if !opts.Force {
			existingProfiles, err := config.LoadProfiles(ws)
			if err != nil {
				return InitResult{}, fmt.Errorf("existing profiles invalid (use Force to overwrite): %w", err)
			}
			activeProfilesCfg = existingProfiles
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return InitResult{}, fmt.Errorf("stat profiles: %w", err)
	}

	var activeProfilePtr *config.ProfileConfig
	if prof, ok := activeProfilesCfg.Profiles[activeProfilesCfg.ActiveProfile]; ok {
		activeProfilePtr = &prof
	}

	if _, _, err := readJSONObject(HooksPath(ws)); err != nil {
		return InitResult{}, err
	}
	mcpDoc, _, err := readJSONObject(MCPConfigPath(ws))
	if err != nil {
		return InitResult{}, err
	}
	if rawServers, ok := mcpDoc["mcpServers"]; ok && rawServers != nil {
		if _, ok := rawServers.(map[string]any); !ok {
			return InitResult{}, fmt.Errorf("%s: mcpServers must be a JSON object", MCPConfigPath(ws))
		}
	}

	if opts.AllowGlobalMCP {
		if err := PreflightGlobalMCP(opts.HomeDir); err != nil {
			return InitResult{}, err
		}
	}

	prevEngramManaged := false
	if prevManifest, err := LoadManifest(ws); err == nil {
		prevEngramManaged = slices.Contains(prevManifest.ManagedMCPKeys, EngramMCPKey)
	}

	// Write embedded rules, agents, and skills under .agents/, plus session settings.
	sessBytes, err := sessionSettingsBytes()
	if err != nil {
		return InitResult{}, err
	}

	manifestFiles := make(map[string]string, len(allAssets)+1)
	installed := make([]string, 0, len(allAssets)+1)
	for _, af := range allAssets {
		relSlash := ".agents/" + af.RelPath
		target, err := resolveContainedPath(ws, relSlash)
		if err != nil {
			return InitResult{}, err
		}
		rendered := renderAssetForProfile(af, activeProfilePtr)
		if err := writeFileAtomic(target, rendered, 0o644); err != nil {
			return InitResult{}, err
		}
		manifestFiles[relSlash] = sha256PrefixedHex(rendered)
		installed = append(installed, relSlash)
	}

	sessTarget, err := resolveContainedPath(ws, SessionSettingsRelPath)
	if err != nil {
		return InitResult{}, err
	}
	if err := writeFileAtomic(sessTarget, sessBytes, 0o644); err != nil {
		return InitResult{}, err
	}
	manifestFiles[SessionSettingsRelPath] = sha256PrefixedHex(sessBytes)
	installed = append(installed, SessionSettingsRelPath)

	preservedConfig := false
	if !configExists || opts.Force {
		if err := config.SaveConfig(ws, desiredCfg); err != nil {
			return InitResult{}, fmt.Errorf("save config: %w", err)
		}
	} else {
		preservedConfig = true
	}

	if !profilesExists || opts.Force {
		if err := config.SaveProfiles(ws, desiredProfiles); err != nil {
			return InitResult{}, fmt.Errorf("save profiles: %w", err)
		}
	}

	if err := mergeHooksJSON(ws, binCmd); err != nil {
		return InitResult{}, fmt.Errorf("merge hooks.json: %w", err)
	}

	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	includeEngram := false
	if !opts.ExcludeEngramMCP {
		if opts.IncludeEngramMCP {
			includeEngram = true
		} else if _, err := lookPath("engram"); err == nil {
			includeEngram = true
		}
	}

	engramManaged, err := mergeMCPConfigJSON(ws, binCmd, includeEngram, prevEngramManaged)
	if err != nil {
		return InitResult{}, fmt.Errorf("merge mcp_config.json: %w", err)
	}

	managedMCP := []string{ManagedMCPKey}
	if engramManaged {
		managedMCP = append(managedMCP, EngramMCPKey)
	}

	manifest := Manifest{
		SchemaVersion:   ManifestSchemaVersion,
		Version:         version.Version,
		Files:           manifestFiles,
		ManagedHookKeys: []string{ManagedHookKey},
		ManagedMCPKeys:  managedMCP,
	}
	if err := saveManifest(ws, manifest); err != nil {
		return InitResult{}, fmt.Errorf("save manifest: %w", err)
	}

	out := InitResult{
		InstalledFiles:  installed,
		PreservedConfig: preservedConfig,
		AddedEngramMCP:  engramManaged,
		ManifestPath:    ".agents/odd/.managed-manifest.json",
	}
	if opts.AllowGlobalMCP {
		permRes, regRes, err := EnableGlobalMCP(opts.HomeDir, ResolveBinaryPath(opts.BinaryCommand))
		if err != nil {
			return InitResult{}, fmt.Errorf("enable global MCP: %w", err)
		}
		out.GlobalMCPAllowed = permRes.Enabled
		out.GlobalMCPRegistered = regRes.Registered
		out.GlobalSettingsPath = permRes.SettingsPath
		out.GlobalMCPConfigPath = regRes.ConfigPath
	}

	return out, nil
}
