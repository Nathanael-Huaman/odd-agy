package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
)

const (
	// SessionSettingsRelPath is the project-scoped Antigravity settings file managed by init/sync/uninstall.
	SessionSettingsRelPath = ".agents/odd/agy-session-settings.json"

	// MCPAllowRule permits odd-agy MCP tools without repeated prompts in Antigravity CLI.
	MCPAllowRule = "mcp(odd-agy/*)"
	// MCPAskRule forces explicit human confirmation for odd_review_consent via Deny > Ask > Allow precedence.
	MCPAskRule = "mcp(odd-agy/odd_review_consent)"
)

// GlobalPermissionsResult summarizes a Grant or Revoke operation on
// ~/.gemini/antigravity-cli/settings.json and/or ~/.gemini/config/config.json.
type GlobalPermissionsResult struct {
	SettingsPath  string
	BackupPath    string
	Changed       bool
	Enabled       bool
	AppConfigPath string
	AppBackupPath string
	AppChanged    bool
	AppEnabled    bool
}

// GlobalMCPRegistrationResult summarizes a Register or Unregister operation on ~/.gemini/config/mcp_config.json.
type GlobalMCPRegistrationResult struct {
	ConfigPath string
	BackupPath string
	Command    string
	Changed    bool
	Registered bool
}

// SessionSettingsPath returns the path to .agents/odd/agy-session-settings.json under workspaceRoot.
func SessionSettingsPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".agents", "odd", "agy-session-settings.json")
}

// GlobalSettingsPath returns the path to <homeDir>/.gemini/antigravity-cli/settings.json.
func GlobalSettingsPath(homeDir string) string {
	return filepath.Join(resolveHomeDir(homeDir), ".gemini", "antigravity-cli", "settings.json")
}

// GlobalAppConfigPath returns the path to <homeDir>/.gemini/config/config.json.
func GlobalAppConfigPath(homeDir string) string {
	return filepath.Join(resolveHomeDir(homeDir), ".gemini", "config", "config.json")
}

// GlobalMCPConfigPath returns the path to <homeDir>/.gemini/config/mcp_config.json.
func GlobalMCPConfigPath(homeDir string) string {
	return filepath.Join(resolveHomeDir(homeDir), ".gemini", "config", "mcp_config.json")
}

// ResolveBinaryPath resolves the odd-agy command path from an explicit value,
// PATH lookup, the running non-test executable, or falls back to "odd-agy".
func ResolveBinaryPath(explicit string) string {
	if trimmed := strings.TrimSpace(explicit); trimmed != "" {
		return trimmed
	}
	if p, err := exec.LookPath("odd-agy"); err == nil && strings.TrimSpace(p) != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" && !isTestBinaryPath(exe) {
		return exe
	}
	return config.DefaultBinaryCommand
}

func isTestBinaryPath(exe string) bool {
	base := strings.ToLower(filepath.Base(exe))
	if strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
		return true
	}
	return strings.Contains(exe, string(filepath.Separator)+"go-build")
}

func resolveHomeDir(homeDir string) string {
	if trimmed := strings.TrimSpace(homeDir); trimmed != "" {
		return trimmed
	}
	h, _ := os.UserHomeDir()
	return strings.TrimSpace(h)
}

func sessionSettingsBytes() ([]byte, error) {
	payload := map[string]any{
		"permissions": map[string]any{
			"allow": []string{MCPAllowRule},
			"ask":   []string{MCPAskRule},
		},
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal session settings: %w", err)
	}
	return append(data, '\n'), nil
}

