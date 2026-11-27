package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Nathanael-Huaman/odd-agy/internal/assets"
	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

func fakeLookPath(found map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := found[name]; ok {
			return p, nil
		}
		return "", errors.New("executable file not found in $PATH")
	}
}

func fakeLookupEnv(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

func sha256Prefixed(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestInitCreatesAssetsConfigAndMergesHooksAndMCP(t *testing.T) {
	ws := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Pre-populate user hooks.json and mcp_config.json with custom entries.
	agentsDir := filepath.Join(ws, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir .agents: %v", err)
	}

	userHooks := map[string]any{
		"user-custom-hook": map[string]any{
			"Stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "echo user-stop"},
					},
				},
			},
		},
	}
	writeJSONFile(t, filepath.Join(agentsDir, "hooks.json"), userHooks)

	userMCP := map[string]any{
		"customTopField": "keep-me",
		"mcpServers": map[string]any{
			"user-server": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "my-mcp"},
			},
		},
	}
	writeJSONFile(t, filepath.Join(agentsDir, "mcp_config.json"), userMCP)

	res, err := Init(ws, InitOptions{
		LookPath: fakeLookPath(map[string]string{
			"agy":    "/usr/local/bin/agy",
			"engram": "/usr/local/bin/engram",
		}),
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if len(res.InstalledFiles) == 0 {
		t.Fatalf("expected installed asset files, got 0")
	}

	// Verify ~/.gemini/ was never created or touched.
	if _, err := os.Stat(filepath.Join(fakeHome, ".gemini")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ~/.gemini not to exist, got err=%v", err)
	}

	// Verify all embedded assets were written.
	allAssets, err := assets.All()
	if err != nil {
		t.Fatalf("assets.All: %v", err)
	}
	for _, af := range allAssets {
		diskPath := filepath.Join(ws, ".agents", filepath.FromSlash(af.RelPath))
		got, err := os.ReadFile(diskPath)
		if err != nil {
			t.Fatalf("missing installed asset %s: %v", diskPath, err)
		}
		if string(got) != string(af.Content) {
			t.Fatalf("installed asset %s content mismatch", af.RelPath)
		}
	}

	// Verify default config.json and profiles.json.
	cfg, err := config.LoadConfig(ws)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DelegationMode != config.DelegationModeFull {
		t.Errorf("DelegationMode = %q, want %q", cfg.DelegationMode, config.DelegationModeFull)
	}
	if cfg.InstallMode != config.InstallModeLocal {
		t.Errorf("InstallMode = %q, want %q", cfg.InstallMode, config.InstallModeLocal)
	}
	if cfg.BinaryCommand != "./node_modules/.bin/odd-agy" {
		t.Errorf("BinaryCommand = %q, want ./node_modules/.bin/odd-agy", cfg.BinaryCommand)
	}
	if !cfg.RDDEnabled {
		t.Errorf("RDDEnabled = false, want true")
	}

	profCfg, err := config.LoadProfiles(ws)
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	if profCfg.ActiveProfile != "agy-oauth" {
		t.Errorf("ActiveProfile = %q, want agy-oauth", profCfg.ActiveProfile)
	}

	// Verify .agents/hooks.json schema and preservation of user hook.
	hooksDoc := readJSONFile(t, filepath.Join(agentsDir, "hooks.json"))
	if _, ok := hooksDoc["user-custom-hook"]; !ok {
		t.Fatalf("expected user-custom-hook to be preserved in hooks.json")
	}
	oddHooks, ok := hooksDoc["odd-agy"].(map[string]any)
	if !ok {
		t.Fatalf("expected odd-agy hook map in hooks.json, got %T", hooksDoc["odd-agy"])
	}
	wantEvents := map[string]struct {
		matcher string
		cmd     string
	}{
		"SessionStart": {
			cmd: "./node_modules/.bin/odd-agy hook SessionStart",
		},
		"PreToolUse": {
			matcher: "write_to_file|replace_file_content|multi_replace_file_content|invoke_subagent|send_message|run_command|view_file|mcp_odd-agy_.*|odd-agy/.*|McpOdd-agy.*|call_mcp_tool",
			cmd:     "./node_modules/.bin/odd-agy hook PreToolUse",
		},
		"PostToolUse": {
			matcher: "write_to_file|replace_file_content|multi_replace_file_content|invoke_subagent|run_command",
			cmd:     "./node_modules/.bin/odd-agy hook PostToolUse",
		},
		"PreInvocation": {
			cmd: "./node_modules/.bin/odd-agy hook PreInvocation",
		},
		"Stop": {
			cmd: "./node_modules/.bin/odd-agy hook Stop",
		},
	}
	for event, want := range wantEvents {
		entries, ok := oddHooks[event].([]any)
		if !ok || len(entries) != 1 {
			t.Fatalf("hooks.json event %s: got %#v, want 1 entry", event, oddHooks[event])
		}
		entry, ok := entries[0].(map[string]any)
		if !ok {
			t.Fatalf("hooks.json event %s entry not object", event)
		}
		if want.matcher != "" {
			if gotMatcher, _ := entry["matcher"].(string); gotMatcher != want.matcher {
				t.Errorf("hooks.json %s matcher = %q, want %q", event, gotMatcher, want.matcher)
			}
			if event == "PreToolUse" {
				gotMatcher, _ := entry["matcher"].(string)
				for _, sub := range []string{"odd-agy/.*", "mcp_odd-agy_.*", "McpOdd-agy.*", "call_mcp_tool"} {
					if !strings.Contains(gotMatcher, sub) {
						t.Errorf("hooks.json PreToolUse matcher %q missing %q", gotMatcher, sub)
					}
				}
			}
		} else if _, hasMatcher := entry["matcher"]; hasMatcher {
			t.Errorf("hooks.json %s unexpectedly has matcher", event)
		}
		innerHooks, ok := entry["hooks"].([]any)
		if !ok || len(innerHooks) != 1 {
			t.Fatalf("hooks.json %s inner hooks invalid: %#v", event, entry["hooks"])
		}
		h0 := innerHooks[0].(map[string]any)
		if h0["type"] != "command" || h0["command"] != want.cmd {
			t.Errorf("hooks.json %s command = %#v, want type=command command=%q", event, h0, want.cmd)
		}
	}

	// Verify .agents/mcp_config.json schema and preservation of user MCP server.
	mcpDoc := readJSONFile(t, filepath.Join(agentsDir, "mcp_config.json"))
	if mcpDoc["customTopField"] != "keep-me" {
		t.Errorf("customTopField lost from mcp_config.json")
	}
	servers := mcpDoc["mcpServers"].(map[string]any)
	if _, ok := servers["user-server"]; !ok {
		t.Fatalf("user-server lost from mcp_config.json")
	}
	oddMCP, ok := servers["odd-agy"].(map[string]any)
	if !ok {
		t.Fatalf("odd-agy missing from mcp_config.json")
	}
	if oddMCP["command"] != "./node_modules/.bin/odd-agy" {
		t.Errorf("odd-agy mcp command = %v", oddMCP["command"])
	}
	if oddMCP["timeoutSeconds"] != float64(600) {
		t.Errorf("odd-agy timeoutSeconds = %v, want 600", oddMCP["timeoutSeconds"])
	}
	toolsMap, ok := oddMCP["tools"].(map[string]any)
	if !ok {
		t.Fatalf("odd-agy tools missing or invalid: %T", oddMCP["tools"])
	}
	if toolsMap["todo"].(map[string]any)["eager"] != true {
		t.Errorf("todo.eager != true")
	}
	if toolsMap["odd_review_assess"].(map[string]any)["eager"] != true {
		t.Errorf("odd_review_assess.eager != true")
	}
	if toolsMap["odd_external_run"].(map[string]any)["background"] != "always" {
		t.Errorf("odd_external_run.background != always")
	}
	if toolsMap["odd_external_continue"].(map[string]any)["background"] != "always" {
		t.Errorf("odd_external_continue.background != always")
	}

	engramMCP, ok := servers["engram"].(map[string]any)
	if !ok {
		t.Fatalf("engram missing from mcp_config.json")
	}
	if engramMCP["command"] != "engram" {
		t.Errorf("engram command = %v, want engram", engramMCP["command"])
	}

	// Verify managed manifest.
	manifest, err := LoadManifest(ws)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.SchemaVersion != ManifestSchemaVersion {
		t.Errorf("manifest.SchemaVersion = %d, want %d", manifest.SchemaVersion, ManifestSchemaVersion)
	}
	if manifest.Version != version.Version {
		t.Errorf("manifest.Version = %q, want %q", manifest.Version, version.Version)
	}
	if len(manifest.Files) != len(allAssets)+1 {
		t.Errorf("manifest.Files len = %d, want %d", len(manifest.Files), len(allAssets)+1)
	}
	if _, ok := manifest.Files[SessionSettingsRelPath]; !ok {
		t.Errorf("manifest.Files missing %s", SessionSettingsRelPath)
	}
	if !slices.Contains(manifest.ManagedHookKeys, "odd-agy") {
		t.Errorf("manifest.ManagedHookKeys = %v, want odd-agy", manifest.ManagedHookKeys)
	}
	if !slices.Contains(manifest.ManagedMCPKeys, "odd-agy") || !slices.Contains(manifest.ManagedMCPKeys, "engram") {
		t.Errorf("manifest.ManagedMCPKeys = %v, want [odd-agy engram]", manifest.ManagedMCPKeys)
	}
}

func TestInitIdempotentAndPreservesUserConfigUnlessForce(t *testing.T) {
	ws := t.TempDir()
	opts := InitOptions{
		InstallMode:    config.InstallModeStandalone,
		DelegationMode: config.DelegationModeOrganic,
		Profile:        "deepseek-hybrid",
		LookPath:       fakeLookPath(map[string]string{"agy": "/bin/agy"}),
	}
	if _, err := Init(ws, opts); err != nil {
		t.Fatalf("first Init: %v", err)
	}

	// Mutate config and profile by user.
	cfg, err := config.LoadConfig(ws)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.RDDEnabled = false
	if err := config.SaveConfig(ws, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// Second Init without Force preserves user config.
	if _, err := Init(ws, InitOptions{
		InstallMode:    config.InstallModeLocal,
		DelegationMode: config.DelegationModeFull,
		Profile:        "agy-oauth",
		LookPath:       fakeLookPath(map[string]string{"agy": "/bin/agy"}),
	}); err != nil {
		t.Fatalf("second Init: %v", err)
	}

	cfgAfter, err := config.LoadConfig(ws)
	if err != nil {
		t.Fatalf("LoadConfig after 2nd init: %v", err)
	}
	if cfgAfter.RDDEnabled != false || cfgAfter.DelegationMode != config.DelegationModeOrganic {
		t.Fatalf("second Init overwrote user config without Force: %+v", cfgAfter)
	}
	profAfter, err := config.LoadProfiles(ws)
	if err != nil {
		t.Fatalf("LoadProfiles after 2nd init: %v", err)
	}
	if profAfter.ActiveProfile != "deepseek-hybrid" {
		t.Fatalf("second Init overwrote user profile without Force: %q", profAfter.ActiveProfile)
	}

	// Third Init with Force resets config and profile.
	if _, err := Init(ws, InitOptions{
		InstallMode:    config.InstallModeLocal,
		DelegationMode: config.DelegationModeFull,
		Profile:        "agy-oauth",
		Force:          true,
		LookPath:       fakeLookPath(map[string]string{"agy": "/bin/agy"}),
	}); err != nil {
		t.Fatalf("forced Init: %v", err)
	}
	cfgForced, _ := config.LoadConfig(ws)
	if !cfgForced.RDDEnabled || cfgForced.DelegationMode != config.DelegationModeFull {
		t.Fatalf("forced Init did not reset config: %+v", cfgForced)
	}
	profForced, _ := config.LoadProfiles(ws)
	if profForced.ActiveProfile != "agy-oauth" {
		t.Fatalf("forced Init did not reset profile: %q", profForced.ActiveProfile)
	}
}

func TestInitPreservesExistingUserEngramAndQuotesBinaryWithSpaces(t *testing.T) {
	ws := t.TempDir()
	agentsDir := filepath.Join(ws, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing user engram entry with custom args must NOT be touched or claimed by manifest.
	writeJSONFile(t, filepath.Join(agentsDir, "mcp_config.json"), map[string]any{
		"mcpServers": map[string]any{
			"engram": map[string]any{
				"command": "/custom/engram",
				"args":    []string{"mcp", "--port", "9999"},
			},
		},
	})

	_, err := Init(ws, InitOptions{
		InstallMode:      config.InstallModeStandalone,
		BinaryCommand:    "/opt/My Tools/odd-agy",
		IncludeEngramMCP: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	hooksDoc := readJSONFile(t, filepath.Join(agentsDir, "hooks.json"))
	oddHooks := hooksDoc["odd-agy"].(map[string]any)
	ss := oddHooks["SessionStart"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if ss["command"] != "'/opt/My Tools/odd-agy' hook SessionStart" {
		t.Errorf("quoted hook command = %q", ss["command"])
	}

	mcpDoc := readJSONFile(t, filepath.Join(agentsDir, "mcp_config.json"))
	servers := mcpDoc["mcpServers"].(map[string]any)
	engram := servers["engram"].(map[string]any)
	if engram["command"] != "/custom/engram" {
		t.Errorf("user engram overwritten: %v", engram["command"])
	}

	manifest, err := LoadManifest(ws)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(manifest.ManagedMCPKeys, "engram") {
		t.Errorf("manifest claimed user-owned engram key: %v", manifest.ManagedMCPKeys)
	}
}

func TestSyncCleanUpdateRefusalAndForceBackup(t *testing.T) {
	ws := t.TempDir()
	if _, err := Init(ws, InitOptions{
		LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy"}),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Simulate an older embedded version of odd-01-core.md on disk matching an older manifest hash,
	// and a deleted agent file.
	ruleRel := ".agents/rules/odd-01-core.md"
	rulePath := filepath.Join(ws, filepath.FromSlash(ruleRel))
	oldContent := []byte("---\ntrigger: always_on\n---\nold content\n")
	if err := os.WriteFile(rulePath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadManifest(ws)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Files[ruleRel] = sha256Prefixed(oldContent)
	manifest.Version = "0.0.1"
	if err := saveManifest(ws, manifest); err != nil {
		t.Fatal(err)
	}

	missingRel := ".agents/agents/gentle-ai-explore.md"
	missingPath := filepath.Join(ws, filepath.FromSlash(missingRel))
	if err := os.Remove(missingPath); err != nil {
		t.Fatal(err)
	}

	// Clean sync should update odd-01-core.md and recreate missing agent.
	syncRes, err := Sync(ws, SyncOptions{})
	if err != nil {
		t.Fatalf("clean Sync failed: %v", err)
	}
	if !slices.Contains(syncRes.Updated, ruleRel) {
		t.Errorf("expected %s in Updated, got %v", ruleRel, syncRes.Updated)
	}
	if !slices.Contains(syncRes.Created, missingRel) {
		t.Errorf("expected %s in Created, got %v", missingRel, syncRes.Created)
	}
	if _, err := os.Stat(missingPath); err != nil {
		t.Fatalf("missing file not recreated: %v", err)
	}

	// Now simulate user editing two files: one modified, one outdated.
	// Without Force, Sync MUST refuse all-or-nothing without touching ANY file.
	editedRel := ".agents/rules/odd-02-orchestrator.md"
	editedPath := filepath.Join(ws, filepath.FromSlash(editedRel))
	userEdits := []byte("---\ntrigger: always_on\n---\n# user custom edits\n")
	if err := os.WriteFile(editedPath, userEdits, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulePath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, _ = LoadManifest(ws)
	manifest.Files[ruleRel] = sha256Prefixed(oldContent)
	if err := saveManifest(ws, manifest); err != nil {
		t.Fatal(err)
	}

	refusedRes, err := Sync(ws, SyncOptions{Force: false})
	if err == nil {
		t.Fatalf("expected Sync without Force to fail on user-edited file")
	}
	if !slices.Contains(refusedRes.Conflicts, editedRel) {
		t.Errorf("expected %s in Conflicts, got %v", editedRel, refusedRes.Conflicts)
	}
	// Verify all-or-nothing: rulePath must STILL have oldContent and editedPath must STILL have userEdits.
	if got, _ := os.ReadFile(rulePath); string(got) != string(oldContent) {
		t.Errorf("Sync modified %s despite preflight conflict refusal", ruleRel)
	}
	if got, _ := os.ReadFile(editedPath); string(got) != string(userEdits) {
		t.Errorf("Sync overwrote %s despite conflict refusal", editedRel)
	}

	// With Force: true, Sync backs up editedPath to .agents/odd/.backups/<ts>/ and overwrites cleanly.
	forcedRes, err := Sync(ws, SyncOptions{Force: true})
	if err != nil {
		t.Fatalf("forced Sync failed: %v", err)
	}
	if len(forcedRes.BackedUp) != 1 {
		t.Fatalf("expected 1 backup entry, got %v", forcedRes.BackedUp)
	}
	backupFile := filepath.Join(ws, filepath.FromSlash(forcedRes.BackedUp[0]))
	gotBackup, err := os.ReadFile(backupFile)
	if err != nil {
		t.Fatalf("read backup %s: %v", backupFile, err)
	}
	if string(gotBackup) != string(userEdits) {
		t.Errorf("backup content mismatch: got %q, want %q", string(gotBackup), string(userEdits))
	}
	if !strings.HasPrefix(forcedRes.BackedUp[0], ".agents/odd/.backups/") {
		t.Errorf("unexpected backup location %q", forcedRes.BackedUp[0])
	}
	// Disk file now matches embedded asset.
	allAssets, _ := assets.All()
	for _, af := range allAssets {
		if ".agents/"+af.RelPath == editedRel {
			gotDisk, _ := os.ReadFile(editedPath)
			if string(gotDisk) != string(af.Content) {
				t.Errorf("forced Sync did not restore embedded content for %s", editedRel)
			}
		}
	}
}

func TestDoctorHealthyAndWarningsAndSecretRedaction(t *testing.T) {
	ws := t.TempDir()
	fakeHome := t.TempDir()

	if _, err := Init(ws, InitOptions{
		Profile:        "deepseek-hybrid",
		AllowGlobalMCP: true,
		HomeDir:        fakeHome,
		LookPath: fakeLookPath(map[string]string{
			"agy":    "/bin/agy",
			"engram": "/bin/engram",
		}),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const secretKey = "sk-super-secret-deepseek-value-12345"

	// 1. Healthy green report (with DeepSeek key set -> must NEVER leak secretKey in output).
	report, err := Doctor(ws, DoctorOptions{
		HomeDir: fakeHome,
		LookPath: fakeLookPath(map[string]string{
			"agy":    "/bin/agy",
			"engram": "/bin/engram",
		}),
		LookupEnv: fakeLookupEnv(map[string]string{
			"DEEPSEEK_API_KEY": secretKey,
		}),
	})
	if err != nil {
		t.Fatalf("Doctor healthy: %v", err)
	}
	if report.HasFailures() || report.HasWarnings() {
		t.Fatalf("expected healthy green report, got checks=%+v", report.Checks)
	}
	for _, c := range report.Checks {
		if strings.Contains(c.Detail, secretKey) {
			t.Fatalf("Doctor leaked secret key in check %s: %s", c.Name, c.Detail)
		}
	}

	// 2. Missing engram + global gentle-ai collision + drifted file.
	geminiRulesDir := filepath.Join(fakeHome, ".gemini", "config", "rules")
	if err := os.MkdirAll(geminiRulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(geminiRulesDir, "gentle-ai-core.md"), []byte("# collision"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".gemini", "GEMINI.md"), []byte("<!-- gentle-ai -->\n# old"), 0o644); err != nil {
		t.Fatal(err)
	}

	driftedRule := filepath.Join(ws, ".agents", "rules", "odd-01-core.md")
	originalBytes, _ := os.ReadFile(driftedRule)
	if err := os.WriteFile(driftedRule, append(originalBytes, []byte("\n# drifted\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	warnReport, err := Doctor(ws, DoctorOptions{
		HomeDir: fakeHome,
		LookPath: fakeLookPath(map[string]string{
			"agy": "/bin/agy",
		}),
		LookupEnv: fakeLookupEnv(nil),
	})
	if err != nil {
		t.Fatalf("Doctor warn: %v", err)
	}

	findCheck := func(name string) Check {
		for _, c := range warnReport.Checks {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("missing check %q in %+v", name, warnReport.Checks)
		return Check{}
	}

	if c := findCheck("engram-cli"); c.Status != CheckWarn {
		t.Errorf("engram-cli status = %q, want warn (%s)", c.Status, c.Detail)
	}
	if c := findCheck("global-collision"); c.Status != CheckWarn {
		t.Errorf("global-collision status = %q, want warn (%s)", c.Status, c.Detail)
	}
	if c := findCheck("managed-files"); c.Status != CheckWarn {
		t.Errorf("managed-files status = %q, want warn (%s)", c.Status, c.Detail)
	}
	if c := findCheck("provider-readiness"); c.Status != CheckWarn {
		t.Errorf("provider-readiness status = %q, want warn (%s)", c.Status, c.Detail)
	}

	// Verify Doctor is strictly read-only (drifted file was NOT overwritten).
	afterBytes, _ := os.ReadFile(driftedRule)
	if string(afterBytes) == string(originalBytes) {
		t.Fatalf("Doctor mutated drifted file!")
	}

	// 3. Loopback 9router-hybrid profile is OK even without API key env var; missing agy & missing managed file are fail.
	profCfg, err := config.LoadProfiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	profCfg.ActiveProfile = "9router-hybrid"
	if err := config.SaveProfiles(ws, profCfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ws, ".agents", "agents", "odd-orchestrator.md")); err != nil {
		t.Fatal(err)
	}
	failReport, err := Doctor(ws, DoctorOptions{
		HomeDir:   fakeHome,
		LookPath:  fakeLookPath(map[string]string{}),
		LookupEnv: fakeLookupEnv(nil),
	})
	if err != nil {
		t.Fatalf("Doctor fail report: %v", err)
	}
	findCheckFail := func(name string) Check {
		for _, c := range failReport.Checks {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("missing check %q", name)
		return Check{}
	}
	if c := findCheckFail("agy-cli"); c.Status != CheckFail {
		t.Errorf("agy-cli status = %q, want fail", c.Status)
	}
	if c := findCheckFail("managed-files"); c.Status != CheckFail {
		t.Errorf("managed-files status = %q, want fail", c.Status)
	}
	if c := findCheckFail("provider-readiness"); c.Status != CheckOK {
		t.Errorf("provider-readiness for loopback 9router-hybrid = %q (%s), want ok", c.Status, c.Detail)
	}
}

func TestUninstallRemovesManagedAndPreservesUserFilesAndEdits(t *testing.T) {
	ws := t.TempDir()
	agentsDir := filepath.Join(ws, ".agents")
	if err := os.MkdirAll(filepath.Join(agentsDir, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Unrelated user file inside .agents/rules/
	userRulePath := filepath.Join(agentsDir, "rules", "my-team-rule.md")
	if err := os.WriteFile(userRulePath, []byte("# team rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-existing user hook and user MCP server.
	writeJSONFile(t, filepath.Join(agentsDir, "hooks.json"), map[string]any{
		"user-hook": map[string]any{"Stop": []any{}},
	})
	writeJSONFile(t, filepath.Join(agentsDir, "mcp_config.json"), map[string]any{
		"mcpServers": map[string]any{
			"user-mcp": map[string]any{"command": "node", "args": []string{"server.js"}},
		},
	})

	if _, err := Init(ws, InitOptions{
		LookPath: fakeLookPath(map[string]string{
			"agy":    "/bin/agy",
			"engram": "/bin/engram",
		}),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// User modifies one managed rule; Uninstall must preserve it.
	editedManagedRel := ".agents/rules/odd-03-tracking.md"
	editedManagedPath := filepath.Join(ws, filepath.FromSlash(editedManagedRel))
	if err := os.WriteFile(editedManagedPath, []byte("# user customized\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	unRes, err := Uninstall(ws)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if !slices.Contains(unRes.Preserved, editedManagedRel) {
		t.Errorf("expected %s in Preserved, got %v", editedManagedRel, unRes.Preserved)
	}
	if _, err := os.Stat(editedManagedPath); err != nil {
		t.Errorf("user-edited managed file was deleted: %v", err)
	}
	if _, err := os.Stat(userRulePath); err != nil {
		t.Errorf("unrelated user rule was deleted: %v", err)
	}

	// Unmodified managed file must be gone.
	if _, err := os.Stat(filepath.Join(agentsDir, "rules", "odd-01-core.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unmodified managed rule still exists: %v", err)
	}
	// Manifest must be gone.
	if _, err := os.Stat(ManifestPath(ws)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("manifest still exists: %v", err)
	}

	// hooks.json and mcp_config.json keep user entries and drop managed entries.
	hooksDoc := readJSONFile(t, filepath.Join(agentsDir, "hooks.json"))
	if _, ok := hooksDoc["odd-agy"]; ok {
		t.Errorf("odd-agy still in hooks.json")
	}
	if _, ok := hooksDoc["user-hook"]; !ok {
		t.Errorf("user-hook lost from hooks.json")
	}

	mcpDoc := readJSONFile(t, filepath.Join(agentsDir, "mcp_config.json"))
	servers := mcpDoc["mcpServers"].(map[string]any)
	if _, ok := servers["odd-agy"]; ok {
		t.Errorf("odd-agy still in mcp_config.json")
	}
	if _, ok := servers["engram"]; ok {
		t.Errorf("installer-added engram still in mcp_config.json")
	}
	if _, ok := servers["user-mcp"]; !ok {
		t.Errorf("user-mcp lost from mcp_config.json")
	}

	// In a fresh workspace with NO user hooks/MCP, Uninstall removes hooks.json and mcp_config.json completely.
	wsClean := t.TempDir()
	if _, err := Init(wsClean, InitOptions{
		LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
	}); err != nil {
		t.Fatalf("Init clean: %v", err)
	}
	if _, err := Uninstall(wsClean); err != nil {
		t.Fatalf("Uninstall clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wsClean, ".agents", "hooks.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected empty hooks.json to be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(wsClean, ".agents", "mcp_config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected empty mcp_config.json to be removed, got err=%v", err)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

func TestInstallerRejectsSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	outsideFile, outsideDir := filepath.Join(outside, "secret.txt"), filepath.Join(outside, "rules-target")
	const sentinel = "outside-sentinel\n"
	if err := os.WriteFile(outsideFile, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ target, linkRel string }{
		{outsideDir, filepath.Join(".agents", "rules")},
		{outsideFile, filepath.Join(".agents", "hooks.json")},
	} {
		ws := t.TempDir()
		_ = os.MkdirAll(filepath.Join(ws, ".agents"), 0o755)
		if err := os.Symlink(tc.target, filepath.Join(ws, tc.linkRel)); err != nil {
			t.Fatal(err)
		}
		if _, err := Init(ws, InitOptions{}); err == nil {
			t.Fatalf("expected Init to reject symlink %s", tc.linkRel)
		}
	}
	if entries, _ := os.ReadDir(outsideDir); len(entries) != 0 {
		t.Fatalf("Init wrote into external symlink target: %v", entries)
	}

	ws := t.TempDir()
	if _, err := Init(ws, InitOptions{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rulePath := filepath.Join(ws, ".agents", "rules", "odd-01-core.md")
	_ = os.Remove(rulePath)
	if err := os.Symlink(outsideFile, rulePath); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(ws, SyncOptions{Force: true}); err == nil {
		t.Fatalf("expected Sync(Force: true) to reject file symlink")
	}
	if _, err := os.Stat(filepath.Join(ws, ".agents", "odd", ".backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Sync created backup despite symlink rejection: err=%v", err)
	}
	if _, err := Uninstall(ws); err == nil {
		t.Fatalf("expected Uninstall to reject file symlink")
	}
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != sentinel {
		t.Fatalf("external symlink target altered or deleted: got=%q err=%v", string(got), err)
	}
}

func TestInitExcludeEngramRemovesPreviouslyManagedEntry(t *testing.T) {
	ws := t.TempDir()
	if r1, err := Init(ws, InitOptions{IncludeEngramMCP: true}); err != nil || !r1.AddedEngramMCP {
		t.Fatalf("first Init failed or did not add engram: res=%+v err=%v", r1, err)
	}
	if r2, err := Init(ws, InitOptions{ExcludeEngramMCP: true}); err != nil || r2.AddedEngramMCP {
		t.Fatalf("second Init failed or kept engram: res=%+v err=%v", r2, err)
	}
	servers := readJSONFile(t, MCPConfigPath(ws))["mcpServers"].(map[string]any)
	if _, ok := servers[EngramMCPKey]; ok {
		t.Errorf("expected managed engram removed from mcp_config.json")
	}
	if _, ok := servers[ManagedMCPKey]; !ok {
		t.Errorf("expected %s retained in mcp_config.json", ManagedMCPKey)
	}
	if m, err := LoadManifest(ws); err != nil || slices.Contains(m.ManagedMCPKeys, EngramMCPKey) {
		t.Errorf("expected engram removed from ManagedMCPKeys, err=%v keys=%v", err, m.ManagedMCPKeys)
	}

	wsUser := t.TempDir()
	_ = os.MkdirAll(filepath.Join(wsUser, ".agents"), 0o755)
	writeJSONFile(t, MCPConfigPath(wsUser), map[string]any{
		"mcpServers": map[string]any{EngramMCPKey: map[string]any{"command": "/custom/engram"}},
	})
	for _, opts := range []InitOptions{{IncludeEngramMCP: true}, {ExcludeEngramMCP: true}} {
		if _, err := Init(wsUser, opts); err != nil {
			t.Fatalf("user Init %+v: %v", opts, err)
		}
	}
	userServers := readJSONFile(t, MCPConfigPath(wsUser))["mcpServers"].(map[string]any)
	if entry, ok := userServers[EngramMCPKey].(map[string]any); !ok || entry["command"] != "/custom/engram" {
		t.Errorf("user-owned engram entry not preserved: %#v", userServers[EngramMCPKey])
	}
}

func TestInitSyncUninstallManageSessionSettingsWithoutTouchingHome(t *testing.T) {
	ws := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	if _, err := Init(ws, InitOptions{
		LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy"}),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Never touches ~/.gemini/
	if _, err := os.Stat(filepath.Join(fakeHome, ".gemini")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ~/.gemini not to exist after Init, err=%v", err)
	}

	sessPath := SessionSettingsPath(ws)
	doc := readJSONFile(t, sessPath)
	perms, ok := doc["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions missing or wrong type in %s: %#v", sessPath, doc)
	}
	allow, _ := perms["allow"].([]any)
	ask, _ := perms["ask"].([]any)
	if len(allow) != 1 || allow[0] != "mcp(odd-agy/*)" {
		t.Fatalf("permissions.allow = %#v, want [mcp(odd-agy/*)]", allow)
	}
	if len(ask) != 1 || ask[0] != "mcp(odd-agy/odd_review_consent)" {
		t.Fatalf("permissions.ask = %#v, want [mcp(odd-agy/odd_review_consent)]", ask)
	}

	manifest, err := LoadManifest(ws)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	rawSess, err := os.ReadFile(sessPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Files[SessionSettingsRelPath] != sha256Prefixed(rawSess) {
		t.Fatalf("manifest hash mismatch for %s", SessionSettingsRelPath)
	}

	// Delete session settings -> Sync recreates it.
	if err := os.Remove(sessPath); err != nil {
		t.Fatal(err)
	}
	syncRes, err := Sync(ws, SyncOptions{})
	if err != nil {
		t.Fatalf("Sync recreate: %v", err)
	}
	if !slices.Contains(syncRes.Created, SessionSettingsRelPath) {
		t.Fatalf("expected %s in Sync Created, got %v", SessionSettingsRelPath, syncRes.Created)
	}
	if _, err := os.Stat(sessPath); err != nil {
		t.Fatalf("session settings not recreated by Sync: %v", err)
	}

	// User-edited session settings causes Sync conflict without Force, and backs up + restores with Force.
	if err := os.WriteFile(sessPath, []byte(`{"permissions":{"allow":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	refusedRes, err := Sync(ws, SyncOptions{Force: false})
	if err == nil || !slices.Contains(refusedRes.Conflicts, SessionSettingsRelPath) {
		t.Fatalf("expected conflict for edited %s, got err=%v conflicts=%v", SessionSettingsRelPath, err, refusedRes.Conflicts)
	}
	forcedRes, err := Sync(ws, SyncOptions{Force: true})
	if err != nil {
		t.Fatalf("forced Sync failed: %v", err)
	}
	if len(forcedRes.BackedUp) != 1 {
		t.Fatalf("expected 1 backup on forced Sync, got %v", forcedRes.BackedUp)
	}

	// Uninstall removes unmodified session settings.
	unRes, err := Uninstall(ws)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !slices.Contains(unRes.Removed, SessionSettingsRelPath) {
		t.Fatalf("expected %s in Uninstall Removed, got %v", SessionSettingsRelPath, unRes.Removed)
	}
	if _, err := os.Stat(sessPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s removed after Uninstall, err=%v", sessPath, err)
	}
}

func TestGlobalMCPPermissionsGrantCheckAndRevoke(t *testing.T) {
	fakeHome := t.TempDir()
	settingsPath := GlobalSettingsPath(fakeHome)

	// Missing file -> Check false, Revoke no-op.
	enabled, err := CheckGlobalMCPPermissions(fakeHome)
	if err != nil || enabled {
		t.Fatalf("initial CheckGlobalMCPPermissions = (%t, %v), want (false, nil)", enabled, err)
	}
	rev0, err := RevokeGlobalMCPPermissions(fakeHome)
	if err != nil || rev0.Changed || rev0.Enabled {
		t.Fatalf("Revoke on missing = %+v, err=%v", rev0, err)
	}

	// Seed existing settings.json with custom keys and rules.
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := map[string]any{
		"theme": "dark",
		"nested": map[string]any{
			"keep": true,
		},
		"permissions": map[string]any{
			"allow": []string{"Bash(ls)"},
			"ask":   []string{"Bash(git push)"},
			"deny":  []string{"Bash(rm -rf *)"},
		},
	}
	writeJSONFile(t, settingsPath, original)
	origBytes, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	// Grant creates backup, preserves existing keys/rules, sets 0600.
	grant1, err := GrantGlobalMCPPermissions(fakeHome)
	if err != nil {
		t.Fatalf("GrantGlobalMCPPermissions: %v", err)
	}
	if !grant1.Changed || !grant1.Enabled || grant1.BackupPath == "" {
		t.Fatalf("unexpected grant1 result: %+v", grant1)
	}
	if !strings.HasPrefix(grant1.BackupPath, settingsPath+".odd-agy.bak.") {
		t.Fatalf("unexpected backup path: %q", grant1.BackupPath)
	}
	bakBytes, err := os.ReadFile(grant1.BackupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(bakBytes) != string(origBytes) {
		t.Fatalf("backup mismatch:\ngot:\n%s\nwant:\n%s", bakBytes, origBytes)
	}

	info, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("settings.json perm = %o, want 0600", info.Mode().Perm())
	}

	doc := readJSONFile(t, settingsPath)
	if doc["theme"] != "dark" {
		t.Errorf("theme lost: %#v", doc["theme"])
	}
	if nested, ok := doc["nested"].(map[string]any); !ok || nested["keep"] != true {
		t.Errorf("nested lost: %#v", doc["nested"])
	}
	perms := doc["permissions"].(map[string]any)
	if gotAllow := perms["allow"].([]any); len(gotAllow) != 2 || gotAllow[0] != "Bash(ls)" || gotAllow[1] != "mcp(odd-agy/*)" {
		t.Errorf("allow = %#v", gotAllow)
	}
	if gotAsk := perms["ask"].([]any); len(gotAsk) != 2 || gotAsk[0] != "Bash(git push)" || gotAsk[1] != "mcp(odd-agy/odd_review_consent)" {
		t.Errorf("ask = %#v", gotAsk)
	}
	if gotDeny := perms["deny"].([]any); len(gotDeny) != 1 || gotDeny[0] != "Bash(rm -rf *)" {
		t.Errorf("deny = %#v", gotDeny)
	}

	enabled, err = CheckGlobalMCPPermissions(fakeHome)
	if err != nil || !enabled {
		t.Fatalf("CheckGlobalMCPPermissions after grant = (%t, %v)", enabled, err)
	}

	// Idempotent second grant does not duplicate rules.
	grant2, err := GrantGlobalMCPPermissions(fakeHome)
	if err != nil {
		t.Fatalf("second GrantGlobalMCPPermissions: %v", err)
	}
	if grant2.Changed || !grant2.Enabled {
		t.Fatalf("expected idempotent grant2 Changed=false Enabled=true, got %+v", grant2)
	}
	doc2 := readJSONFile(t, settingsPath)
	perms2 := doc2["permissions"].(map[string]any)
	if len(perms2["allow"].([]any)) != 2 || len(perms2["ask"].([]any)) != 2 {
		t.Fatalf("duplicate rules added on second grant: %#v", perms2)
	}

	// Revoke removes only the two odd-agy rules cleanly.
	rev1, err := RevokeGlobalMCPPermissions(fakeHome)
	if err != nil {
		t.Fatalf("RevokeGlobalMCPPermissions: %v", err)
	}
	if !rev1.Changed || rev1.Enabled {
		t.Fatalf("unexpected rev1 result: %+v", rev1)
	}

	doc3 := readJSONFile(t, settingsPath)
	if doc3["theme"] != "dark" {
		t.Errorf("theme lost after revoke")
	}
	perms3 := doc3["permissions"].(map[string]any)
	if gotAllow := perms3["allow"].([]any); len(gotAllow) != 1 || gotAllow[0] != "Bash(ls)" {
		t.Errorf("allow after revoke = %#v", gotAllow)
	}
	if gotAsk := perms3["ask"].([]any); len(gotAsk) != 1 || gotAsk[0] != "Bash(git push)" {
		t.Errorf("ask after revoke = %#v", gotAsk)
	}
	if gotDeny := perms3["deny"].([]any); len(gotDeny) != 1 || gotDeny[0] != "Bash(rm -rf *)" {
		t.Errorf("deny after revoke = %#v", gotDeny)
	}

	enabled, err = CheckGlobalMCPPermissions(fakeHome)
	if err != nil || enabled {
		t.Fatalf("CheckGlobalMCPPermissions after revoke = (%t, %v)", enabled, err)
	}

	// Idempotent second revoke.
	rev2, err := RevokeGlobalMCPPermissions(fakeHome)
	if err != nil || rev2.Changed || rev2.Enabled {
		t.Fatalf("second RevokeGlobalMCPPermissions = %+v, err=%v", rev2, err)
	}

	// Fresh home without pre-existing settings.json -> Grant works without backup, Revoke removes cleanly.
	freshHome := t.TempDir()
	grantFresh, err := GrantGlobalMCPPermissions(freshHome)
	if err != nil {
		t.Fatalf("Grant on fresh home: %v", err)
	}
	if !grantFresh.Changed || !grantFresh.Enabled || grantFresh.BackupPath != "" {
		t.Fatalf("unexpected fresh grant: %+v", grantFresh)
	}
	revFresh, err := RevokeGlobalMCPPermissions(freshHome)
	if err != nil || !revFresh.Changed || revFresh.Enabled {
		t.Fatalf("unexpected fresh revoke: %+v, err=%v", revFresh, err)
	}
	freshDoc := readJSONFile(t, GlobalSettingsPath(freshHome))
	if _, hasPerms := freshDoc["permissions"]; hasPerms {
		t.Fatalf("expected empty permissions key pruned on fresh revoke, got %#v", freshDoc)
	}
}

func TestGlobalMCPPermissionsInvalidSettingsRemainByteIdentical(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "malformed_json",
			raw:  `{"theme": "dark", "permissions": `,
		},
		{
			name: "non_object_permissions",
			raw:  "{\n  \"theme\": \"dark\",\n  \"permissions\": \"invalid\"\n}\n",
		},
		{
			name: "non_array_permissions_allow",
			raw:  "{\n  \"theme\": \"dark\",\n  \"permissions\": {\n    \"allow\": {\"mcp\": true}\n  }\n}\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeHome := t.TempDir()
			settingsPath := GlobalSettingsPath(fakeHome)
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(settingsPath, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := GrantGlobalMCPPermissions(fakeHome); err == nil {
				t.Fatalf("expected GrantGlobalMCPPermissions to fail on %s", tc.name)
			}
			afterGrant, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(afterGrant) != tc.raw {
				t.Fatalf("Grant modified invalid settings.json:\ngot:\n%s\nwant:\n%s", afterGrant, tc.raw)
			}

			if _, err := RevokeGlobalMCPPermissions(fakeHome); err == nil {
				t.Fatalf("expected RevokeGlobalMCPPermissions to fail on %s", tc.name)
			}
			afterRevoke, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(afterRevoke) != tc.raw {
				t.Fatalf("Revoke modified invalid settings.json:\ngot:\n%s\nwant:\n%s", afterRevoke, tc.raw)
			}

			if _, err := CheckGlobalMCPPermissions(fakeHome); err == nil {
				t.Fatalf("expected CheckGlobalMCPPermissions to fail on %s", tc.name)
			}
			afterCheck, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(afterCheck) != tc.raw {
				t.Fatalf("Check modified invalid settings.json:\ngot:\n%s\nwant:\n%s", afterCheck, tc.raw)
			}

			// Also verify no backup files were created on failed Grant.
			matches, err := filepath.Glob(settingsPath + ".odd-agy.bak.*")
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("expected no backup files on failed Grant, got %v", matches)
			}
		})
	}
}

func TestDoctorFailsWhenSessionSettingsMissingFromManifest(t *testing.T) {
	ws := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	if _, err := Init(ws, InitOptions{
		LookPath: fakeLookPath(map[string]string{
			"agy":    "/bin/agy",
			"engram": "/bin/engram",
		}),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	manifest, err := LoadManifest(ws)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if _, ok := manifest.Files[SessionSettingsRelPath]; !ok {
		t.Fatalf("expected %s in manifest before deletion", SessionSettingsRelPath)
	}
	delete(manifest.Files, SessionSettingsRelPath)
	if err := saveManifest(ws, manifest); err != nil {
		t.Fatalf("saveManifest: %v", err)
	}

	report, err := Doctor(ws, DoctorOptions{
		HomeDir: fakeHome,
		LookPath: fakeLookPath(map[string]string{
			"agy":    "/bin/agy",
			"engram": "/bin/engram",
		}),
		LookupEnv: fakeLookupEnv(nil),
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !report.HasFailures() {
		t.Fatalf("expected Doctor report to fail when %s missing from manifest, got %+v", SessionSettingsRelPath, report.Checks)
	}

	var managed Check
	var found bool
	for _, c := range report.Checks {
		if c.Name == "managed-files" {
			managed = c
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing managed-files check in report: %+v", report.Checks)
	}
	if managed.Status != CheckFail {
		t.Fatalf("managed-files status = %q, want %q (detail=%q)", managed.Status, CheckFail, managed.Detail)
	}
	if !strings.Contains(managed.Detail, SessionSettingsRelPath) {
		t.Fatalf("managed-files detail = %q, want mention of %q", managed.Detail, SessionSettingsRelPath)
	}
}

func TestInitAndSyncRenderAgyNativeModelsIntoAgentFrontmatter(t *testing.T) {
	ws := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Seed custom profiles.json before Init (simulates preserving existing profiles.json on Init without Force).
	profs := config.DefaultProfilesConfig()
	profs.ActiveProfile = "deepseek-hybrid"
	ds := profs.Profiles["deepseek-hybrid"]
	ds.Roles[config.RoleOrchestrator] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-2.5-pro"}
	ds.Roles["explore"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-2.5-flash"}
	ds.Roles["worker"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "claude-sonnet-4.5"}
	// verify remains external-mcp (deepseek-v4-pro) -> embedded frontmatter must keep default "pro".
	profs.Profiles["deepseek-hybrid"] = ds
	if err := config.SaveProfiles(ws, profs); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	if _, err := Init(ws, InitOptions{
		AllowGlobalMCP: true,
		HomeDir:        fakeHome,
		LookPath:       fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
	}); err != nil {
		t.Fatalf("Init with preserved profiles.json: %v", err)
	}

	assertFrontmatterModel := func(relSlash, wantModel string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(relSlash)))
		if err != nil {
			t.Fatalf("read %s: %v", relSlash, err)
		}
		fm, _, err := assets.ParseFrontmatter(data)
		if err != nil {
			t.Fatalf("ParseFrontmatter(%s): %v", relSlash, err)
		}
		if fm.Model != wantModel {
			t.Fatalf("%s frontmatter model = %q, want %q", relSlash, fm.Model, wantModel)
		}
	}

	assertFrontmatterModel(".agents/agents/odd-orchestrator.md", "inherit")
	assertFrontmatterModel(".agents/agents/gentle-ai-explore.md", "flash")
	assertFrontmatterModel(".agents/agents/gentle-ai-worker.md", "pro")
	assertFrontmatterModel(".agents/agents/gentle-ai-verify.md", "pro")

	// Doctor must report managed-files OK (manifest hashes match rendered agent files).
	docRep, err := Doctor(ws, DoctorOptions{
		HomeDir:   fakeHome,
		LookPath:  fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
		LookupEnv: fakeLookupEnv(map[string]string{"DEEPSEEK_API_KEY": "sk-test"}),
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if docRep.HasFailures() || docRep.HasWarnings() {
		t.Fatalf("expected healthy doctor report, got %+v", docRep.Checks)
	}

	// Update profiles.json (switch worker from pro-normalized claude-sonnet-4.5 to flash-normalized gemini-3.8-flash-low)
	// and run Sync without Force: must NOT report conflicts, must update agent frontmatter to flash.
	profs.Profiles["deepseek-hybrid"].Roles["worker"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-low"}
	if err := config.SaveProfiles(ws, profs); err != nil {
		t.Fatalf("SaveProfiles update: %v", err)
	}

	syncRes, err := Sync(ws, SyncOptions{Force: false})
	if err != nil {
		t.Fatalf("Sync after profile model change failed: %v", err)
	}
	if len(syncRes.Conflicts) != 0 {
		t.Fatalf("unexpected Sync conflicts: %v", syncRes.Conflicts)
	}
	if !slices.Contains(syncRes.Updated, ".agents/agents/gentle-ai-worker.md") {
		t.Fatalf("expected gentle-ai-worker.md in Sync Updated, got %v", syncRes.Updated)
	}
	assertFrontmatterModel(".agents/agents/gentle-ai-worker.md", "flash")

	// Immediate second Sync must report all unchanged.
	syncRes2, err := Sync(ws, SyncOptions{Force: false})
	if err != nil {
		t.Fatalf("second Sync failed: %v", err)
	}
	if len(syncRes2.Updated) != 0 || len(syncRes2.Created) != 0 || len(syncRes2.Conflicts) != 0 {
		t.Fatalf("expected no-op second Sync, got %+v", syncRes2)
	}
}

func TestApplyActiveProfileModels(t *testing.T) {
	// 1. Uninitialized workspace returns (0, nil) without error.
	uninit := t.TempDir()
	n, err := ApplyActiveProfileModels(uninit)
	if err != nil || n != 0 {
		t.Fatalf("ApplyActiveProfileModels(uninit) = (%d, %v), want (0, nil)", n, err)
	}

	// 2. Initialized workspace updates agy-native agent files & manifest cleanly.
	ws := t.TempDir()
	if _, err := Init(ws, InitOptions{
		LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy"}),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	profs, err := config.LoadProfiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	profs.ActiveProfile = "deepseek-hybrid"
	ds := profs.Profiles["deepseek-hybrid"]
	ds.Roles[config.RoleOrchestrator] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-medium"}
	ds.Roles["explore"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "claude-sonnet-4-6"}
	ds.Roles["worker"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-low"}
	profs.Profiles["deepseek-hybrid"] = ds
	if err := config.SaveProfiles(ws, profs); err != nil {
		t.Fatal(err)
	}

	updated, err := ApplyActiveProfileModels(ws)
	if err != nil {
		t.Fatalf("ApplyActiveProfileModels: %v", err)
	}
	if updated != 2 {
		t.Fatalf("updated = %d, want 2 (explore->pro, worker->flash; orchestrator stays inherit)", updated)
	}

	// Idempotent second call returns (0, nil).
	updated2, err := ApplyActiveProfileModels(ws)
	if err != nil || updated2 != 0 {
		t.Fatalf("second ApplyActiveProfileModels = (%d, %v), want (0, nil)", updated2, err)
	}

	// Verify normalized frontmatter values on disk and Doctor OK.
	assertFrontmatter := func(relSlash, want string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(relSlash)))
		if err != nil {
			t.Fatalf("read %s: %v", relSlash, err)
		}
		fm, _, err := assets.ParseFrontmatter(b)
		if err != nil {
			t.Fatalf("ParseFrontmatter(%s): %v", relSlash, err)
		}
		if fm.Model != want {
			t.Fatalf("%s frontmatter model = %q, want %q", relSlash, fm.Model, want)
		}
	}
	assertFrontmatter(".agents/agents/odd-orchestrator.md", "inherit")
	assertFrontmatter(".agents/agents/gentle-ai-explore.md", "pro")
	assertFrontmatter(".agents/agents/gentle-ai-worker.md", "flash")

	// Manifest hashes updated so Sync reports zero changes and zero conflicts.
	syncRes, err := Sync(ws, SyncOptions{})
	if err != nil || len(syncRes.Updated) != 0 || len(syncRes.Conflicts) != 0 {
		t.Fatalf("Sync after ApplyActiveProfileModels: res=%+v err=%v", syncRes, err)
	}

	// 3. User-modified agent body is preserved and its manifest hash is NOT overwritten.
	workerRel := ".agents/agents/gentle-ai-worker.md"
	workerPath := filepath.Join(ws, filepath.FromSlash(workerRel))
	workerData, err := os.ReadFile(workerPath)
	if err != nil {
		t.Fatal(err)
	}
	// Also put a "model: keep-in-body" line in body to prove only frontmatter is rewritten.
	customBodyNote := "\n\nmodel: body-literal-must-stay\n"
	if err := os.WriteFile(workerPath, append(workerData, []byte(customBodyNote)...), 0o644); err != nil {
		t.Fatal(err)
	}

	profs.Profiles["deepseek-hybrid"].Roles["worker"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "claude-sonnet-4-6"}
	if err := config.SaveProfiles(ws, profs); err != nil {
		t.Fatal(err)
	}

	updated3, err := ApplyActiveProfileModels(ws)
	if err != nil || updated3 != 1 {
		t.Fatalf("ApplyActiveProfileModels on user-edited agent = (%d, %v), want (1, nil)", updated3, err)
	}
	afterWorker, err := os.ReadFile(workerPath)
	if err != nil {
		t.Fatal(err)
	}
	fm, body, err := assets.ParseFrontmatter(afterWorker)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Model != "pro" {
		t.Fatalf("worker frontmatter model = %q, want pro", fm.Model)
	}
	if !strings.Contains(body, "model: body-literal-must-stay") {
		t.Fatalf("user body edit or body 'model:' line was clobbered:\n%s", body)
	}
	// Sync without Force must STILL detect workerRel as a conflict (manifest hash was NOT updated for dirty file).
	refused, syncErr := Sync(ws, SyncOptions{Force: false})
	if syncErr == nil || !slices.Contains(refused.Conflicts, workerRel) {
		t.Fatalf("expected Sync conflict on user-modified %s, got err=%v conflicts=%v", workerRel, syncErr, refused.Conflicts)
	}

	// 4. Missing agent file on disk is skipped cleanly.
	explorePath := filepath.Join(ws, ".agents", "agents", "gentle-ai-explore.md")
	if err := os.Remove(explorePath); err != nil {
		t.Fatal(err)
	}
	profs.Profiles["deepseek-hybrid"].Roles["explore"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "flash"}
	if err := config.SaveProfiles(ws, profs); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyActiveProfileModels(ws); err != nil {
		t.Fatalf("ApplyActiveProfileModels with missing file failed: %v", err)
	}

	// 5. Symlink escape is rejected before mutating external target.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.md")
	const sentinel = "---\nname: outside\nmodel: original\n---\n\nbody\n"
	if err := os.WriteFile(outsideFile, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, explorePath); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyActiveProfileModels(ws); err == nil {
		t.Fatalf("expected ApplyActiveProfileModels to reject symlink escape")
	}
	if got, _ := os.ReadFile(outsideFile); string(got) != sentinel {
		t.Fatalf("external symlink target was mutated: %q", string(got))
	}
}

func TestSyncPreservesEmbeddedDefaultsWhenProfilesMissing(t *testing.T) {
	ws := t.TempDir()
	if _, err := Init(ws, InitOptions{
		LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy"}),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.Remove(config.ProfilesPath(ws)); err != nil {
		t.Fatal(err)
	}
	res, err := Sync(ws, SyncOptions{})
	if err != nil {
		t.Fatalf("Sync without profiles.json failed: %v", err)
	}
	if len(res.Conflicts) != 0 || len(res.Updated) != 0 || len(res.Created) != 0 {
		t.Fatalf("expected clean no-op Sync without profiles.json, got %+v", res)
	}
}

func TestApplyActiveProfileModelsDirSymlinkAndCRLFPreservation(t *testing.T) {
	t.Run("dir_symlink_escape_rejected_with_zero_writes", func(t *testing.T) {
		ws := t.TempDir()
		if _, err := Init(ws, InitOptions{
			LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy"}),
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}

		outsideDir := t.TempDir()
		outsideOrch := filepath.Join(outsideDir, "odd-orchestrator.md")
		const sentinel = "---\nname: odd-orchestrator\nmodel: original\n---\n\nbody\n"
		if err := os.WriteFile(outsideOrch, []byte(sentinel), 0o644); err != nil {
			t.Fatal(err)
		}

		agentsDir := filepath.Join(ws, ".agents", "agents")
		if err := os.RemoveAll(agentsDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideDir, agentsDir); err != nil {
			t.Fatal(err)
		}

		profs, err := config.LoadProfiles(ws)
		if err != nil {
			t.Fatal(err)
		}
		p := profs.Profiles[profs.ActiveProfile]
		p.Roles["worker"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-low"}
		profs.Profiles[profs.ActiveProfile] = p

		if _, err := ApplyProfileModels(ws, profs); err == nil {
			t.Fatalf("expected ApplyProfileModels to reject symlinked .agents/agents directory")
		}
		if got, err := os.ReadFile(outsideOrch); err != nil || string(got) != sentinel {
			t.Fatalf("external dir symlink target mutated: got=%q err=%v", string(got), err)
		}
	})

	t.Run("two_phase_zero_partial_writes_on_later_symlink", func(t *testing.T) {
		ws := t.TempDir()
		if _, err := Init(ws, InitOptions{
			LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy"}),
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}

		orchPath := filepath.Join(ws, ".agents", "agents", "odd-orchestrator.md")
		beforeOrch, err := os.ReadFile(orchPath)
		if err != nil {
			t.Fatal(err)
		}
		workerPath := filepath.Join(ws, ".agents", "agents", "gentle-ai-worker.md")
		beforeWorker, err := os.ReadFile(workerPath)
		if err != nil {
			t.Fatal(err)
		}
		beforeManifest, err := os.ReadFile(ManifestPath(ws))
		if err != nil {
			t.Fatal(err)
		}

		outsideFile := filepath.Join(t.TempDir(), "outside.md")
		const sentinel = "---\nname: outside\nmodel: original\n---\n\nbody\n"
		if err := os.WriteFile(outsideFile, []byte(sentinel), 0o644); err != nil {
			t.Fatal(err)
		}
		explorePath := filepath.Join(ws, ".agents", "agents", "gentle-ai-explore.md")
		if err := os.Remove(explorePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideFile, explorePath); err != nil {
			t.Fatal(err)
		}

		profs, err := config.LoadProfiles(ws)
		if err != nil {
			t.Fatal(err)
		}
		p := profs.Profiles[profs.ActiveProfile]
		p.Roles["worker"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-low"}
		p.Roles["explore"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "claude-sonnet-4-6"}
		profs.Profiles[profs.ActiveProfile] = p

		if _, err := ApplyProfileModels(ws, profs); err == nil {
			t.Fatalf("expected ApplyProfileModels to fail on symlinked explore file")
		}
		if afterOrch, _ := os.ReadFile(orchPath); string(afterOrch) != string(beforeOrch) {
			t.Fatalf("odd-orchestrator.md was modified despite later symlink error")
		}
		if afterWorker, _ := os.ReadFile(workerPath); string(afterWorker) != string(beforeWorker) {
			t.Fatalf("gentle-ai-worker.md was modified despite later symlink error")
		}
		if afterManifest, _ := os.ReadFile(ManifestPath(ws)); string(afterManifest) != string(beforeManifest) {
			t.Fatalf("manifest was modified despite symlink error")
		}
		if got, _ := os.ReadFile(outsideFile); string(got) != sentinel {
			t.Fatalf("external symlink target was mutated: %q", string(got))
		}
	})

	t.Run("crlf_line_endings_preserved", func(t *testing.T) {
		ws := t.TempDir()
		if _, err := Init(ws, InitOptions{
			LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy"}),
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}

		workerRel := ".agents/agents/gentle-ai-worker.md"
		workerPath := filepath.Join(ws, filepath.FromSlash(workerRel))
		lfBytes, err := os.ReadFile(workerPath)
		if err != nil {
			t.Fatal(err)
		}
		crlfBytes := []byte(strings.ReplaceAll(string(lfBytes), "\n", "\r\n"))
		if err := os.WriteFile(workerPath, crlfBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		manifest, err := LoadManifest(ws)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Files[workerRel] = sha256Prefixed(crlfBytes)
		if err := saveManifest(ws, manifest); err != nil {
			t.Fatal(err)
		}

		profs, err := config.LoadProfiles(ws)
		if err != nil {
			t.Fatal(err)
		}
		p := profs.Profiles[profs.ActiveProfile]
		p.Roles["worker"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-low"}
		profs.Profiles[profs.ActiveProfile] = p
		if err := config.SaveProfiles(ws, profs); err != nil {
			t.Fatal(err)
		}

		updated, err := ApplyActiveProfileModels(ws)
		if err != nil {
			t.Fatalf("ApplyActiveProfileModels: %v", err)
		}
		if updated != 1 {
			t.Fatalf("updated = %d, want 1", updated)
		}

		gotBytes, err := os.ReadFile(workerPath)
		if err != nil {
			t.Fatal(err)
		}
		fm, _, err := assets.ParseFrontmatter(gotBytes)
		if err != nil {
			t.Fatalf("ParseFrontmatter after CRLF update: %v", err)
		}
		if fm.Model != "flash" {
			t.Fatalf("worker model = %q, want flash", fm.Model)
		}
		if !strings.Contains(string(gotBytes), "model: flash\r\n") {
			t.Fatalf("expected CRLF after updated model line")
		}
		// Ensure zero bare LF (every \n must be preceded by \r).
		stripped := strings.ReplaceAll(string(gotBytes), "\r\n", "")
		if strings.Contains(stripped, "\n") {
			t.Fatalf("found bare LF in CRLF-preserved file")
		}
	})
}

func TestNormalizeAntigravityAgentFrontmatterModel(t *testing.T) {
	cases := []struct {
		name  string
		role  string
		model string
		want  string
	}{
		{name: "empty_orchestrator", role: config.RoleOrchestrator, model: "", want: "inherit"},
		{name: "empty_subagent", role: "worker", model: "  ", want: "inherit"},
		{name: "inherit_case", role: "explore", model: " INHERIT ", want: "inherit"},
		{name: "flash_exact", role: "explore", model: "Flash", want: "flash"},
		{name: "pro_exact", role: "worker", model: "PRO", want: "pro"},
		{name: "orchestrator_flash_model_maps_to_inherit", role: config.RoleOrchestrator, model: "gemini-3.8-flash-medium", want: "inherit"},
		{name: "orchestrator_hydrogen_maps_to_inherit", role: config.RoleOrchestrator, model: "gemini-4-hydrogen-high", want: "inherit"},
		{name: "orchestrator_claude_maps_to_inherit", role: config.RoleOrchestrator, model: "claude-sonnet-4-6", want: "inherit"},
		{name: "explore_flash_model_maps_to_flash", role: "explore", model: "gemini-3.8-flash-low", want: "flash"},
		{name: "worker_flash_case_insensitive", role: "worker", model: "Gemini-3.8-FLASH-High", want: "flash"},
		{name: "worker_claude_maps_to_pro", role: "worker", model: "claude-sonnet-4-6", want: "pro"},
		{name: "verify_hydrogen_maps_to_pro", role: "verify", model: "gemini-4-hydrogen-high", want: "pro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeAntigravityAgentFrontmatterModel(tc.role, tc.model); got != tc.want {
				t.Fatalf("NormalizeAntigravityAgentFrontmatterModel(%q, %q) = %q, want %q", tc.role, tc.model, got, tc.want)
			}
		})
	}
}

func TestNormalizedFrontmatterPreservesDoctorOK(t *testing.T) {
	ws := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	if _, err := Init(ws, InitOptions{
		LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	profs, err := config.LoadProfiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	p := profs.Profiles[profs.ActiveProfile]
	p.Roles[config.RoleOrchestrator] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-medium", Effort: "medium"}
	p.Roles["explore"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-low"}
	p.Roles["worker"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "claude-sonnet-4-6"}
	profs.Profiles[profs.ActiveProfile] = p

	if _, err := ApplyProfileModels(ws, profs); err != nil {
		t.Fatalf("ApplyProfileModels: %v", err)
	}
	if err := config.SaveProfiles(ws, profs); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	assertModel := func(rel, want string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		fm, _, err := assets.ParseFrontmatter(b)
		if err != nil {
			t.Fatal(err)
		}
		if fm.Model != want {
			t.Fatalf("%s model = %q, want %q", rel, fm.Model, want)
		}
	}
	assertModel(".agents/agents/odd-orchestrator.md", "inherit")
	assertModel(".agents/agents/gentle-ai-explore.md", "flash")
	assertModel(".agents/agents/gentle-ai-worker.md", "pro")

	rep, err := Doctor(ws, DoctorOptions{
		HomeDir:   fakeHome,
		LookPath:  fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
		LookupEnv: fakeLookupEnv(nil),
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.HasFailures() || rep.HasWarnings() {
		t.Fatalf("expected healthy doctor report, got %+v", rep.Checks)
	}
}

func TestDoctorProviderReadinessUsesStoredSecrets(t *testing.T) {
	ws := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("DEEPSEEK_API_KEY", "")

	if _, err := Init(ws, InitOptions{
		Profile:        "deepseek-hybrid",
		AllowGlobalMCP: true,
		HomeDir:        fakeHome,
		LookPath: fakeLookPath(map[string]string{
			"agy":    "/bin/agy",
			"engram": "/bin/engram",
		}),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const storedSecret = "sk-doctor-stored-secret-999"
	if err := config.SetSecret(fakeHome, "DEEPSEEK_API_KEY", storedSecret); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	// 1. Doctor with explicit HomeDir and nil LookupEnv recognizes stored secret.
	rep1, err := Doctor(ws, DoctorOptions{
		HomeDir: fakeHome,
		LookPath: fakeLookPath(map[string]string{
			"agy":    "/bin/agy",
			"engram": "/bin/engram",
		}),
	})
	if err != nil {
		t.Fatalf("Doctor(nil LookupEnv): %v", err)
	}
	if rep1.HasFailures() || rep1.HasWarnings() {
		t.Fatalf("expected healthy report with stored secret, got %+v", rep1.Checks)
	}
	for _, c := range rep1.Checks {
		if strings.Contains(c.Detail, storedSecret) {
			t.Fatalf("Doctor leaked stored secret in %s: %s", c.Name, c.Detail)
		}
	}

	// 2. Doctor with explicit HomeDir AND non-nil LookupEnv also recognizes stored secret.
	rep2, err := Doctor(ws, DoctorOptions{
		HomeDir: fakeHome,
		LookPath: fakeLookPath(map[string]string{
			"agy":    "/bin/agy",
			"engram": "/bin/engram",
		}),
		LookupEnv: fakeLookupEnv(nil),
	})
	if err != nil {
		t.Fatalf("Doctor(fakeLookupEnv): %v", err)
	}
	if rep2.HasFailures() || rep2.HasWarnings() {
		t.Fatalf("expected healthy report with HomeDir+LookupEnv and stored secret, got %+v", rep2.Checks)
	}
	for _, c := range rep2.Checks {
		if strings.Contains(c.Detail, storedSecret) {
			t.Fatalf("Doctor leaked stored secret in %s: %s", c.Name, c.Detail)
		}
	}
}

func TestGlobalMCPServerRegisterCheckAndUnregister(t *testing.T) {
	fakeHome := t.TempDir()
	cfgPath := GlobalMCPConfigPath(fakeHome)
	wantCfgPath := filepath.Join(fakeHome, ".gemini", "config", "mcp_config.json")
	if cfgPath != wantCfgPath {
		t.Fatalf("GlobalMCPConfigPath = %q, want %q", cfgPath, wantCfgPath)
	}

	// Missing file -> not registered, unregister is a no-op.
	reg, cmd, err := GlobalMCPServerRegistered(fakeHome)
	if err != nil || reg || cmd != "" {
		t.Fatalf("initial GlobalMCPServerRegistered = (%t, %q, %v), want (false, \"\", nil)", reg, cmd, err)
	}
	un0, err := UnregisterGlobalMCPServer(fakeHome)
	if err != nil || un0.Changed || un0.Registered {
		t.Fatalf("Unregister on missing = %+v, err=%v", un0, err)
	}

	// Pre-populate mcp_config.json with top-level keys and "engram".
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := map[string]any{
		"customTop": "keep-me",
		"mcpServers": map[string]any{
			"engram": map[string]any{
				"command": "engram",
				"args":    []string{"mcp"},
			},
		},
	}
	writeJSONFile(t, cfgPath, original)
	origBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// Register creates 0600 backup, preserves engram/customTop, sets odd-agy without --cwd.
	res1, err := RegisterGlobalMCPServer(fakeHome, "/opt/bin/odd-agy")
	if err != nil {
		t.Fatalf("RegisterGlobalMCPServer: %v", err)
	}
	if !res1.Changed || !res1.Registered || res1.Command != "/opt/bin/odd-agy" || res1.BackupPath == "" || res1.ConfigPath != cfgPath {
		t.Fatalf("unexpected res1: %+v", res1)
	}
	bakInfo, err := os.Stat(res1.BackupPath)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if bakInfo.Mode().Perm() != 0o600 {
		t.Errorf("backup perm = %o, want 0600", bakInfo.Mode().Perm())
	}
	bakBytes, err := os.ReadFile(res1.BackupPath)
	if err != nil || string(bakBytes) != string(origBytes) {
		t.Fatalf("backup mismatch: err=%v got=%q want=%q", err, string(bakBytes), string(origBytes))
	}

	cfgInfo, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfgInfo.Mode().Perm() != 0o600 {
		t.Errorf("mcp_config.json perm = %o, want 0600", cfgInfo.Mode().Perm())
	}

	doc := readJSONFile(t, cfgPath)
	if doc["customTop"] != "keep-me" {
		t.Errorf("customTop lost: %#v", doc["customTop"])
	}
	servers := doc["mcpServers"].(map[string]any)
	if engram, ok := servers["engram"].(map[string]any); !ok || engram["command"] != "engram" {
		t.Errorf("engram lost or mutated: %#v", servers["engram"])
	}
	odd, ok := servers["odd-agy"].(map[string]any)
	if !ok {
		t.Fatalf("odd-agy missing from mcpServers")
	}
	if odd["command"] != "/opt/bin/odd-agy" || odd["disabled"] != false {
		t.Errorf("odd-agy entry unexpected: %#v", odd)
	}
	args, _ := odd["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Errorf("odd-agy args = %#v, want [mcp] (no --cwd)", args)
	}

	reg, cmd, err = GlobalMCPServerRegistered(fakeHome)
	if err != nil || !reg || cmd != "/opt/bin/odd-agy" {
		t.Fatalf("GlobalMCPServerRegistered = (%t, %q, %v), want (true, /opt/bin/odd-agy, nil)", reg, cmd, err)
	}

	// Idempotent second call: Changed=false, Registered=true, no backup created.
	res2, err := RegisterGlobalMCPServer(fakeHome, "/opt/bin/odd-agy")
	if err != nil {
		t.Fatalf("second RegisterGlobalMCPServer: %v", err)
	}
	if res2.Changed || !res2.Registered || res2.BackupPath != "" || res2.Command != "/opt/bin/odd-agy" {
		t.Fatalf("expected idempotent res2, got %+v", res2)
	}

	// Empty binaryPath defaults to "odd-agy".
	res3, err := RegisterGlobalMCPServer(fakeHome, "   ")
	if err != nil {
		t.Fatalf("RegisterGlobalMCPServer default: %v", err)
	}
	if !res3.Changed || !res3.Registered || res3.Command != "odd-agy" || res3.BackupPath == "" {
		t.Fatalf("unexpected default res3: %+v", res3)
	}

	// disabled: true makes GlobalMCPServerRegistered return false.
	docDisabled := readJSONFile(t, cfgPath)
	docDisabled["mcpServers"].(map[string]any)["odd-agy"].(map[string]any)["disabled"] = true
	writeJSONFile(t, cfgPath, docDisabled)
	reg, cmd, err = GlobalMCPServerRegistered(fakeHome)
	if err != nil || reg || cmd != "" {
		t.Fatalf("disabled entry GlobalMCPServerRegistered = (%t, %q, %v), want (false, \"\", nil)", reg, cmd, err)
	}

	// Unregister creates 0600 backup, removes ONLY odd-agy, preserves engram & customTop.
	un1, err := UnregisterGlobalMCPServer(fakeHome)
	if err != nil {
		t.Fatalf("UnregisterGlobalMCPServer: %v", err)
	}
	if !un1.Changed || un1.Registered || un1.BackupPath == "" {
		t.Fatalf("unexpected un1: %+v", un1)
	}
	unDoc := readJSONFile(t, cfgPath)
	if unDoc["customTop"] != "keep-me" {
		t.Errorf("customTop lost on unregister")
	}
	unServers := unDoc["mcpServers"].(map[string]any)
	if _, hasOdd := unServers["odd-agy"]; hasOdd {
		t.Errorf("odd-agy still present after unregister")
	}
	if _, hasEngram := unServers["engram"]; !hasEngram {
		t.Errorf("engram removed by unregister")
	}

	// Second unregister is a no-op.
	un2, err := UnregisterGlobalMCPServer(fakeHome)
	if err != nil || un2.Changed || un2.Registered || un2.BackupPath != "" {
		t.Fatalf("second UnregisterGlobalMCPServer = %+v, err=%v", un2, err)
	}
}

func TestGlobalMCPServerRejectsSymlinksAndInvalidJSON(t *testing.T) {
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.json")
	outsideDir := filepath.Join(outside, "outside-config")
	const sentinel = `{"mcpServers":{"keep":{"command":"x"}}}` + "\n"
	if err := os.WriteFile(outsideFile, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Symlink at ~/.gemini/config
	homeDirSym := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDirSym, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(homeDirSym, ".gemini", "config")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := GlobalMCPServerRegistered(homeDirSym); err == nil {
		t.Fatalf("expected GlobalMCPServerRegistered to reject config dir symlink")
	}
	if _, err := RegisterGlobalMCPServer(homeDirSym, "odd-agy"); err == nil {
		t.Fatalf("expected RegisterGlobalMCPServer to reject config dir symlink")
	}
	if _, err := UnregisterGlobalMCPServer(homeDirSym); err == nil {
		t.Fatalf("expected UnregisterGlobalMCPServer to reject config dir symlink")
	}
	if entries, _ := os.ReadDir(outsideDir); len(entries) != 0 {
		t.Fatalf("Register wrote through config dir symlink: %v", entries)
	}

	// 2. Symlink at ~/.gemini/config/mcp_config.json
	homeFileSym := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeFileSym, ".gemini", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, GlobalMCPConfigPath(homeFileSym)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := GlobalMCPServerRegistered(homeFileSym); err == nil {
		t.Fatalf("expected GlobalMCPServerRegistered to reject file symlink")
	}
	if _, err := RegisterGlobalMCPServer(homeFileSym, "odd-agy"); err == nil {
		t.Fatalf("expected RegisterGlobalMCPServer to reject file symlink")
	}
	if _, err := UnregisterGlobalMCPServer(homeFileSym); err == nil {
		t.Fatalf("expected UnregisterGlobalMCPServer to reject file symlink")
	}
	if got, _ := os.ReadFile(outsideFile); string(got) != sentinel {
		t.Fatalf("external symlink target altered: %q", string(got))
	}

	// 3. Invalid JSON remains untouched with zero backups.
	homeBad := t.TempDir()
	badPath := GlobalMCPConfigPath(homeBad)
	if err := os.MkdirAll(filepath.Dir(badPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const badRaw = `{"mcpServers": `
	if err := os.WriteFile(badPath, []byte(badRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := GlobalMCPServerRegistered(homeBad); err == nil {
		t.Fatalf("expected error on invalid JSON")
	}
	if _, err := RegisterGlobalMCPServer(homeBad, "odd-agy"); err == nil {
		t.Fatalf("expected error on invalid JSON")
	}
	if _, err := UnregisterGlobalMCPServer(homeBad); err == nil {
		t.Fatalf("expected error on invalid JSON")
	}
	if got, _ := os.ReadFile(badPath); string(got) != badRaw {
		t.Fatalf("invalid mcp_config.json was modified")
	}
	if matches, _ := filepath.Glob(badPath + ".odd-agy.bak.*"); len(matches) != 0 {
		t.Fatalf("unexpected backups on invalid JSON: %v", matches)
	}
}

func TestInitAllowGlobalMCPAndDoctorMCPRegistration(t *testing.T) {
	// Default Init (AllowGlobalMCP=false) -> ZERO writes to ~/.gemini/.
	wsDefault := t.TempDir()
	homeDefault := t.TempDir()
	if _, err := Init(wsDefault, InitOptions{
		HomeDir:  homeDefault,
		LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
	}); err != nil {
		t.Fatalf("Init default: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeDefault, ".gemini")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default Init wrote ~/.gemini: err=%v", err)
	}

	// Doctor on agy-oauth (native-only) + unregistered -> mcp-registration is CheckOK.
	repNative, err := Doctor(wsDefault, DoctorOptions{
		HomeDir:  homeDefault,
		LookPath: fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
	})
	if err != nil {
		t.Fatalf("Doctor native: %v", err)
	}
	findCheck := func(rep DoctorReport, name string) Check {
		for _, c := range rep.Checks {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("missing check %q in %+v", name, rep.Checks)
		return Check{}
	}
	cNative := findCheck(repNative, "mcp-registration")
	if cNative.Status != CheckOK || !strings.Contains(cNative.Detail, "project-scoped only") {
		t.Errorf("native mcp-registration = %+v, want CheckOK project-scoped only", cNative)
	}

	// Switch to deepseek-hybrid (external-mcp) while unregistered -> CheckWarn.
	profs, err := config.LoadProfiles(wsDefault)
	if err != nil {
		t.Fatal(err)
	}
	profs.ActiveProfile = "deepseek-hybrid"
	if err := config.SaveProfiles(wsDefault, profs); err != nil {
		t.Fatal(err)
	}
	repWarn, err := Doctor(wsDefault, DoctorOptions{
		HomeDir:   homeDefault,
		LookPath:  fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
		LookupEnv: fakeLookupEnv(map[string]string{"DEEPSEEK_API_KEY": "sk-test"}),
	})
	if err != nil {
		t.Fatalf("Doctor external unregistered: %v", err)
	}
	cWarn := findCheck(repWarn, "mcp-registration")
	if cWarn.Status != CheckWarn || !strings.Contains(cWarn.Detail, "external-mcp") || !strings.Contains(cWarn.Detail, "odd-agy permissions enable") {
		t.Errorf("external unregistered mcp-registration = %+v, want CheckWarn", cWarn)
	}

	// Init with AllowGlobalMCP=true registers both settings.json and mcp_config.json using BinaryCommand.
	wsAllow := t.TempDir()
	homeAllow := t.TempDir()
	resAllow, err := Init(wsAllow, InitOptions{
		Profile:        "deepseek-hybrid",
		BinaryCommand:  "/custom/bin/odd-agy",
		AllowGlobalMCP: true,
		HomeDir:        homeAllow,
		LookPath:       fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
	})
	if err != nil {
		t.Fatalf("Init AllowGlobalMCP: %v", err)
	}
	if !resAllow.GlobalMCPAllowed || !resAllow.GlobalMCPRegistered {
		t.Fatalf("unexpected AllowGlobalMCP result: %+v", resAllow)
	}
	allowed, err := CheckGlobalMCPPermissions(homeAllow)
	if err != nil || !allowed {
		t.Fatalf("CheckGlobalMCPPermissions = (%t, %v), want (true, nil)", allowed, err)
	}
	reg, cmd, err := GlobalMCPServerRegistered(homeAllow)
	if err != nil || !reg || cmd != "/custom/bin/odd-agy" {
		t.Fatalf("GlobalMCPServerRegistered = (%t, %q, %v), want (true, /custom/bin/odd-agy, nil)", reg, cmd, err)
	}

	// Doctor now reports mcp-registration CheckOK with command.
	repOK, err := Doctor(wsAllow, DoctorOptions{
		HomeDir:   homeAllow,
		LookPath:  fakeLookPath(map[string]string{"agy": "/bin/agy", "engram": "/bin/engram"}),
		LookupEnv: fakeLookupEnv(map[string]string{"DEEPSEEK_API_KEY": "sk-test"}),
	})
	if err != nil {
		t.Fatalf("Doctor registered: %v", err)
	}
	cOK := findCheck(repOK, "mcp-registration")
	if cOK.Status != CheckOK || !strings.Contains(cOK.Detail, "command=/custom/bin/odd-agy") {
		t.Errorf("registered mcp-registration = %+v, want CheckOK with command", cOK)
	}
}

func TestResolveBinaryPath(t *testing.T) {
	if got := ResolveBinaryPath("  /explicit/odd-agy  "); got != "/explicit/odd-agy" {
		t.Fatalf("ResolveBinaryPath explicit = %q, want /explicit/odd-agy", got)
	}
	// When no odd-agy is on PATH during go test, must never return *.test test binary.
	t.Setenv("PATH", t.TempDir())
	got := ResolveBinaryPath("")
	if strings.HasSuffix(got, ".test") || strings.HasSuffix(got, ".test.exe") {
		t.Fatalf("ResolveBinaryPath returned test binary %q", got)
	}
	if got != "odd-agy" {
		t.Fatalf("ResolveBinaryPath fallback = %q, want odd-agy", got)
	}
}


