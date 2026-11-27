package installer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Nathanael-Huaman/odd-agy/internal/assets"
	"github.com/Nathanael-Huaman/odd-agy/internal/config"
)

const (
	ManifestRelPath    = ".agents/odd/.managed-manifest.json"
	preToolUseMatcher  = "write_to_file|replace_file_content|multi_replace_file_content|invoke_subagent|send_message|run_command|view_file|mcp_odd-agy_.*|odd-agy/.*|McpOdd-agy.*|call_mcp_tool"
	postToolUseMatcher = "write_to_file|replace_file_content|multi_replace_file_content|invoke_subagent|run_command"
)

// ManifestPath returns the path to .agents/odd/.managed-manifest.json under workspaceRoot.
func ManifestPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".agents", "odd", ".managed-manifest.json")
}

// HooksPath returns the path to .agents/hooks.json under workspaceRoot.
func HooksPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".agents", "hooks.json")
}

// MCPConfigPath returns the path to .agents/mcp_config.json under workspaceRoot.
func MCPConfigPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".agents", "mcp_config.json")
}

// LoadManifest reads and validates .agents/odd/.managed-manifest.json under workspaceRoot.
func LoadManifest(workspaceRoot string) (Manifest, error) {
	p := ManifestPath(workspaceRoot)
	data, err := os.ReadFile(p)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %s: %w", p, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", p, err)
	}
	if m.SchemaVersion != ManifestSchemaVersion {
		return Manifest{}, fmt.Errorf("invalid manifest schemaVersion %d: must be %d", m.SchemaVersion, ManifestSchemaVersion)
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return m, nil
}

func saveManifest(workspaceRoot string, m Manifest) error {
	p, err := resolveContainedPath(workspaceRoot, ManifestRelPath)
	if err != nil {
		return err
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return writeJSONAtomic(p, m, 0o644)
}

func sha256PrefixedHex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateWorkspaceRoot(workspaceRoot string) (string, error) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return "", errors.New("workspaceRoot must not be empty")
	}
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspaceRoot %q: %w", workspaceRoot, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat workspaceRoot %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspaceRoot %q is not a directory", abs)
	}
	return abs, nil
}