// GrantGlobalMCPPermissions ensures mcp(odd-agy/*) is in permissions.allow and
// mcp(odd-agy/odd_review_consent) is in permissions.ask inside <homeDir>/.gemini/antigravity-cli/settings.json.
// If the file already exists and needs changes, a timestamped backup is written first.
func GrantGlobalMCPPermissions(homeDir string) (GlobalPermissionsResult, error) {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return GlobalPermissionsResult{}, errors.New("home directory unavailable")
	}
	if err := checkGlobalSettingsSymlinks(hd); err != nil {
		return GlobalPermissionsResult{}, err
	}
	p := GlobalSettingsPath(hd)

	var origBytes []byte
	doc, exists, err := readJSONObject(p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}
	if exists {
		origBytes, err = os.ReadFile(p)
		if err != nil {
			return GlobalPermissionsResult{}, fmt.Errorf("read %s for backup: %w", p, err)
		}
	}

	perms, err := extractPermissionsMap(doc, p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}

	allow, err := extractRuleSlice(perms, "allow", p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}
	ask, err := extractRuleSlice(perms, "ask", p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}

	changed := !exists
	if !containsRule(allow, MCPAllowRule) {
		allow = append(allow, MCPAllowRule)
		changed = true
	}
	if !containsRule(ask, MCPAskRule) {
		ask = append(ask, MCPAskRule)
		changed = true
	}

	if !changed {
		return GlobalPermissionsResult{
			SettingsPath: p,
			Changed:      false,
			Enabled:      true,
		}, nil
	}

	var backupPath string
	if exists {
		backupPath, err = allocateSettingsBackupPath(p)
		if err != nil {
			return GlobalPermissionsResult{}, err
		}
		if err := writeFileAtomic(backupPath, origBytes, 0o600); err != nil {
			return GlobalPermissionsResult{}, fmt.Errorf("write backup %s: %w", backupPath, err)
		}
	}

	perms["allow"] = allow
	perms["ask"] = ask
	doc["permissions"] = perms

	if err := writeJSONAtomic(p, doc, 0o600); err != nil {
		return GlobalPermissionsResult{}, err
	}

	return GlobalPermissionsResult{
		SettingsPath: p,
		BackupPath:   backupPath,
		Changed:      true,
		Enabled:      true,
	}, nil
}

// RevokeGlobalMCPPermissions idempotently removes only mcp(odd-agy/*) from permissions.allow
// and mcp(odd-agy/odd_review_consent) from permissions.ask in <homeDir>/.gemini/antigravity-cli/settings.json.
func RevokeGlobalMCPPermissions(homeDir string) (GlobalPermissionsResult, error) {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return GlobalPermissionsResult{}, errors.New("home directory unavailable")
	}
	if err := checkGlobalSettingsSymlinks(hd); err != nil {
		return GlobalPermissionsResult{}, err
	}
	p := GlobalSettingsPath(hd)

	doc, exists, err := readJSONObject(p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}
	if !exists {
		return GlobalPermissionsResult{
			SettingsPath: p,
			Changed:      false,
			Enabled:      false,
		}, nil
	}

	rawPerms, hasPerms := doc["permissions"]
	if !hasPerms || rawPerms == nil {
		return GlobalPermissionsResult{
			SettingsPath: p,
			Changed:      false,
			Enabled:      false,
		}, nil
	}
	perms, ok := rawPerms.(map[string]any)
	if !ok {
		return GlobalPermissionsResult{}, fmt.Errorf("%s: permissions must be a JSON object", p)
	}

	allow, err := extractRuleSlice(perms, "allow", p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}
	ask, err := extractRuleSlice(perms, "ask", p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}

	newAllow, allowRemoved := removeRule(allow, MCPAllowRule)
	newAsk, askRemoved := removeRule(ask, MCPAskRule)
	if !allowRemoved && !askRemoved {
		return GlobalPermissionsResult{
			SettingsPath: p,
			Changed:      false,
			Enabled:      false,
		}, nil
	}

	if allowRemoved {
		if len(newAllow) == 0 {
			delete(perms, "allow")
		} else {
			perms["allow"] = newAllow
		}
	}
	if askRemoved {
		if len(newAsk) == 0 {
			delete(perms, "ask")
		} else {
			perms["ask"] = newAsk
		}
	}
	if len(perms) == 0 {
		delete(doc, "permissions")
	} else {
		doc["permissions"] = perms
	}

	if err := writeJSONAtomic(p, doc, 0o600); err != nil {
		return GlobalPermissionsResult{}, err
	}

	return GlobalPermissionsResult{
		SettingsPath: p,
		Changed:      true,
		Enabled:      false,
	}, nil
}

