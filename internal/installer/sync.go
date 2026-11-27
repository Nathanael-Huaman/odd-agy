package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Nathanael-Huaman/odd-agy/internal/assets"
	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

type syncActionKind int

const (
	actionUnchanged syncActionKind = iota
	actionCreate
	actionUpdate
	actionConflict
)

type plannedAssetAction struct {
	relSlash string
	diskPath string
	content  []byte
	newHash  string
	kind     syncActionKind
}

// Sync refreshes managed assets, hooks.json, and mcp_config.json against .agents/odd/.managed-manifest.json.
func Sync(workspaceRoot string, opts SyncOptions) (SyncResult, error) {
	ws, err := validateWorkspaceRoot(workspaceRoot)
	if err != nil {
		return SyncResult{}, err
	}

	manifest, err := LoadManifest(ws)
	if err != nil {
		return SyncResult{}, fmt.Errorf("workspace not initialized (run odd-agy init first): %w", err)
	}

	if err := assets.ValidateAll(); err != nil {
		return SyncResult{}, fmt.Errorf("validate embedded assets: %w", err)
	}
	allAssets, err := assets.All()
	if err != nil {
		return SyncResult{}, fmt.Errorf("load embedded assets: %w", err)
	}

	cfg, err := config.LoadConfig(ws)
	if err != nil {
		return SyncResult{}, fmt.Errorf("load config: %w", err)
	}
	binCmd := strings.TrimSpace(opts.BinaryCommand)
	if binCmd == "" {
		binCmd = cfg.BinaryCommand
	}

	// Preflight hooks.json and mcp_config.json validity before touching any files.
	if _, _, err := readJSONObject(HooksPath(ws)); err != nil {
		return SyncResult{}, err
	}
	mcpDoc, _, err := readJSONObject(MCPConfigPath(ws))
	if err != nil {
		return SyncResult{}, err
	}
	if rawServers, ok := mcpDoc["mcpServers"]; ok && rawServers != nil {
		if _, ok := rawServers.(map[string]any); !ok {
			return SyncResult{}, fmt.Errorf("%s: mcpServers must be a JSON object", MCPConfigPath(ws))
		}
	}

	// Plan all asset actions and detect conflicts BEFORE writing anything.
	actions := make([]plannedAssetAction, 0, len(allAssets))
	expectedSet := make(map[string]bool, len(allAssets))
	var conflicts []string

	for _, af := range allAssets {
		relSlash := ".agents/" + af.RelPath
		expectedSet[relSlash] = true
		diskPath, err := resolveContainedPath(ws, relSlash)
		if err != nil {
			return SyncResult{}, err
		}
		newHash := sha256PrefixedHex(af.Content)
		manifestHash, inManifest := manifest.Files[relSlash]

		diskBytes, err := os.ReadFile(diskPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				actions = append(actions, plannedAssetAction{
					relSlash: relSlash,
					diskPath: diskPath,
					content:  af.Content,
					newHash:  newHash,
					kind:     actionCreate,
				})
				continue
			}
			return SyncResult{}, fmt.Errorf("read %s: %w", relSlash, err)
		}

		diskHash := sha256PrefixedHex(diskBytes)
		if diskHash == newHash {
			actions = append(actions, plannedAssetAction{
				relSlash: relSlash,
				diskPath: diskPath,
				content:  af.Content,
				newHash:  newHash,
				kind:     actionUnchanged,
			})
			continue
		}
		if inManifest && diskHash == manifestHash {
			actions = append(actions, plannedAssetAction{
				relSlash: relSlash,
				diskPath: diskPath,
				content:  af.Content,
				newHash:  newHash,
				kind:     actionUpdate,
			})
			continue
		}

		conflicts = append(conflicts, relSlash)
		actions = append(actions, plannedAssetAction{
			relSlash: relSlash,
			diskPath: diskPath,
			content:  af.Content,
			newHash:  newHash,
			kind:     actionConflict,
		})
	}

	// Check obsolete files that were in manifest.Files but removed from embedded assets.
	type obsoleteAction struct {
		relSlash string
		diskPath string
		conflict bool
	}
	var obsolete []obsoleteAction
	for relSlash, oldHash := range manifest.Files {
		if expectedSet[relSlash] {
			continue
		}
		diskPath, err := resolveContainedPath(ws, relSlash)
		if err != nil {
			return SyncResult{}, err
		}
		diskBytes, err := os.ReadFile(diskPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return SyncResult{}, fmt.Errorf("read obsolete managed file %s: %w", relSlash, err)
		}
		isConflict := sha256PrefixedHex(diskBytes) != oldHash
		if isConflict {
			conflicts = append(conflicts, relSlash)
		}
		obsolete = append(obsolete, obsoleteAction{
			relSlash: relSlash,
			diskPath: diskPath,
			conflict: isConflict,
		})
	}
	sort.Slice(obsolete, func(i, j int) bool {
		return obsolete[i].relSlash < obsolete[j].relSlash
	})
	sort.Strings(conflicts)

	if len(conflicts) > 0 && !opts.Force {
		return SyncResult{Conflicts: conflicts}, fmt.Errorf("sync aborted: %d user-modified file(s) conflict (%s); re-run with Force to backup and overwrite", len(conflicts), strings.Join(conflicts, ", "))
	}

	var result SyncResult
	var backupRootRel string
	if len(conflicts) > 0 && opts.Force {
		backupRootRel, err = allocateBackupDir(ws)
		if err != nil {
			return SyncResult{}, err
		}
	}

	backupFile := func(relSlash, diskPath string) error {
		data, err := os.ReadFile(diskPath)
		if err != nil {
			return fmt.Errorf("read %s for backup: %w", relSlash, err)
		}
		relUnderAgents := strings.TrimPrefix(relSlash, ".agents/")
		backupRel := filepath.ToSlash(filepath.Join(backupRootRel, relUnderAgents))
		backupAbs, err := resolveContainedPath(ws, backupRel)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(backupAbs, data, 0o600); err != nil {
			return fmt.Errorf("write backup %s: %w", backupRel, err)
		}
		result.BackedUp = append(result.BackedUp, backupRel)
		return nil
	}

	newFiles := make(map[string]string, len(actions))
	for _, act := range actions {
		switch act.kind {
		case actionUnchanged:
			result.Unchanged = append(result.Unchanged, act.relSlash)
		case actionCreate:
			if err := writeFileAtomic(act.diskPath, act.content, 0o644); err != nil {
				return SyncResult{}, err
			}
			result.Created = append(result.Created, act.relSlash)
		case actionUpdate:
			if err := writeFileAtomic(act.diskPath, act.content, 0o644); err != nil {
				return SyncResult{}, err
			}
			result.Updated = append(result.Updated, act.relSlash)
		case actionConflict:
			if err := backupFile(act.relSlash, act.diskPath); err != nil {
				return SyncResult{}, err
			}
			if err := writeFileAtomic(act.diskPath, act.content, 0o644); err != nil {
				return SyncResult{}, err
			}
			result.Updated = append(result.Updated, act.relSlash)
		}
		newFiles[act.relSlash] = act.newHash
	}

	for _, obs := range obsolete {
		if obs.conflict {
			if err := backupFile(obs.relSlash, obs.diskPath); err != nil {
				return SyncResult{}, err
			}
		}
		if err := os.Remove(obs.diskPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return SyncResult{}, fmt.Errorf("remove obsolete %s: %w", obs.relSlash, err)
		}
		result.Removed = append(result.Removed, obs.relSlash)
	}

	if err := mergeHooksJSON(ws, binCmd); err != nil {
		return SyncResult{}, fmt.Errorf("refresh hooks.json: %w", err)
	}
	engramManaged := slices.Contains(manifest.ManagedMCPKeys, EngramMCPKey)
	if _, err := mergeMCPConfigJSON(ws, binCmd, engramManaged, engramManaged); err != nil {
		return SyncResult{}, fmt.Errorf("refresh mcp_config.json: %w", err)
	}

	manifest.SchemaVersion = ManifestSchemaVersion
	manifest.Version = version.Version
	manifest.Files = newFiles
	if !slices.Contains(manifest.ManagedHookKeys, ManagedHookKey) {
		manifest.ManagedHookKeys = append(manifest.ManagedHookKeys, ManagedHookKey)
	}
	if !slices.Contains(manifest.ManagedMCPKeys, ManagedMCPKey) {
		manifest.ManagedMCPKeys = append(manifest.ManagedMCPKeys, ManagedMCPKey)
	}
	if err := saveManifest(ws, manifest); err != nil {
		return SyncResult{}, fmt.Errorf("save manifest: %w", err)
	}

	return result, nil
}

func allocateBackupDir(ws string) (string, error) {
	base := time.Now().UTC().Format("20060102T150405Z")
	for i := 0; i < 1000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		rel := ".agents/odd/.backups/" + name
		abs := filepath.Join(ws, filepath.FromSlash(rel))
		if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
			return rel, nil
		}
	}
	return "", errors.New("could not allocate unique backup directory")
}
