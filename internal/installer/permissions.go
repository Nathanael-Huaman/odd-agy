package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// SessionSettingsRelPath is the project-scoped Antigravity settings file managed by init/sync/uninstall.
	SessionSettingsRelPath = ".agents/odd/agy-session-settings.json"

	// MCPAllowRule permits odd-agy MCP tools without repeated prompts in Antigravity CLI.
	MCPAllowRule = "mcp(odd-agy/*)"
	// MCPAskRule forces explicit human confirmation for odd_review_consent via Deny > Ask > Allow precedence.
	MCPAskRule = "mcp(odd-agy/odd_review_consent)"
)

// GlobalPermissionsResult summarizes a Grant or Revoke operation on ~/.gemini/antigravity-cli/settings.json.
type GlobalPermissionsResult struct {
	SettingsPath string
	BackupPath   string
	Changed      bool
	Enabled      bool
}

// SessionSettingsPath returns the path to .agents/odd/agy-session-settings.json under workspaceRoot.
func SessionSettingsPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".agents", "odd", "agy-session-settings.json")
}

// GlobalSettingsPath returns the path to <homeDir>/.gemini/antigravity-cli/settings.json.
func GlobalSettingsPath(homeDir string) string {
	return filepath.Join(resolveHomeDir(homeDir), ".gemini", "antigravity-cli", "settings.json")
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