// CheckGlobalMCPPermissions reports whether both odd-agy rules are present in
// <homeDir>/.gemini/antigravity-cli/settings.json without modifying disk.
func CheckGlobalMCPPermissions(homeDir string) (bool, error) {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return false, errors.New("home directory unavailable")
	}
	if err := checkGlobalSettingsSymlinks(hd); err != nil {
		return false, err
	}
	p := GlobalSettingsPath(hd)

	doc, exists, err := readJSONObject(p)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}

	rawPerms, hasPerms := doc["permissions"]
	if !hasPerms || rawPerms == nil {
		return false, nil
	}
	perms, ok := rawPerms.(map[string]any)
	if !ok {
		return false, fmt.Errorf("%s: permissions must be a JSON object", p)
	}

	allow, err := extractRuleSlice(perms, "allow", p)
	if err != nil {
		return false, err
	}
	ask, err := extractRuleSlice(perms, "ask", p)
	if err != nil {
		return false, err
	}

	return containsRule(allow, MCPAllowRule) && containsRule(ask, MCPAskRule), nil
}

// GrantGlobalAppMCPPermissions ensures mcp(odd-agy/*) is in userSettings.globalPermissionGrants.allow
// and mcp(odd-agy/odd_review_consent) is in userSettings.globalPermissionGrants.ask inside
// <homeDir>/.gemini/config/config.json while preserving all unrelated keys and rules.
// If the file already exists and needs changes, a timestamped 0600 backup is written first.
func GrantGlobalAppMCPPermissions(homeDir string) (GlobalPermissionsResult, error) {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return GlobalPermissionsResult{}, errors.New("home directory unavailable")
	}
	if err := checkGlobalAppConfigSymlinks(hd); err != nil {
		return GlobalPermissionsResult{}, err
	}
	p := GlobalAppConfigPath(hd)

	var origBytes []byte
	doc, exists, err := readJSONObject(p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}
	if exists {
		origBytes, err = os.ReadFile(p)
		if err != nil {
			return GlobalPermissionsResult{}, fmt.Errorf("read %s for backup: %w", p, err)
		}
	}

	userSettings, grants, allow, ask, err := extractAppGrants(doc, p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}

	changed := !exists
	if !containsRule(allow, MCPAllowRule) {
		allow = append(allow, MCPAllowRule)
		changed = true
	}
	if !containsRule(ask, MCPAskRule) {
		ask = append(ask, MCPAskRule)
		changed = true
	}

	if !changed {
		return GlobalPermissionsResult{
			SettingsPath:  p,
			Changed:       false,
			Enabled:       true,
			AppConfigPath: p,
			AppChanged:    false,
			AppEnabled:    true,
		}, nil
	}

	var backupPath string
	if exists {
		backupPath, err = allocateSettingsBackupPath(p)
		if err != nil {
			return GlobalPermissionsResult{}, err
		}
		if err := writeFileAtomic(backupPath, origBytes, 0o600); err != nil {
			return GlobalPermissionsResult{}, fmt.Errorf("write backup %s: %w", backupPath, err)
		}
	}

	grants["allow"] = allow
	grants["ask"] = ask
	userSettings["globalPermissionGrants"] = grants
	doc["userSettings"] = userSettings

	if err := writeJSONAtomic(p, doc, 0o600); err != nil {
		return GlobalPermissionsResult{}, err
	}

	return GlobalPermissionsResult{
		SettingsPath:  p,
		BackupPath:    backupPath,
		Changed:       true,
		Enabled:       true,
		AppConfigPath: p,
		AppBackupPath: backupPath,
		AppChanged:    true,
		AppEnabled:    true,
	}, nil
}