func resolveContainedPath(workspaceRoot, relSlash string) (string, error) {
	if strings.TrimSpace(relSlash) == "" {
		return "", errors.New("empty relative path")
	}
	if strings.HasPrefix(relSlash, "/") || strings.Contains(relSlash, `\`) {
		return "", fmt.Errorf("path %q must be a repo-relative slash path", relSlash)
	}
	cleaned := path.Clean(relSlash)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || !strings.HasPrefix(cleaned, ".agents/") {
		return "", fmt.Errorf("path %q escapes .agents/", relSlash)
	}

	agentsRoot := filepath.Join(workspaceRoot, ".agents")
	target := filepath.Join(workspaceRoot, filepath.FromSlash(cleaned))
	rel, err := filepath.Rel(agentsRoot, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes .agents/", relSlash)
	}

	cur := workspaceRoot
	for _, part := range strings.Split(cleaned, "/") {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			return "", fmt.Errorf("lstat %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path %q traverses symlink %s", relSlash, cur)
		}
	}
	return target, nil
}

func writeJSONAtomic(targetPath string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json for %s: %w", targetPath, err)
	}
	data = append(data, '\n')
	return writeFileAtomic(targetPath, data, perm)
}

func writeFileAtomic(targetPath string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-odd-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, targetPath, err)
	}
	cleanup = false
	return nil
}

func readJSONObject(targetPath string) (map[string]any, bool, error) {
	data, err := os.ReadFile(targetPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", targetPath, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, true, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, true, fmt.Errorf("parse JSON object %s: %w", targetPath, err)
	}
	if obj == nil {
		return nil, true, fmt.Errorf("parse JSON object %s: expected object, got null", targetPath)
	}
	return obj, true, nil
}

func shellQuoteIfNeeded(bin string) string {
	for i := 0; i < len(bin); i++ {
		c := bin[i]
		isSafe := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '/' || c == '-' || c == ':'
		if !isSafe {
			return "'" + strings.ReplaceAll(bin, "'", `'\''`) + "'"
		}
	}
	return bin
}

func buildManagedHooksEntry(binaryCommand string) map[string]any {
	cmdPrefix := shellQuoteIfNeeded(binaryCommand)
	cmdEntry := func(event string) map[string]any {
		return map[string]any{
			"type":    "command",
			"command": cmdPrefix + " hook " + event,
		}
	}
	return map[string]any{
		"SessionStart": []any{
			map[string]any{
				"hooks": []any{cmdEntry("SessionStart")},
			},
		},
		"PreToolUse": []any{
			map[string]any{
				"matcher": preToolUseMatcher,
				"hooks":   []any{cmdEntry("PreToolUse")},
			},
		},
		"PostToolUse": []any{
			map[string]any{
				"matcher": postToolUseMatcher,
				"hooks":   []any{cmdEntry("PostToolUse")},
			},
		},
		"PreInvocation": []any{
			map[string]any{
				"hooks": []any{cmdEntry("PreInvocation")},
			},
		},
		"Stop": []any{
			map[string]any{
				"hooks": []any{cmdEntry("Stop")},
			},
		},
	}
}

func buildOddMCPEntry(binaryCommand string) map[string]any {
	return map[string]any{
		"command":        binaryCommand,
		"args":           []string{"mcp"},
		"timeoutSeconds": 600,
		"tools": map[string]any{
			"todo": map[string]any{
				"eager": true,
			},
			"odd_review_assess": map[string]any{
				"eager": true,
			},
			"odd_external_run": map[string]any{
				"background": "always",
			},
			"odd_external_continue": map[string]any{
				"background": "always",
			},
		},
	}
}

func buildEngramMCPEntry() map[string]any {
	return map[string]any{
		"command": "engram",
		"args":    []string{"mcp"},
	}
}

func mergeHooksJSON(workspaceRoot, binaryCommand string) error {
	p, err := resolveContainedPath(workspaceRoot, ".agents/hooks.json")
	if err != nil {
		return err
	}
	doc, _, err := readJSONObject(p)
	if err != nil {
		return err
	}
	doc[ManagedHookKey] = buildManagedHooksEntry(binaryCommand)
	return writeJSONAtomic(p, doc, 0o644)
}

func mergeMCPConfigJSON(workspaceRoot, binaryCommand string, includeEngram, prevEngramManaged bool) (bool, error) {
	p, err := resolveContainedPath(workspaceRoot, ".agents/mcp_config.json")
	if err != nil {
		return false, err
	}
	doc, _, err := readJSONObject(p)
	if err != nil {
		return false, err
	}

	var servers map[string]any
	if raw, exists := doc["mcpServers"]; exists && raw != nil {
		typed, ok := raw.(map[string]any)
		if !ok {
			return false, fmt.Errorf("%s: mcpServers must be a JSON object", p)
		}
		servers = typed
	} else {
		servers = map[string]any{}
	}

	servers[ManagedMCPKey] = buildOddMCPEntry(binaryCommand)

	engramManaged := false
	if includeEngram {
		if _, exists := servers[EngramMCPKey]; !exists || prevEngramManaged {
			servers[EngramMCPKey] = buildEngramMCPEntry()
			engramManaged = true
		}
	} else if prevEngramManaged {
		delete(servers, EngramMCPKey)
	}

	doc["mcpServers"] = servers
	if err := writeJSONAtomic(p, doc, 0o644); err != nil {
		return false, err
	}
	return engramManaged, nil
}

func roleForAgentRelPath(relSlash string) (string, bool) {
	for _, role := range config.AllRoles {
		if p, ok := config.RoleAgentRelPath(role); ok && p == relSlash {
			return role, true
		}
	}
	return "", false
}

// NormalizeAntigravityAgentFrontmatterModel maps a configured role model ID to
// the valid Antigravity CLI .agents/agents/*.md frontmatter enum: inherit | flash | pro.
func NormalizeAntigravityAgentFrontmatterModel(role, model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch lower {
	case "", "inherit":
		return "inherit"
	case "flash":
		return "flash"
	case "pro":
		return "pro"
	}
	if role == config.RoleOrchestrator {
		return "inherit"
	}
	if strings.Contains(lower, "flash") {
		return "flash"
	}
	return "pro"
}

func renderAssetForProfile(af assets.AssetFile, pf *config.ProfileConfig) []byte {
	if pf == nil || af.Category != assets.CategoryAgent {
		return af.Content
	}
	role, ok := roleForAgentRelPath(".agents/" + af.RelPath)
	if !ok {
		return af.Content
	}
	ra, hasRole := pf.Roles[role]
	if !hasRole || ra.Runtime != config.RuntimeAgyNative {
		return af.Content
	}
	if strings.TrimSpace(ra.Model) == "" {
		return af.Content
	}
	model := NormalizeAntigravityAgentFrontmatterModel(role, ra.Model)
	updated, _, err := replaceFrontmatterModel(af.Content, model)
	if err != nil {
		return af.Content
	}
	return updated
}

func replaceFrontmatterModel(data []byte, model string) ([]byte, bool, error) {
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return data, false, errors.New("model must not be empty")
	}
	for _, r := range trimmedModel {
		if r < 0x20 || r == 0x7f {
			return data, false, errors.New("model must not contain control characters")
		}
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) < 3 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return data, false, nil
	}

	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx == -1 {
		return data, false, nil
	}

	replaced := false
	for i := 1; i < closeIdx; i++ {
		hadCR := strings.HasSuffix(lines[i], "\r")
		trimmed := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
		if key, _, ok := strings.Cut(trimmed, ":"); ok && strings.TrimSpace(key) == "model" {
			newLine := "model: " + trimmedModel
			if hadCR {
				newLine += "\r"
			}
			lines[i] = newLine
			replaced = true
			break
		}
	}
	if !replaced {
		return data, false, nil
	}

	out := []byte(strings.Join(lines, "\n"))
	if bytes.Equal(out, data) {
		return data, false, nil
	}
	return out, true, nil
}

// ApplyActiveProfileModels loads .agents/odd/profiles.json and delegates to ApplyProfileModels.
func ApplyActiveProfileModels(workspaceRoot string) (int, error) {
	ws, err := validateWorkspaceRoot(workspaceRoot)
	if err != nil {
		return 0, err
	}

	profs, err := config.LoadProfiles(ws)
	if err != nil {
		return 0, fmt.Errorf("load profiles: %w", err)
	}
	return ApplyProfileModels(ws, profs)
}

// ApplyProfileModels syncs YAML frontmatter model lines in existing .agents/agents/*.md
// files to match the active profile in profs, updating manifest hashes only for files
// whose previous on-disk hash matched the manifest.
//
// Phase 1 (preflight, zero disk writes) validates profs, manifest, and all contained paths
// and computes planned updates in memory. Phase 2 commits pending writes and manifest updates.
func ApplyProfileModels(workspaceRoot string, profs config.ProfilesConfig) (int, error) {
	ws, err := validateWorkspaceRoot(workspaceRoot)
	if err != nil {
		return 0, err
	}

	if err := config.ValidateProfiles(profs); err != nil {
		return 0, fmt.Errorf("validate profiles: %w", err)
	}
	activeProf, ok := profs.Profiles[profs.ActiveProfile]
	if !ok {
		return 0, fmt.Errorf("active profile %q not found", profs.ActiveProfile)
	}

	if _, err := resolveContainedPath(ws, ManifestRelPath); err != nil {
		return 0, err
	}
	if _, err := os.Stat(ManifestPath(ws)); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("stat manifest: %w", err)
	}

	manifest, err := LoadManifest(ws)
	if err != nil {
		return 0, err
	}

	agentAssets, err := assets.Agents()
	if err != nil {
		return 0, fmt.Errorf("load embedded agents: %w", err)
	}

	type plannedWrite struct {
		relSlash        string
		targetPath      string
		content         []byte
		wasCleanManaged bool
	}
	var planned []plannedWrite

	for _, af := range agentAssets {
		relSlash := ".agents/" + af.RelPath
		role, ok := roleForAgentRelPath(relSlash)
		if !ok {
			continue
		}
		ra, hasRole := activeProf.Roles[role]
		if !hasRole {
			continue
		}

		target, err := resolveContainedPath(ws, relSlash)
		if err != nil {
			return 0, err
		}

		diskBytes, err := os.ReadFile(target)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, fmt.Errorf("read %s: %w", relSlash, err)
		}

		var desiredModel string
		if ra.Runtime == config.RuntimeAgyNative && strings.TrimSpace(ra.Model) != "" {
			desiredModel = NormalizeAntigravityAgentFrontmatterModel(role, ra.Model)
		} else {
			fm, _, err := assets.ParseFrontmatter(af.Content)
			if err != nil {
				return 0, fmt.Errorf("parse embedded %s frontmatter: %w", af.RelPath, err)
			}
			if strings.TrimSpace(fm.Model) != "" {
				desiredModel = NormalizeAntigravityAgentFrontmatterModel(role, fm.Model)
			}
		}
		if desiredModel == "" {
			continue
		}

		updated, changed, err := replaceFrontmatterModel(diskBytes, desiredModel)
		if err != nil {
			return 0, fmt.Errorf("update %s model: %w", relSlash, err)
		}
		if !changed {
			continue
		}

		prevHash := sha256PrefixedHex(diskBytes)
		manifestHash, inManifest := manifest.Files[relSlash]
		planned = append(planned, plannedWrite{
			relSlash:        relSlash,
			targetPath:      target,
			content:         updated,
			wasCleanManaged: inManifest && prevHash == manifestHash,
		})
	}

	if len(planned) == 0 {
		return 0, nil
	}

	manifestChanged := false
	for _, w := range planned {
		if err := writeFileAtomic(w.targetPath, w.content, 0o644); err != nil {
			return 0, err
		}
		if w.wasCleanManaged {
			manifest.Files[w.relSlash] = sha256PrefixedHex(w.content)
			manifestChanged = true
		}
	}

	if manifestChanged {
		if err := saveManifest(ws, manifest); err != nil {
			return 0, fmt.Errorf("save manifest: %w", err)
		}
	}

	return len(planned), nil
}
