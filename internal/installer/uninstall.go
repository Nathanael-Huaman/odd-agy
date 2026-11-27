package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Uninstall removes managed files matching the manifest, removes managed keys from
// .agents/hooks.json and .agents/mcp_config.json, preserves user edits and unrelated
// user files, and cleans up empty managed directories.
func Uninstall(workspaceRoot string) (UninstallResult, error) {
	ws, err := validateWorkspaceRoot(workspaceRoot)
	if err != nil {
		return UninstallResult{}, err
	}

	manifest, err := LoadManifest(ws)
	if err != nil {
		return UninstallResult{}, fmt.Errorf("load manifest for uninstall: %w", err)
	}

	var result UninstallResult

	// 1. Remove only unmodified managed files.
	relPaths := make([]string, 0, len(manifest.Files))
	for rel := range manifest.Files {
		relPaths = append(relPaths, rel)
	}
	sort.Strings(relPaths)

	skillDirs := map[string]bool{}
	for _, rel := range relPaths {
		diskPath, err := resolveContainedPath(ws, rel)
		if err != nil {
			return UninstallResult{}, err
		}
		parent := filepath.Dir(diskPath)
		if filepath.Base(filepath.Dir(parent)) == "skills" {
			skillDirs[parent] = true
		}

		data, err := os.ReadFile(diskPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return UninstallResult{}, fmt.Errorf("read %s: %w", rel, err)
		}

		if sha256PrefixedHex(data) == manifest.Files[rel] {
			if err := os.Remove(diskPath); err != nil {
				return UninstallResult{}, fmt.Errorf("remove %s: %w", rel, err)
			}
			result.Removed = append(result.Removed, rel)
		} else {
			result.Preserved = append(result.Preserved, rel)
		}
	}

	// 2. Remove managed hook keys from .agents/hooks.json.
	hookKeys := manifest.ManagedHookKeys
	if len(hookKeys) == 0 {
		hookKeys = []string{ManagedHookKey}
	}
	if err := unmergeHooksJSON(ws, hookKeys, &result); err != nil {
		return UninstallResult{}, err
	}

	// 3. Remove managed MCP keys from .agents/mcp_config.json.
	mcpKeys := manifest.ManagedMCPKeys
	if len(mcpKeys) == 0 {
		mcpKeys = []string{ManagedMCPKey}
	}
	if err := unmergeMCPConfigJSON(ws, mcpKeys, &result); err != nil {
		return UninstallResult{}, err
	}

	// 4. Remove manifest.
	mPath := ManifestPath(ws)
	if err := os.Remove(mPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return UninstallResult{}, fmt.Errorf("remove manifest %s: %w", mPath, err)
	}
	result.Removed = append(result.Removed, ".agents/odd/.managed-manifest.json")

	// 5. Remove empty managed directories bottom-up (never removes non-empty dirs).
	var sortedSkillDirs []string
	for d := range skillDirs {
		sortedSkillDirs = append(sortedSkillDirs, d)
	}
	sort.Strings(sortedSkillDirs)
	for _, d := range sortedSkillDirs {
		removeIfEmpty(d)
	}
	removeIfEmpty(filepath.Join(ws, ".agents", "skills"))
	removeIfEmpty(filepath.Join(ws, ".agents", "rules"))
	removeIfEmpty(filepath.Join(ws, ".agents", "agents"))
	removeIfEmpty(filepath.Join(ws, ".agents", "odd"))
	removeIfEmpty(filepath.Join(ws, ".agents"))

	return result, nil
}

func unmergeHooksJSON(ws string, managedKeys []string, res *UninstallResult) error {
	p := HooksPath(ws)
	doc, exists, err := readJSONObject(p)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	changed := false
	for _, k := range managedKeys {
		if _, ok := doc[k]; ok {
			delete(doc, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}

	if len(doc) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty hooks.json: %w", err)
		}
		res.Removed = append(res.Removed, ".agents/hooks.json")
		return nil
	}

	if err := writeJSONAtomic(p, doc, 0o644); err != nil {
		return fmt.Errorf("update hooks.json: %w", err)
	}
	res.Updated = append(res.Updated, ".agents/hooks.json")
	return nil
}

func unmergeMCPConfigJSON(ws string, managedKeys []string, res *UninstallResult) error {
	p := MCPConfigPath(ws)
	doc, exists, err := readJSONObject(p)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	rawServers, ok := doc["mcpServers"]
	if !ok || rawServers == nil {
		return nil
	}
	servers, ok := rawServers.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: mcpServers must be a JSON object", p)
	}

	changed := false
	for _, k := range managedKeys {
		if _, ok := servers[k]; ok {
			delete(servers, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}

	if len(servers) == 0 && len(doc) == 1 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty mcp_config.json: %w", err)
		}
		res.Removed = append(res.Removed, ".agents/mcp_config.json")
		return nil
	}

	doc["mcpServers"] = servers
	if err := writeJSONAtomic(p, doc, 0o644); err != nil {
		return fmt.Errorf("update mcp_config.json: %w", err)
	}
	res.Updated = append(res.Updated, ".agents/mcp_config.json")
	return nil
}

func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(dir)
}