// RevokeGlobalAppMCPPermissions idempotently removes only mcp(odd-agy/*) from
// userSettings.globalPermissionGrants.allow and mcp(odd-agy/odd_review_consent) from
// userSettings.globalPermissionGrants.ask in <homeDir>/.gemini/config/config.json.
// When modifying an existing file, a timestamped 0600 backup is written first.
func RevokeGlobalAppMCPPermissions(homeDir string) (GlobalPermissionsResult, error) {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return GlobalPermissionsResult{}, errors.New("home directory unavailable")
	}
	if err := checkGlobalAppConfigSymlinks(hd); err != nil {
		return GlobalPermissionsResult{}, err
	}
	p := GlobalAppConfigPath(hd)

	doc, exists, err := readJSONObject(p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}
	if !exists {
		return GlobalPermissionsResult{
			SettingsPath:  p,
			Changed:       false,
			Enabled:       false,
			AppConfigPath: p,
			AppChanged:    false,
			AppEnabled:    false,
		}, nil
	}

	userSettings, grants, allow, ask, err := extractAppGrants(doc, p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}

	newAllow, allowRemoved := removeRule(allow, MCPAllowRule)
	newAsk, askRemoved := removeRule(ask, MCPAskRule)
	if !allowRemoved && !askRemoved {
		return GlobalPermissionsResult{
			SettingsPath:  p,
			Changed:       false,
			Enabled:       false,
			AppConfigPath: p,
			AppChanged:    false,
			AppEnabled:    false,
		}, nil
	}

	origBytes, err := os.ReadFile(p)
	if err != nil {
		return GlobalPermissionsResult{}, fmt.Errorf("read %s for backup: %w", p, err)
	}
	backupPath, err := allocateSettingsBackupPath(p)
	if err != nil {
		return GlobalPermissionsResult{}, err
	}
	if err := writeFileAtomic(backupPath, origBytes, 0o600); err != nil {
		return GlobalPermissionsResult{}, fmt.Errorf("write backup %s: %w", backupPath, err)
	}

	if allowRemoved {
		if len(newAllow) == 0 {
			delete(grants, "allow")
		} else {
			grants["allow"] = newAllow
		}
	}
	if askRemoved {
		if len(newAsk) == 0 {
			delete(grants, "ask")
		} else {
			grants["ask"] = newAsk
		}
	}
	if len(grants) == 0 {
		delete(userSettings, "globalPermissionGrants")
	} else {
		userSettings["globalPermissionGrants"] = grants
	}
	if len(userSettings) == 0 {
		delete(doc, "userSettings")
	} else {
		doc["userSettings"] = userSettings
	}

	if err := writeJSONAtomic(p, doc, 0o600); err != nil {
		return GlobalPermissionsResult{}, err
	}

	return GlobalPermissionsResult{
		SettingsPath:  p,
		BackupPath:    backupPath,
		Changed:       true,
		Enabled:       false,
		AppConfigPath: p,
		AppBackupPath: backupPath,
		AppChanged:    true,
		AppEnabled:    false,
	}, nil
}

// CheckGlobalAppMCPPermissions reports whether both odd-agy rules are present in
// userSettings.globalPermissionGrants in <homeDir>/.gemini/config/config.json without modifying disk.
func CheckGlobalAppMCPPermissions(homeDir string) (bool, error) {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return false, errors.New("home directory unavailable")
	}
	if err := checkGlobalAppConfigSymlinks(hd); err != nil {
		return false, err
	}
	p := GlobalAppConfigPath(hd)

	doc, exists, err := readJSONObject(p)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}

	_, _, allow, ask, err := extractAppGrants(doc, p)
	if err != nil {
		return false, err
	}

	return containsRule(allow, MCPAllowRule) && containsRule(ask, MCPAskRule), nil
}

// GlobalMCPServerRegistered reports whether mcpServers["odd-agy"] is configured and enabled
// in <homeDir>/.gemini/config/mcp_config.json without modifying disk.
func GlobalMCPServerRegistered(homeDir string) (registered bool, command string, err error) {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return false, "", errors.New("home directory unavailable")
	}
	if err := checkGlobalMCPConfigSymlinks(hd); err != nil {
		return false, "", err
	}
	p := GlobalMCPConfigPath(hd)

	doc, exists, err := readJSONObject(p)
	if err != nil {
		return false, "", err
	}
	if !exists {
		return false, "", nil
	}

	rawServers, hasServers := doc["mcpServers"]
	if !hasServers || rawServers == nil {
		return false, "", nil
	}
	servers, ok := rawServers.(map[string]any)
	if !ok {
		return false, "", fmt.Errorf("%s: mcpServers must be a JSON object", p)
	}

	rawOdd, hasOdd := servers[ManagedMCPKey]
	if !hasOdd || rawOdd == nil {
		return false, "", nil
	}
	odd, ok := rawOdd.(map[string]any)
	if !ok {
		return false, "", fmt.Errorf("%s: mcpServers.%s must be a JSON object", p, ManagedMCPKey)
	}
	if disabled, ok := odd["disabled"].(bool); ok && disabled {
		return false, "", nil
	}
	cmd, _ := odd["command"].(string)
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false, "", nil
	}
	return true, cmd, nil
}

// RegisterGlobalMCPServer ensures mcpServers["odd-agy"] is registered in
// <homeDir>/.gemini/config/mcp_config.json with command, args=["mcp"], and disabled=false.
// Existing servers (such as "engram") and top-level keys are preserved, and existing files
// are backed up with 0600 permissions before modification.
func RegisterGlobalMCPServer(homeDir, binaryPath string) (GlobalMCPRegistrationResult, error) {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return GlobalMCPRegistrationResult{}, errors.New("home directory unavailable")
	}
	if err := checkGlobalMCPConfigSymlinks(hd); err != nil {
		return GlobalMCPRegistrationResult{}, err
	}
	cmd := strings.TrimSpace(binaryPath)
	if cmd == "" {
		cmd = config.DefaultBinaryCommand
	}
	p := GlobalMCPConfigPath(hd)

	var origBytes []byte
	doc, exists, err := readJSONObject(p)
	if err != nil {
		return GlobalMCPRegistrationResult{}, err
	}
	if exists {
		origBytes, err = os.ReadFile(p)
		if err != nil {
			return GlobalMCPRegistrationResult{}, fmt.Errorf("read %s for backup: %w", p, err)
		}
	}

	var servers map[string]any
	if rawServers, hasServers := doc["mcpServers"]; hasServers && rawServers != nil {
		typed, ok := rawServers.(map[string]any)
		if !ok {
			return GlobalMCPRegistrationResult{}, fmt.Errorf("%s: mcpServers must be a JSON object", p)
		}
		servers = typed
	} else {
		servers = map[string]any{}
	}

	if exists && isCanonicalOddMCPRegistration(servers[ManagedMCPKey], cmd) {
		return GlobalMCPRegistrationResult{
			ConfigPath: p,
			Command:    cmd,
			Changed:    false,
			Registered: true,
		}, nil
	}

	var backupPath string
	if exists {
		backupPath, err = allocateSettingsBackupPath(p)
		if err != nil {
			return GlobalMCPRegistrationResult{}, err
		}
		if err := writeFileAtomic(backupPath, origBytes, 0o600); err != nil {
			return GlobalMCPRegistrationResult{}, fmt.Errorf("write backup %s: %w", backupPath, err)
		}
	}

	servers[ManagedMCPKey] = map[string]any{
		"command":  cmd,
		"args":     []string{"mcp"},
		"disabled": false,
	}
	doc["mcpServers"] = servers

	if err := writeJSONAtomic(p, doc, 0o600); err != nil {
		return GlobalMCPRegistrationResult{}, err
	}

	return GlobalMCPRegistrationResult{
		ConfigPath: p,
		BackupPath: backupPath,
		Command:    cmd,
		Changed:    true,
		Registered: true,
	}, nil
}

// UnregisterGlobalMCPServer removes only mcpServers["odd-agy"] from
// <homeDir>/.gemini/config/mcp_config.json while preserving all other servers and top-level keys.
func UnregisterGlobalMCPServer(homeDir string) (GlobalMCPRegistrationResult, error) {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return GlobalMCPRegistrationResult{}, errors.New("home directory unavailable")
	}
	if err := checkGlobalMCPConfigSymlinks(hd); err != nil {
		return GlobalMCPRegistrationResult{}, err
	}
	p := GlobalMCPConfigPath(hd)

	doc, exists, err := readJSONObject(p)
	if err != nil {
		return GlobalMCPRegistrationResult{}, err
	}
	if !exists {
		return GlobalMCPRegistrationResult{
			ConfigPath: p,
			Changed:    false,
			Registered: false,
		}, nil
	}

	rawServers, hasServers := doc["mcpServers"]
	if !hasServers || rawServers == nil {
		return GlobalMCPRegistrationResult{
			ConfigPath: p,
			Changed:    false,
			Registered: false,
		}, nil
	}
	servers, ok := rawServers.(map[string]any)
	if !ok {
		return GlobalMCPRegistrationResult{}, fmt.Errorf("%s: mcpServers must be a JSON object", p)
	}
	if _, hasOdd := servers[ManagedMCPKey]; !hasOdd {
		return GlobalMCPRegistrationResult{
			ConfigPath: p,
			Changed:    false,
			Registered: false,
		}, nil
	}

	origBytes, err := os.ReadFile(p)
	if err != nil {
		return GlobalMCPRegistrationResult{}, fmt.Errorf("read %s for backup: %w", p, err)
	}
	backupPath, err := allocateSettingsBackupPath(p)
	if err != nil {
		return GlobalMCPRegistrationResult{}, err
	}
	if err := writeFileAtomic(backupPath, origBytes, 0o600); err != nil {
		return GlobalMCPRegistrationResult{}, fmt.Errorf("write backup %s: %w", backupPath, err)
	}

	delete(servers, ManagedMCPKey)
	doc["mcpServers"] = servers

	if err := writeJSONAtomic(p, doc, 0o600); err != nil {
		return GlobalMCPRegistrationResult{}, err
	}

	return GlobalMCPRegistrationResult{
		ConfigPath: p,
		BackupPath: backupPath,
		Changed:    true,
		Registered: false,
	}, nil
}

// PreflightGlobalMCP performs zero-write validation of all three global Antigravity config files
// (<homeDir>/.gemini/antigravity-cli/settings.json, <homeDir>/.gemini/config/config.json, and
// <homeDir>/.gemini/config/mcp_config.json): it rejects symlinks along any path and validates
// JSON structure and required field types.
func PreflightGlobalMCP(homeDir string) error {
	hd := resolveHomeDir(homeDir)
	if hd == "" {
		return errors.New("home directory unavailable")
	}
	if err := checkGlobalSettingsSymlinks(hd); err != nil {
		return err
	}
	settingsPath := GlobalSettingsPath(hd)
	settingsDoc, _, err := readJSONObject(settingsPath)
	if err != nil {
		return err
	}
	perms, err := extractPermissionsMap(settingsDoc, settingsPath)
	if err != nil {
		return err
	}
	if _, err := extractRuleSlice(perms, "allow", settingsPath); err != nil {
		return err
	}
	if _, err := extractRuleSlice(perms, "ask", settingsPath); err != nil {
		return err
	}

	if err := checkGlobalAppConfigSymlinks(hd); err != nil {
		return err
	}
	appPath := GlobalAppConfigPath(hd)
	appDoc, _, err := readJSONObject(appPath)
	if err != nil {
		return err
	}
	if _, _, _, _, err := extractAppGrants(appDoc, appPath); err != nil {
		return err
	}

	if err := checkGlobalMCPConfigSymlinks(hd); err != nil {
		return err
	}
	mcpPath := GlobalMCPConfigPath(hd)
	mcpDoc, _, err := readJSONObject(mcpPath)
	if err != nil {
		return err
	}
	if rawServers, hasServers := mcpDoc["mcpServers"]; hasServers && rawServers != nil {
		if _, ok := rawServers.(map[string]any); !ok {
			return fmt.Errorf("%s: mcpServers must be a JSON object", mcpPath)
		}
	}
	return nil
}

// EnableGlobalMCP performs two-phase global MCP activation:
// Phase 1 preflights symlinks and schemas on settings.json, config.json, and mcp_config.json with zero writes;
// Phase 2 grants CLI & Desktop App permissions and registers mcpServers["odd-agy"] with 0600 backups.
func EnableGlobalMCP(homeDir, binaryPath string) (GlobalPermissionsResult, GlobalMCPRegistrationResult, error) {
	if err := PreflightGlobalMCP(homeDir); err != nil {
		return GlobalPermissionsResult{}, GlobalMCPRegistrationResult{}, err
	}
	permRes, err := GrantGlobalMCPPermissions(homeDir)
	if err != nil {
		return GlobalPermissionsResult{}, GlobalMCPRegistrationResult{}, err
	}
	appRes, err := GrantGlobalAppMCPPermissions(homeDir)
	if err != nil {
		return permRes, GlobalMCPRegistrationResult{}, err
	}
	permRes.AppConfigPath = appRes.AppConfigPath
	permRes.AppBackupPath = appRes.AppBackupPath
	permRes.AppChanged = appRes.AppChanged
	permRes.AppEnabled = appRes.AppEnabled

	regRes, err := RegisterGlobalMCPServer(homeDir, binaryPath)
	if err != nil {
		return permRes, GlobalMCPRegistrationResult{}, err
	}
	return permRes, regRes, nil
}

// DisableGlobalMCP performs two-phase global MCP deactivation:
// Phase 1 preflights symlinks and schemas on settings.json, config.json, and mcp_config.json with zero writes;
// Phase 2 revokes CLI & Desktop App permissions and unregisters mcpServers["odd-agy"] cleanly.
func DisableGlobalMCP(homeDir string) (GlobalPermissionsResult, GlobalMCPRegistrationResult, error) {
	if err := PreflightGlobalMCP(homeDir); err != nil {
		return GlobalPermissionsResult{}, GlobalMCPRegistrationResult{}, err
	}
	permRes, err := RevokeGlobalMCPPermissions(homeDir)
	if err != nil {
		return GlobalPermissionsResult{}, GlobalMCPRegistrationResult{}, err
	}
	appRes, err := RevokeGlobalAppMCPPermissions(homeDir)
	if err != nil {
		return permRes, GlobalMCPRegistrationResult{}, err
	}
	permRes.AppConfigPath = appRes.AppConfigPath
	permRes.AppBackupPath = appRes.AppBackupPath
	permRes.AppChanged = appRes.AppChanged
	permRes.AppEnabled = appRes.AppEnabled

	regRes, err := UnregisterGlobalMCPServer(homeDir)
	if err != nil {
		return permRes, GlobalMCPRegistrationResult{}, err
	}
	return permRes, regRes, nil
}

func checkGlobalSettingsSymlinks(hd string) error {
	return checkGlobalFilePathSymlinks(hd, ".gemini", "antigravity-cli", "settings.json")
}

func checkGlobalAppConfigSymlinks(hd string) error {
	return checkGlobalFilePathSymlinks(hd, ".gemini", "config", "config.json")
}

func checkGlobalMCPConfigSymlinks(hd string) error {
	return checkGlobalFilePathSymlinks(hd, ".gemini", "config", "mcp_config.json")
}

func checkGlobalFilePathSymlinks(hd string, parts ...string) error {
	if len(parts) == 0 {
		return nil
	}
	cur := hd
	last := len(parts) - 1
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("lstat %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlink is not allowed", cur)
		}
		if i < last && !info.IsDir() {
			return fmt.Errorf("%s: expected directory", cur)
		}
		if i == last && info.IsDir() {
			return fmt.Errorf("%s: expected file, got directory", cur)
		}
	}
	return nil
}

func isCanonicalOddMCPRegistration(raw any, wantCmd string) bool {
	entry, ok := raw.(map[string]any)
	if !ok || len(entry) != 3 {
		return false
	}
	cmd, ok := entry["command"].(string)
	if !ok || cmd != wantCmd {
		return false
	}
	disabled, ok := entry["disabled"].(bool)
	if !ok || disabled {
		return false
	}
	switch args := entry["args"].(type) {
	case []any:
		if len(args) != 1 {
			return false
		}
		s, ok := args[0].(string)
		return ok && s == "mcp"
	case []string:
		return len(args) == 1 && args[0] == "mcp"
	default:
		return false
	}
}

func extractPermissionsMap(doc map[string]any, filePath string) (map[string]any, error) {
	raw, exists := doc["permissions"]
	if !exists || raw == nil {
		return map[string]any{}, nil
	}
	perms, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: permissions must be a JSON object", filePath)
	}
	return perms, nil
}

func extractRuleSlice(perms map[string]any, key, filePath string) ([]any, error) {
	raw, exists := perms[key]
	if !exists || raw == nil {
		return []any{}, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: permissions.%s must be a JSON array", filePath, key)
	}
	return slices.Clone(arr), nil
}

func extractAppGrants(doc map[string]any, filePath string) (map[string]any, map[string]any, []any, []any, error) {
	var userSettings map[string]any
	if rawUS, exists := doc["userSettings"]; exists && rawUS != nil {
		typed, ok := rawUS.(map[string]any)
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("%s: userSettings must be a JSON object", filePath)
		}
		userSettings = typed
	} else {
		userSettings = map[string]any{}
	}

	var grants map[string]any
	if rawGrants, exists := userSettings["globalPermissionGrants"]; exists && rawGrants != nil {
		typed, ok := rawGrants.(map[string]any)
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("%s: userSettings.globalPermissionGrants must be a JSON object", filePath)
		}
		grants = typed
	} else {
		grants = map[string]any{}
	}

	allow, err := extractAppRuleSlice(grants, "allow", filePath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ask, err := extractAppRuleSlice(grants, "ask", filePath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return userSettings, grants, allow, ask, nil
}

func extractAppRuleSlice(grants map[string]any, key, filePath string) ([]any, error) {
	raw, exists := grants[key]
	if !exists || raw == nil {
		return []any{}, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: userSettings.globalPermissionGrants.%s must be a JSON array", filePath, key)
	}
	return slices.Clone(arr), nil
}

func containsRule(rules []any, target string) bool {
	for _, r := range rules {
		if s, ok := r.(string); ok && s == target {
			return true
		}
	}
	return false
}

func removeRule(rules []any, target string) ([]any, bool) {
	out := make([]any, 0, len(rules))
	removed := false
	for _, r := range rules {
		if s, ok := r.(string); ok && s == target {
			removed = true
			continue
		}
		out = append(out, r)
	}
	return out, removed
}

func allocateSettingsBackupPath(settingsPath string) (string, error) {
	ts := time.Now().UTC().Format("20060102T150405Z")
	for i := 0; i < 1000; i++ {
		suffix := ts
		if i > 0 {
			suffix = fmt.Sprintf("%s-%d", ts, i)
		}
		candidate := fmt.Sprintf("%s.odd-agy.bak.%s", settingsPath, suffix)
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", errors.New("could not allocate unique settings backup path")
}
