package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Nathanael-Huaman/odd-agy/internal/assets"
	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/installer"
	"github.com/Nathanael-Huaman/odd-agy/internal/llm"
)

func sendKey(t *testing.T, m Model, msg tea.KeyMsg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	updated, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", next)
	}
	if cmd != nil {
		if produced := cmd(); produced != nil {
			if _, isQuit := produced.(tea.QuitMsg); !isQuit {
				next2, _ := updated.Update(produced)
				updated, ok = next2.(Model)
				if !ok {
					t.Fatalf("Update(cmd) returned %T, want tui.Model", next2)
				}
			}
		}
	}
	return updated
}

func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func sendText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m = sendKey(t, m, keyRune(r))
	}
	return m
}

func testOptions(homeDir string, env map[string]string, probeFn func(context.Context, string, config.ProviderConfig, func(string) (string, bool)) llm.ProbeResult) Options {
	return Options{
		HomeDir: homeDir,
		LookPath: func(bin string) (string, error) {
			if bin == "agy" || bin == "engram" {
				return "/usr/local/bin/" + bin, nil
			}
			return "", os.ErrNotExist
		},
		LookupEnv: func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		},
		ProbeFunc: probeFn,
	}
}

func TestMainMenuToggleDelegationAndRDD(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	m := NewModel(dir, testOptions(home, nil, nil))

	view := m.View()
	if !strings.Contains(view, "full-delegation") || !strings.Contains(view, "agy-oauth") || !strings.Contains(view, "not initialized") {
		t.Fatalf("unexpected initial overview:\n%s", view)
	}
	if !strings.Contains(view, "Roles Editor (12 roles)") {
		t.Fatalf("expected dynamic role count 'Roles Editor (12 roles)' in overview:\n%s", view)
	}

	// Item 0: Toggle Delegation Mode (full-delegation -> organic)
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	cfg, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig after delegation toggle: %v", err)
	}
	if cfg.DelegationMode != config.DelegationModeOrganic {
		t.Fatalf("DelegationMode = %q, want %q", cfg.DelegationMode, config.DelegationModeOrganic)
	}

	// Move down to Item 1: Toggle RDD (true -> false)
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	cfg, err = config.LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig after rdd toggle: %v", err)
	}
	if cfg.RDDEnabled {
		t.Fatalf("RDDEnabled = true, want false")
	}
	if !strings.Contains(m.View(), "organic") {
		t.Fatalf("view did not update to organic:\n%s", m.View())
	}
}

func TestSwitchActiveProfilePersists(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	m := NewModel(dir, testOptions(home, nil, nil))

	// Navigate to Item 2: Active Profile
	m = sendKey(t, m, keyRune('j'))
	m = sendKey(t, m, keyRune('j'))
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// Profiles are sorted: 0=9router-hybrid, 1=agy-oauth, 2=deepseek-hybrid, 3=omniroute-hybrid
	if !strings.Contains(m.View(), "9router-hybrid") {
		t.Fatalf("expected profile picker view:\n%s", m.View())
	}
	// Cursor starts at 0 (9router-hybrid) or move to 9router-hybrid
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	profCfg, err := config.LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	if profCfg.ActiveProfile != "9router-hybrid" {
		t.Fatalf("ActiveProfile = %q, want 9router-hybrid", profCfg.ActiveProfile)
	}
}

func TestRolesEditorToggleRuntimeProviderAndModel(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	m := NewModel(dir, testOptions(home, nil, nil))

	// Navigate to Item 3: Roles Editor
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if !strings.Contains(m.View(), "orchestrator") || !strings.Contains(m.View(), "explore") || !strings.Contains(m.View(), "review-refuter") {
		t.Fatalf("expected all roles in editor view:\n%s", m.View())
	}

	// Cursor is on role 0 ("orchestrator"). Toggling runtime or cycling provider must keep it on agy-native.
	for _, k := range []tea.KeyMsg{keyRune('t'), {Type: tea.KeySpace}, keyRune('p')} {
		m = sendKey(t, m, k)
		if !strings.Contains(m.View(), "orchestrator always runs on agy-native (press [m] to edit its model)") {
			t.Fatalf("expected orchestrator guard status after key %v:\n%s", k, m.View())
		}
		profCfg, err := config.LoadProfiles(dir)
		if err != nil {
			t.Fatalf("LoadProfiles: %v", err)
		}
		orch := profCfg.Profiles[profCfg.ActiveProfile].Roles[config.RoleOrchestrator]
		if orch.Runtime != config.RuntimeAgyNative || orch.Provider != "" || orch.Model != "inherit" {
			t.Fatalf("orchestrator mutated unexpectedly: %+v", orch)
		}
	}

	// Pressing 'm' or Enter on agy-native orchestrator opens the vertical Model Picker listing Antigravity models.
	m = sendKey(t, m, keyRune('m'))
	hintView := m.View()
	for _, wantID := range []string{
		"inherit",
		"flash",
		"pro",
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-medium",
		"gemini-3.8-flash-low",
		"gemini-4-hydrogen-high",
		"claude-sonnet-4-6",
		"claude-opus-4-6-thinking",
		"Custom model ID...",
	} {
		if !strings.Contains(hintView, wantID) {
			t.Fatalf("expected Antigravity model %q in model picker view:\n%s", wantID, hintView)
		}
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	// Move down to role 1 ("explore"). Pressing 'm' opens subagent tier picker with ONLY inherit, flash, pro + note.
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, keyRune('m'))
	subPickerView := m.View()
	for _, wantSub := range []string{"inherit", "flash", "pro", "external-mcp"} {
		if !strings.Contains(subPickerView, wantSub) {
			t.Fatalf("expected %q in subagent model picker view:\n%s", wantSub, subPickerView)
		}
	}
	for _, unwanted := range []string{"gemini-3.8-flash-high", "claude-sonnet-4-6", "Custom model ID..."} {
		if strings.Contains(subPickerView, unwanted) {
			t.Fatalf("did not expect %q in agy-native subagent model picker view:\n%s", unwanted, subPickerView)
		}
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	// Toggle runtime agy-native -> external-mcp on explore.
	m = sendKey(t, m, keyRune('t'))
	profCfg, err := config.LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles after runtime toggle: %v", err)
	}
	route := profCfg.Profiles[profCfg.ActiveProfile].Roles["explore"]
	if route.Runtime != config.RuntimeExternalMCP || route.Provider == "" {
		t.Fatalf("explore route after toggle = %+v, want external-mcp with provider", route)
	}
	firstProvider := route.Provider

	// Cycle provider with 'p'
	m = sendKey(t, m, keyRune('p'))
	profCfg, err = config.LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles after provider cycle: %v", err)
	}
	route = profCfg.Profiles[profCfg.ActiveProfile].Roles["explore"]
	if route.Provider == firstProvider || route.Provider == "" {
		t.Fatalf("expected provider to cycle from %q, got %+v", firstProvider, route)
	}

	// Edit model with 'm', backspace existing, enter new model; external-mcp should NOT show Antigravity hint.
	m = sendKey(t, m, keyRune('m'))
	if strings.Contains(m.View(), "gemini-3.8-flash-high") {
		t.Fatalf("did not expect agy-native model hint while editing external-mcp role:\n%s", m.View())
	}
	for i := 0; i < 40; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = sendText(t, m, "custom-model-x")
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	profCfg, err = config.LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles after model edit: %v", err)
	}
	if got := profCfg.Profiles[profCfg.ActiveProfile].Roles["explore"].Model; got != "custom-model-x" {
		t.Fatalf("explore model = %q, want custom-model-x", got)
	}
}

func TestProvidersViewRedactsCredentialsAndRunsProbe(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()

	// Seed profiles.json with a credential-bearing URL to verify redaction.
	profs := config.DefaultProfilesConfig()
	ds := profs.Providers["deepseek"]
	ds.BaseURL = "https://user:topsecret@api.deepseek.com/v1?api_key=leakme"
	profs.Providers["deepseek"] = ds
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	const rawAPIKey = "sk-never-display-this-secret"
	var probedProvider string
	probeFn := func(_ context.Context, providerName string, _ config.ProviderConfig, _ func(string) (string, bool)) llm.ProbeResult {
		probedProvider = providerName
		return llm.ProbeResult{
			Provider:    providerName,
			Reachable:   true,
			StatusCode:  200,
			LatencyMS:   12,
			RedactedURL: "https://api.deepseek.com/v1",
			KeySource:   "env",
		}
	}

	m := NewModel(dir, testOptions(home, map[string]string{
		"DEEPSEEK_API_KEY": rawAPIKey,
	}, probeFn))

	// Navigate to Item 4: Providers & Probe
	for i := 0; i < 4; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// Move to "deepseek" (sorted: 0=9router, 1=deepseek, 2=openai-compatible, 3=omniroute)
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	view := m.View()
	if strings.Contains(view, rawAPIKey) || strings.Contains(view, "topsecret") || strings.Contains(view, "leakme") {
		t.Fatalf("credentials leaked in providers view:\n%s", view)
	}
	if !strings.Contains(view, "https://api.deepseek.com/v1") {
		t.Fatalf("expected redacted URL in providers view:\n%s", view)
	}

	// Trigger live probe with 'p'
	m = sendKey(t, m, keyRune('p'))
	if probedProvider != "deepseek" {
		t.Fatalf("probedProvider = %q, want deepseek", probedProvider)
	}
	view = m.View()
	if !strings.Contains(view, "reachable") || !strings.Contains(view, "200") {
		t.Fatalf("expected probe result in view:\n%s", view)
	}
}

func TestProjectActionsInitDoctorAndUninstallConfirmation(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	m := NewModel(dir, testOptions(home, nil, nil))

	// Navigate to Item 5: Project Actions
	for i := 0; i < 5; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// Item 0: Init (.agents/)
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if _, err := os.Stat(filepath.Join(dir, ".agents", "odd", ".managed-manifest.json")); err != nil {
		t.Fatalf("expected manifest after Init: %v", err)
	}

	// Item 2: Doctor
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.View(), "agy-cli") || !strings.Contains(m.View(), "managed-files") {
		t.Fatalf("expected doctor results in view:\n%s", m.View())
	}

	// Item 3: Uninstall — first press Enter to trigger confirmation, then 'n' to cancel
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(strings.ToLower(m.View()), "confirm") {
		t.Fatalf("expected uninstall confirmation prompt:\n%s", m.View())
	}
	m = sendKey(t, m, keyRune('n'))
	if _, err := os.Stat(filepath.Join(dir, ".agents", "odd", ".managed-manifest.json")); err != nil {
		t.Fatalf("manifest should remain after declining uninstall: %v", err)
	}

	// Trigger Uninstall again and confirm with 'y'
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = sendKey(t, m, keyRune('y'))
	if _, err := os.Stat(filepath.Join(dir, ".agents", "odd", ".managed-manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("expected manifest to be removed after confirming uninstall, stat err=%v", err)
	}
}

func TestProjectActionsGlobalMCPGrantAndRevoke(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	m := NewModel(dir, testOptions(home, nil, nil))

	// Navigate to Item 5: Project Actions
	for i := 0; i < 5; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if !strings.Contains(m.View(), "Allow MCP globally (~/.gemini)") {
		t.Fatalf("expected Allow MCP globally action in Project Actions:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "settings.json") || !strings.Contains(m.View(), "config.json") || !strings.Contains(m.View(), "mcp_config.json") {
		t.Fatalf("expected settings.json, config.json, and mcp_config.json status in Project Actions:\n%s", m.View())
	}

	// Move down to Item 4: Allow MCP globally (~/.gemini)
	for i := 0; i < 4; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	enabled, err := installer.CheckGlobalMCPPermissions(home)
	if err != nil || !enabled {
		t.Fatalf("CheckGlobalMCPPermissions after TUI grant = (%t, %v)", enabled, err)
	}
	appEnabled, err := installer.CheckGlobalAppMCPPermissions(home)
	if err != nil || !appEnabled {
		t.Fatalf("CheckGlobalAppMCPPermissions after TUI grant = (%t, %v)", appEnabled, err)
	}
	reg, _, err := installer.GlobalMCPServerRegistered(home)
	if err != nil || !reg {
		t.Fatalf("GlobalMCPServerRegistered after TUI grant = (%t, %v)", reg, err)
	}
	if !strings.Contains(m.View(), "Revoke global MCP rule") {
		t.Fatalf("expected Revoke global MCP rule label after grant:\n%s", m.View())
	}

	// Toggle again -> Revoke global MCP rule & unregister across all 3 files
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	enabled, err = installer.CheckGlobalMCPPermissions(home)
	if err != nil || enabled {
		t.Fatalf("CheckGlobalMCPPermissions after TUI revoke = (%t, %v)", enabled, err)
	}
	appEnabled, err = installer.CheckGlobalAppMCPPermissions(home)
	if err != nil || appEnabled {
		t.Fatalf("CheckGlobalAppMCPPermissions after TUI revoke = (%t, %v)", appEnabled, err)
	}
	reg, _, err = installer.GlobalMCPServerRegistered(home)
	if err != nil || reg {
		t.Fatalf("GlobalMCPServerRegistered after TUI revoke = (%t, %v)", reg, err)
	}
	if !strings.Contains(m.View(), "Allow MCP globally (~/.gemini)") {
		t.Fatalf("expected Allow MCP globally label after revoke:\n%s", m.View())
	}

	// Partial state (CLI + MCP enabled, App config.json disabled) -> action stays Allow and Enter enables all 3.
	if _, err := installer.GrantGlobalMCPPermissions(home); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.RegisterGlobalMCPServer(home, "odd-agy"); err != nil {
		t.Fatal(err)
	}
	mPartial := NewModel(dir, testOptions(home, nil, nil))
	for i := 0; i < 5; i++ {
		mPartial = sendKey(t, mPartial, tea.KeyMsg{Type: tea.KeyDown})
	}
	mPartial = sendKey(t, mPartial, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(mPartial.View(), "Allow MCP globally (~/.gemini)") {
		t.Fatalf("expected Allow label when App perms still disabled:\n%s", mPartial.View())
	}
	for i := 0; i < 4; i++ {
		mPartial = sendKey(t, mPartial, tea.KeyMsg{Type: tea.KeyDown})
	}
	mPartial = sendKey(t, mPartial, tea.KeyMsg{Type: tea.KeyEnter})
	if appEnabled, err := installer.CheckGlobalAppMCPPermissions(home); err != nil || !appEnabled {
		t.Fatalf("expected TUI partial toggle to enable App permissions: (%t, %v)", appEnabled, err)
	}
	if !strings.Contains(mPartial.View(), "Revoke global MCP rule") {
		t.Fatalf("expected Revoke label after all 3 enabled:\n%s", mPartial.View())
	}
}

func TestRolesEditorAndProfileSwitchSyncAgentFrontmatterAndDoctor(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	opts := testOptions(home, nil, nil)

	if _, err := installer.Init(dir, installer.InitOptions{
		LookPath: opts.LookPath,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	m := NewModel(dir, opts)

	// Navigate to Item 3: Roles Editor
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// Edit orchestrator (role 0) model via Model Picker -> gemini-3.8-flash-high (fallback index 7: 0=inherit,1=flash,2=pro,3..6=hydrogen,7=3.8-high)
	m = sendKey(t, m, keyRune('m'))
	for i := 0; i < 7; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// Move down to explore (role 1) and edit model via Model Picker -> pro (starts at index 1 "flash", move down 1 to index 2 "pro")
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, keyRune('m'))
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.View(), ".agents/agents/*.md") || !strings.Contains(m.View(), "odd-agy launch") {
		t.Fatalf("expected agent sync + restart odd-agy launch status after explore edit:\n%s", m.View())
	}

	assertAgentModel := func(rel, want string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", rel, err)
		}
		fm, _, err := assets.ParseFrontmatter(data)
		if err != nil {
			t.Fatalf("ParseFrontmatter(%s): %v", rel, err)
		}
		if fm.Model != want {
			t.Fatalf("%s frontmatter model = %q, want %q", rel, fm.Model, want)
		}
	}

	assertAgentModel(".agents/agents/odd-orchestrator.md", "inherit")
	assertAgentModel(".agents/agents/gentle-ai-explore.md", "pro")

	rep, err := installer.Doctor(dir, installer.DoctorOptions{
		HomeDir:   home,
		LookPath:  opts.LookPath,
		LookupEnv: opts.LookupEnv,
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.HasFailures() {
		t.Fatalf("Doctor reported failures after TUI model edits: %+v", rep.Checks)
	}
	for _, c := range rep.Checks {
		if c.Name == "managed-files" && c.Status != installer.CheckOK {
			t.Fatalf("managed-files check = %+v, want ok", c)
		}
	}

	// Return to Overview, switch to deepseek-hybrid via Profiles screen, and verify frontmatter resets to deepseek-hybrid's defaults.
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	// Sorted profiles: 0=9router-hybrid, 1=agy-oauth (current), 2=deepseek-hybrid
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.View(), ".agents/agents/*.md") || !strings.Contains(m.View(), "odd-agy launch") {
		t.Fatalf("expected agent sync + restart odd-agy launch status after profile switch:\n%s", m.View())
	}

	assertAgentModel(".agents/agents/odd-orchestrator.md", "inherit")
	assertAgentModel(".agents/agents/gentle-ai-explore.md", "flash")

	rep2, err := installer.Doctor(dir, installer.DoctorOptions{
		HomeDir:   home,
		LookPath:  opts.LookPath,
		LookupEnv: opts.LookupEnv,
	})
	if err != nil || rep2.HasFailures() {
		t.Fatalf("Doctor after profile switch: err=%v checks=%+v", err, rep2.Checks)
	}
	for _, c := range rep2.Checks {
		if c.Name == "managed-files" && c.Status != installer.CheckOK {
			t.Fatalf("managed-files check after profile switch = %+v, want ok", c)
		}
	}
}

func TestSaveProfilesFailureReloadsInMemoryState(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	// Save initial profiles.json so file exists, then replace it with a non-empty directory so SaveProfiles fails.
	if err := config.SaveProfiles(dir, config.DefaultProfilesConfig()); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	m := NewModel(dir, testOptions(home, nil, nil))
	// Navigate to Roles Editor -> explore (index 1)
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})

	// Break profiles.json path on disk with a non-empty directory so atomic rename fails.
	profPath := config.ProfilesPath(dir)
	if err := os.Remove(profPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(profPath, "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Attempt to toggle explore runtime -> SaveProfiles fails -> reload restores persisted/default state (no unpersisted external-mcp in memory) and sets errMsg.
	m = sendKey(t, m, keyRune('t'))
	if m.errMsg == "" {
		t.Fatalf("expected error message when SaveProfiles fails, view:\n%s", m.View())
	}
	if got := m.profiles.Profiles[m.profiles.ActiveProfile].Roles["explore"].Runtime; got != config.RuntimeAgyNative {
		t.Fatalf("in-memory explore runtime = %q after SaveProfiles failure, want %q", got, config.RuntimeAgyNative)
	}
}

func TestPersistProfileChangeRollsBackOnApplyFailure(t *testing.T) {
	t.Run("symlink_escape_on_profile_switch", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()
		opts := testOptions(home, nil, nil)

		if _, err := installer.Init(dir, installer.InitOptions{
			LookPath: opts.LookPath,
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}

		// Configure deepseek-hybrid with custom agy-native models for both orchestrator and explore.
		profs, err := config.LoadProfiles(dir)
		if err != nil {
			t.Fatal(err)
		}
		ds := profs.Profiles["deepseek-hybrid"]
		ds.Roles[config.RoleOrchestrator] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-high"}
		ds.Roles["explore"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-low"}
		profs.Profiles["deepseek-hybrid"] = ds
		if err := config.SaveProfiles(dir, profs); err != nil {
			t.Fatal(err)
		}

		beforeProfiles, err := os.ReadFile(config.ProfilesPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		orchPath := filepath.Join(dir, ".agents", "agents", "odd-orchestrator.md")
		beforeOrch, err := os.ReadFile(orchPath)
		if err != nil {
			t.Fatal(err)
		}

		// Replace gentle-ai-explore.md with a symlink pointing outside the workspace.
		outsideDir := t.TempDir()
		outsideFile := filepath.Join(outsideDir, "outside.md")
		const sentinel = "---\nname: outside\nmodel: original\n---\n\nbody\n"
		if err := os.WriteFile(outsideFile, []byte(sentinel), 0o644); err != nil {
			t.Fatal(err)
		}
		explorePath := filepath.Join(dir, ".agents", "agents", "gentle-ai-explore.md")
		if err := os.Remove(explorePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideFile, explorePath); err != nil {
			t.Fatal(err)
		}

		m := NewModel(dir, opts)
		// Navigate to Active Profile (item 2) -> Enter -> move down to deepseek-hybrid -> Enter.
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

		if m.errMsg == "" {
			t.Fatalf("expected errMsg when ApplyProfileModels fails on symlink escape")
		}
		if m.profiles.ActiveProfile != "agy-oauth" {
			t.Fatalf("in-memory ActiveProfile = %q after failure, want agy-oauth", m.profiles.ActiveProfile)
		}
		afterProfiles, err := os.ReadFile(config.ProfilesPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if string(afterProfiles) != string(beforeProfiles) {
			t.Fatalf("profiles.json on disk mutated after ApplyProfileModels failure")
		}
		afterOrch, err := os.ReadFile(orchPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(afterOrch) != string(beforeOrch) {
			t.Fatalf("odd-orchestrator.md partially modified despite preflight failure")
		}
		if got, _ := os.ReadFile(outsideFile); string(got) != sentinel {
			t.Fatalf("external symlink target mutated: %q", string(got))
		}
	})

	t.Run("corrupted_manifest_on_role_model_edit", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()
		opts := testOptions(home, nil, nil)

		if _, err := installer.Init(dir, installer.InitOptions{
			LookPath: opts.LookPath,
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}

		beforeProfiles, err := os.ReadFile(config.ProfilesPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(installer.ManifestPath(dir), []byte("{corrupt manifest"), 0o644); err != nil {
			t.Fatal(err)
		}

		m := NewModel(dir, opts)
		// Navigate to Roles Editor (item 3) -> down to explore (index 1) -> Model Picker -> select "pro" (index 2, starting from "flash" index 1, down 1).
		for i := 0; i < 3; i++ {
			m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		}
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		m = sendKey(t, m, keyRune('m'))
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

		if m.errMsg == "" {
			t.Fatalf("expected errMsg when manifest is corrupted")
		}
		exp := m.profiles.Profiles[m.profiles.ActiveProfile].Roles["explore"]
		if exp.Model != "flash" || exp.Effort != "" {
			t.Fatalf("in-memory explore = %+v after failure, want model=flash effort=\"\"", exp)
		}
		afterProfiles, err := os.ReadFile(config.ProfilesPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if string(afterProfiles) != string(beforeProfiles) {
			t.Fatalf("profiles.json on disk mutated despite corrupted manifest")
		}
	})
}

func TestParseAgyModelsOutputAndLazyCaching(t *testing.T) {
	raw := strings.Join([]string{
		"Fetching available models...",
		"",
		"gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)",
		"bad id with spaces\tInvalid",
		"ctrl\x07char\tInvalid Ctrl",
		"gemini-3.8-flash-low\tGemini 3.8 Flash (Low)",
		"no-tab-line",
	}, "\n")
	parsed := parseAgyModelsOutput(raw)
	if len(parsed) != 2 {
		t.Fatalf("parsed %d models (%+v), want 2", len(parsed), parsed)
	}
	if parsed[0].ID != "gemini-4-hydrogen-high" || parsed[0].Label != "Gemini 4 Hydrogen (High)" {
		t.Fatalf("parsed[0] = %+v", parsed[0])
	}
	if parsed[1].ID != "gemini-3.8-flash-low" || parsed[1].Label != "Gemini 3.8 Flash (Low)" {
		t.Fatalf("parsed[1] = %+v", parsed[1])
	}

	// Fallback returns known Antigravity models when agy is unavailable.
	fallback := defaultListAgyModels(func(string) (string, error) {
		return "", os.ErrNotExist
	})
	if len(fallback) != 10 {
		t.Fatalf("fallback len = %d, want 10", len(fallback))
	}
	wantFallbackIDs := []string{
		"gemini-4-hydrogen-high",
		"gemini-4-hydrogen-xhigh",
		"gemini-4-hydrogen-medium",
		"gemini-4-hydrogen-low",
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-medium",
		"gemini-3.8-flash-low",
		"claude-sonnet-4-6",
		"claude-opus-4-6-thinking",
		"gpt-oss-120b-medium",
	}
	for i, id := range wantFallbackIDs {
		if fallback[i].ID != id {
			t.Errorf("fallback[%d].ID = %q, want %q", i, fallback[i].ID, id)
		}
	}

	// Lazy & cached once per TUI session (NewModel does not call ListAgyModels).
	dir := t.TempDir()
	home := t.TempDir()
	calls := 0
	opts := testOptions(home, nil, nil)
	opts.ListAgyModels = func() []AgyModelOption {
		calls++
		return []AgyModelOption{
			{ID: "gemini-3.8-flash-high", Label: "Gemini 3.8 Flash (High)"},
			{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6"},
		}
	}
	m := NewModel(dir, opts)
	if calls != 0 {
		t.Fatalf("NewModel called ListAgyModels %d times, want 0 (must be lazy)", calls)
	}

	// Open Roles Editor -> open Model Picker twice -> ListAgyModels called once.
	for i := 0; i < 3; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = sendKey(t, m, keyRune('m'))
	if calls != 1 {
		t.Fatalf("ListAgyModels calls after first picker open = %d, want 1", calls)
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	m = sendKey(t, m, keyRune('m'))
	if calls != 1 {
		t.Fatalf("ListAgyModels calls after second picker open = %d, want 1 (cached)", calls)
	}
}

func TestRolesEditorModelPicker(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	opts := testOptions(home, nil, nil)
	opts.ListAgyModels = func() []AgyModelOption {
		return []AgyModelOption{
			{ID: "gemini-3.8-flash-high", Label: "Gemini 3.8 Flash (High)"},
			{ID: "gemini-3.8-flash-low", Label: "Gemini 3.8 Flash (Low)"},
			{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6"},
		}
	}
	if _, err := installer.Init(dir, installer.InitOptions{LookPath: opts.LookPath}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	m := NewModel(dir, opts)
	// Navigate to Roles Editor (index 3).
	for i := 0; i < 3; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	rolesView := m.View()
	if !strings.Contains(rolesView, "default") || !strings.Contains(rolesView, "[m] model") || !strings.Contains(rolesView, "[e] effort") {
		t.Fatalf("Roles Editor missing effort column or footer hints:\n%s", rolesView)
	}

	// Pressing Enter on orchestrator opens screenModelPicker.
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	pickerView := m.View()
	for _, want := range []string{"inherit", "flash", "pro", "gemini-3.8-flash-high", "claude-sonnet-4-6", "Custom model ID..."} {
		if !strings.Contains(pickerView, want) {
			t.Fatalf("model picker missing %q:\n%s", want, pickerView)
		}
	}

	// Pressing Esc cancels without changing model or cursor role.
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if strings.Contains(m.View(), "Custom model ID...") {
		t.Fatalf("expected Esc to return from model picker to roles editor:\n%s", m.View())
	}
	profs, _ := config.LoadProfiles(dir)
	if got := profs.Profiles[profs.ActiveProfile].Roles[config.RoleOrchestrator].Model; got != "inherit" {
		t.Fatalf("orchestrator model changed after Esc: %q", got)
	}

	// Open with 'm' -> pick claude-sonnet-4-6 (index 5: 0=inherit, 1=flash, 2=pro, 3=3.8-high, 4=3.8-low, 5=claude-sonnet-4-6).
	m = sendKey(t, m, keyRune('m'))
	for i := 0; i < 5; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	profs, _ = config.LoadProfiles(dir)
	orch := profs.Profiles[profs.ActiveProfile].Roles[config.RoleOrchestrator]
	if orch.Model != "claude-sonnet-4-6" || orch.Effort != "" {
		t.Fatalf("orchestrator after picking claude-sonnet-4-6 = %+v, want model=claude-sonnet-4-6 effort=\"\"", orch)
	}

	// Re-open with 'm': cursor must pre-position on claude-sonnet-4-6 (index 5); move up 2 to gemini-3.8-flash-high (index 3) and Enter.
	m = sendKey(t, m, keyRune('m'))
	if m.cursor != 5 {
		t.Fatalf("model picker cursor pre-position = %d, want 5 (claude-sonnet-4-6)", m.cursor)
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	profs, _ = config.LoadProfiles(dir)
	orch = profs.Profiles[profs.ActiveProfile].Roles[config.RoleOrchestrator]
	if orch.Model != "gemini-3.8-flash-high" || orch.Effort != "high" {
		t.Fatalf("orchestrator after picking gemini-3.8-flash-high = %+v, want model=gemini-3.8-flash-high effort=high", orch)
	}

	// Select "Custom model ID..." (last item, index 6) -> opens free-text inputRoleModel.
	m = sendKey(t, m, keyRune('m'))
	for i := 0; i < 6; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.inputMode != inputRoleModel {
		t.Fatalf("expected inputRoleModel after selecting Custom model ID..., got inputMode=%v", m.inputMode)
	}
	for i := 0; i < 30; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = sendText(t, m, "my-custom-agy-model")
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	profs, _ = config.LoadProfiles(dir)
	orch = profs.Profiles[profs.ActiveProfile].Roles[config.RoleOrchestrator]
	if orch.Model != "my-custom-agy-model" || orch.Effort != "high" {
		t.Fatalf("orchestrator after custom model = %+v, want model=my-custom-agy-model effort=high preserved", orch)
	}
}

func TestRolesEditorEffortPicker(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	opts := testOptions(home, nil, nil)
	opts.ListAgyModels = func() []AgyModelOption {
		return []AgyModelOption{
			{ID: "gemini-3.8-flash-high", Label: "Gemini 3.8 Flash (High)"},
			{ID: "gemini-3.8-flash-medium", Label: "Gemini 3.8 Flash (Medium)"},
			{ID: "gemini-3.8-flash-low", Label: "Gemini 3.8 Flash (Low)"},
			{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6"},
		}
	}
	if _, err := installer.Init(dir, installer.InitOptions{LookPath: opts.LookPath}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Pre-set explore to gemini-3.8-flash-high + effort high.
	profs, err := config.LoadProfiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	ap := profs.Profiles[profs.ActiveProfile]
	ap.Roles["explore"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-high", Effort: "high"}
	profs.Profiles[profs.ActiveProfile] = ap
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatal(err)
	}

	m := NewModel(dir, opts)
	// Navigate to Roles Editor (index 3) -> down to explore (index 1).
	for i := 0; i < 3; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})

	// Open Effort Picker with 'e'. Pre-positioned on "high" (index 3: 0=default, 1=low, 2=medium, 3=high, 4=xhigh, 5=max).
	m = sendKey(t, m, keyRune('e'))
	ev := m.View()
	for _, want := range []string{"default", "low", "medium", "high", "xhigh", "max"} {
		if !strings.Contains(ev, want) {
			t.Fatalf("effort picker missing %q:\n%s", want, ev)
		}
	}
	if m.cursor != 3 {
		t.Fatalf("effort picker cursor = %d, want 3 (high)", m.cursor)
	}

	// Esc cancels without changes.
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	profs, _ = config.LoadProfiles(dir)
	if got := profs.Profiles[profs.ActiveProfile].Roles["explore"].Effort; got != "high" {
		t.Fatalf("effort changed after Esc: %q", got)
	}

	// Open 'e', move up twice to "low" (index 1) and Enter -> switches both Effort="low" AND Model="gemini-3.8-flash-low".
	m = sendKey(t, m, keyRune('e'))
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	profs, _ = config.LoadProfiles(dir)
	exp := profs.Profiles[profs.ActiveProfile].Roles["explore"]
	if exp.Effort != "low" || exp.Model != "gemini-3.8-flash-low" {
		t.Fatalf("explore after picking low effort = %+v, want effort=low model=gemini-3.8-flash-low", exp)
	}
	// Verify agent frontmatter was synced with normalized tier ("flash", NOT "gemini-3.8-flash-low").
	expBytes, err := os.ReadFile(filepath.Join(dir, ".agents", "agents", "gentle-ai-explore.md"))
	if err != nil {
		t.Fatal(err)
	}
	fm, _, err := assets.ParseFrontmatter(expBytes)
	if err != nil || fm.Model != "flash" {
		t.Fatalf("gentle-ai-explore.md model = %q (err=%v), want flash", fm.Model, err)
	}

	// Pick "max" (not in gemini-3.8-flash-* catalog) -> Effort="max", Model stays "gemini-3.8-flash-low".
	m = sendKey(t, m, keyRune('e'))
	for i := 0; i < 4; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	profs, _ = config.LoadProfiles(dir)
	exp = profs.Profiles[profs.ActiveProfile].Roles["explore"]
	if exp.Effort != "max" || exp.Model != "gemini-3.8-flash-low" {
		t.Fatalf("explore after picking max effort = %+v, want effort=max model=gemini-3.8-flash-low", exp)
	}

	// Toggle explore to external-mcp and pick effort "medium" -> Effort="medium", external model unchanged.
	m = sendKey(t, m, keyRune('t'))
	m = sendKey(t, m, keyRune('e'))
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	profs, _ = config.LoadProfiles(dir)
	exp = profs.Profiles[profs.ActiveProfile].Roles["explore"]
	if exp.Runtime != config.RuntimeExternalMCP || exp.Effort != "medium" {
		t.Fatalf("external-mcp explore after picking medium effort = %+v", exp)
	}
}

func TestCatalogEffortMaxAndEffortOptionsDerivedFromConfig(t *testing.T) {
	if len(effortPickerOptions) != len(config.ValidEfforts) {
		t.Fatalf("effortPickerOptions len = %d, want %d (config.ValidEfforts)", len(effortPickerOptions), len(config.ValidEfforts))
	}
	for i, want := range config.ValidEfforts {
		if effortPickerOptions[i].value != want {
			t.Fatalf("effortPickerOptions[%d].value = %q, want %q", i, effortPickerOptions[i].value, want)
		}
	}

	models := []AgyModelOption{
		{ID: "my-model-high", Label: "My Model High"},
		{ID: "my-model-max", Label: "My Model Max"},
	}
	if s, ok := extractCatalogEffortSuffix("my-model-max"); !ok || s != "max" {
		t.Fatalf("extractCatalogEffortSuffix(my-model-max) = (%q, %t), want (max, true)", s, ok)
	}
	if got, ok := findEffortModelVariant(models, "my-model-high", "max"); !ok || got != "my-model-max" {
		t.Fatalf("findEffortModelVariant(..., high->max) = (%q, %t), want (my-model-max, true)", got, ok)
	}
	if got, ok := findEffortModelVariant(models, "my-model-max", "high"); !ok || got != "my-model-high" {
		t.Fatalf("findEffortModelVariant(..., max->high) = (%q, %t), want (my-model-high, true)", got, ok)
	}
}

func TestProvidersMaskedSetKeySaveClearEscAndProbe(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()

	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-v4-pro"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// Point deepseek at httptest server and leave ProbeFunc nil so default llm.ProbeProvider is used.
	profs := config.DefaultProfilesConfig()
	ds := profs.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profs.Providers["deepseek"] = ds
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	opts := Options{
		HomeDir: home,
		LookPath: func(bin string) (string, error) {
			if bin == "agy" || bin == "engram" {
				return "/usr/local/bin/" + bin, nil
			}
			return "", os.ErrNotExist
		},
		LookupEnv: func(string) (string, bool) { return "", false },
	}
	m := NewModel(dir, opts)

	// Navigate to Providers & Probe (item 4).
	for i := 0; i < 4; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	// Check footer help includes [s] set key, [u], [k], [m], [t].
	v := m.View()
	for _, hint := range []string{"[s] set key", "[u]", "[k]", "[m]", "[t]"} {
		if !strings.Contains(v, hint) {
			t.Fatalf("providers footer missing %q:\n%s", hint, v)
		}
	}

	// Move to deepseek (index 1: 0=9router, 1=deepseek).
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if !strings.Contains(m.View(), "DEEPSEEK_API_KEY(unset)") {
		t.Fatalf("expected DEEPSEEK_API_KEY(unset) initially:\n%s", m.View())
	}

	// 1. Press [s], type secret, press Esc -> cancels cleanly without saving.
	const cancelledSecret = "sk-cancelled-secret-111"
	m = sendKey(t, m, keyRune('s'))
	if !strings.Contains(m.View(), "Set API key (DEEPSEEK_API_KEY) [empty to clear]: ") {
		t.Fatalf("expected masked key prompt:\n%s", m.View())
	}
	m = sendText(t, m, cancelledSecret)
	if strings.Contains(m.View(), cancelledSecret) {
		t.Fatalf("View() leaked secret during typing:\n%s", m.View())
	}
	if !strings.Contains(m.View(), strings.Repeat("•", len([]rune(cancelledSecret)))) {
		t.Fatalf("View() did not render masked bullets:\n%s", m.View())
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.inputBuffer != "" || m.inputMode != inputNone {
		t.Fatalf("Esc did not clear input buffer/mode: mode=%v buf=%q", m.inputMode, m.inputBuffer)
	}
	loaded, err := config.LoadSecrets(home)
	if err != nil || len(loaded) != 0 {
		t.Fatalf("expected no secrets saved after Esc, got %v err=%v", loaded, err)
	}

	// 2. Press [s], type secret, press Enter -> saves 0600 file, status non-secret, shows (stored).
	const realSecret = "sk-tui-saved-secret-777"
	m = sendKey(t, m, keyRune('s'))
	if m.inputBuffer != "" {
		t.Fatalf("expected empty input buffer on opening [s], got %q", m.inputBuffer)
	}
	m = sendText(t, m, realSecret)
	if strings.Contains(m.View(), realSecret) {
		t.Fatalf("View() leaked secret during typing:\n%s", m.View())
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.inputBuffer != "" {
		t.Fatalf("Enter did not clear inputBuffer")
	}

	afterSaveView := m.View()
	if strings.Contains(afterSaveView, realSecret) {
		t.Fatalf("View() leaked secret after saving:\n%s", afterSaveView)
	}
	if !strings.Contains(afterSaveView, "Saved API key for DEEPSEEK_API_KEY in ~/.config/odd-agy/secrets.json (0600)") {
		t.Fatalf("expected saved status message in view:\n%s", afterSaveView)
	}
	if !strings.Contains(afterSaveView, "DEEPSEEK_API_KEY(stored)") {
		t.Fatalf("expected DEEPSEEK_API_KEY(stored) in view:\n%s", afterSaveView)
	}

	sp, err := config.SecretsPath(home)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(sp)
		if err != nil {
			t.Fatalf("stat secrets.json: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("secrets.json perm = %04o, want 0600", info.Mode().Perm())
		}
	}

	// 3. Press [t] immediately -> ProbeProvider uses stored key.
	m = sendKey(t, m, keyRune('t'))
	if seenAuth != "Bearer "+realSecret {
		t.Fatalf("probe Authorization header = %q, want %q", seenAuth, "Bearer "+realSecret)
	}
	probeView := m.View()
	if strings.Contains(probeView, realSecret) {
		t.Fatalf("View() leaked secret after probe:\n%s", probeView)
	}
	if !strings.Contains(probeView, "reachable") || !strings.Contains(probeView, "200") {
		t.Fatalf("expected reachable HTTP 200 after [t] probe:\n%s", probeView)
	}

	// 4. Press [s] again: buffer MUST start empty (never pre-fill existing secret); press Enter with empty input -> clears stored key.
	m = sendKey(t, m, keyRune('s'))
	if m.inputBuffer != "" {
		t.Fatalf("re-opening [s] must NEVER pre-fill existing secret, got len=%d", len(m.inputBuffer))
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	afterClearView := m.View()
	if !strings.Contains(afterClearView, "Cleared stored API key for DEEPSEEK_API_KEY") {
		t.Fatalf("expected cleared status message:\n%s", afterClearView)
	}
	if !strings.Contains(afterClearView, "DEEPSEEK_API_KEY(unset)") {
		t.Fatalf("expected DEEPSEEK_API_KEY(unset) after clearing:\n%s", afterClearView)
	}

	// 5. Env var status shows (env) and takes precedence over stored key.
	if err := config.SetSecret(home, "DEEPSEEK_API_KEY", realSecret); err != nil {
		t.Fatal(err)
	}
	optsEnv := opts
	optsEnv.LookupEnv = func(k string) (string, bool) {
		if k == "DEEPSEEK_API_KEY" {
			return "sk-env-precedence-123", true
		}
		return "", false
	}
	mEnv := NewModel(dir, optsEnv)
	for i := 0; i < 4; i++ {
		mEnv = sendKey(t, mEnv, tea.KeyMsg{Type: tea.KeyDown})
	}
	mEnv = sendKey(t, mEnv, tea.KeyMsg{Type: tea.KeyEnter})
	mEnv = sendKey(t, mEnv, tea.KeyMsg{Type: tea.KeyDown})
	if !strings.Contains(mEnv.View(), "DEEPSEEK_API_KEY(env)") {
		t.Fatalf("expected DEEPSEEK_API_KEY(env) when env is set:\n%s", mEnv.View())
	}

	// 6. PRESERVE: [u] edit baseUrl and [m] edit defaultModel still work cleanly.
	mPreserve := NewModel(dir, opts)
	for i := 0; i < 4; i++ {
		mPreserve = sendKey(t, mPreserve, tea.KeyMsg{Type: tea.KeyDown})
	}
	mPreserve = sendKey(t, mPreserve, tea.KeyMsg{Type: tea.KeyEnter})
	mPreserve = sendKey(t, mPreserve, tea.KeyMsg{Type: tea.KeyDown}) // deepseek

	mPreserve = sendKey(t, mPreserve, keyRune('u'))
	for i := 0; i < 50; i++ {
		mPreserve = sendKey(t, mPreserve, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	mPreserve = sendText(t, mPreserve, "https://api.deepseek.example/v1")
	mPreserve = sendKey(t, mPreserve, tea.KeyMsg{Type: tea.KeyEnter})

	mPreserve = sendKey(t, mPreserve, keyRune('m'))
	for i := 0; i < 30; i++ {
		mPreserve = sendKey(t, mPreserve, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	mPreserve = sendText(t, mPreserve, "deepseek-chat")
	mPreserve = sendKey(t, mPreserve, tea.KeyMsg{Type: tea.KeyEnter})

	savedProfs, err := config.LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles preserve: %v", err)
	}
	if got := savedProfs.Providers["deepseek"]; got.BaseURL != "https://api.deepseek.example/v1" || got.DefaultModel != "deepseek-chat" {
		t.Fatalf("preserved provider edits mismatch: %+v", got)
	}
}

func TestProvidersSetKeyControlCharPasteAndSymlinkErrorClearBuffer(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	opts := testOptions(home, nil, nil)

	openDeepSeekProviders := func() Model {
		m := NewModel(dir, opts)
		for i := 0; i < 4; i++ {
			m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		}
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown}) // deepseek
		return m
	}

	// 1. Control-character paste (\n, \r, \x00) is rejected without leaking secret and clears inputBuffer/mode.
	for _, badPaste := range []string{"sk-pasted-secret\nnextline", "sk-pasted-secret\rcarriage", "sk-pasted-secret\x00nul"} {
		m := openDeepSeekProviders()
		m = sendKey(t, m, keyRune('s'))
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(badPaste), Paste: true})
		if strings.Contains(m.View(), "sk-pasted-secret") {
			t.Fatalf("View() leaked pasted secret before Enter:\n%s", m.View())
		}
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if m.inputMode != inputNone || m.inputBuffer != "" || m.inputTarget != "" {
			t.Fatalf("expected input state cleared after rejected paste, got mode=%v buf=%q target=%q", m.inputMode, m.inputBuffer, m.inputTarget)
		}
		if m.errMsg == "" {
			t.Fatalf("expected error message for control-char paste")
		}
		if strings.Contains(m.errMsg, "sk-pasted-secret") || strings.Contains(m.View(), "sk-pasted-secret") {
			t.Fatalf("error/view leaked pasted secret: err=%q view=%s", m.errMsg, m.View())
		}
	}

	// 2. Symlink error on secrets.json rejects save, clears inputBuffer/mode, and never leaks secret.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside-secrets.json")
	cfgDir := filepath.Join(home, ".config", "odd-agy")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(cfgDir, "secrets.json")); err != nil {
		t.Fatal(err)
	}

	const symlinkSecret = "sk-symlink-should-clear-buffer"
	m := openDeepSeekProviders()
	m = sendKey(t, m, keyRune('s'))
	m = sendText(t, m, symlinkSecret)
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.inputMode != inputNone || m.inputBuffer != "" || m.inputTarget != "" {
		t.Fatalf("expected input state cleared after symlink error, got mode=%v buf=%q target=%q", m.inputMode, m.inputBuffer, m.inputTarget)
	}
	if m.errMsg == "" || !strings.Contains(m.errMsg, "symlink") {
		t.Fatalf("expected symlink error message, got errMsg=%q", m.errMsg)
	}
	if strings.Contains(m.errMsg, symlinkSecret) || strings.Contains(m.View(), symlinkSecret) {
		t.Fatalf("symlink error/view leaked secret: err=%q", m.errMsg)
	}
	if _, err := os.Lstat(outsideFile); !os.IsNotExist(err) {
		t.Fatalf("symlink target must not be created, stat err=%v", err)
	}

	// Re-opening [s] must start with an empty buffer.
	m = sendKey(t, m, keyRune('s'))
	if m.inputBuffer != "" {
		t.Fatalf("expected empty inputBuffer after previous error, got len=%d", len(m.inputBuffer))
	}
}

func TestProjectActionsGlobalMCPSymlinkRejectionDoesNotMutateSettings(t *testing.T) {
	t.Run("enable_rejected_on_mcp_config_symlink_leaves_settings_unchanged", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()

		settingsPath := installer.GlobalSettingsPath(home)
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		const settingsOrig = "{\n  \"theme\": \"dark\"\n}\n"
		if err := os.WriteFile(settingsPath, []byte(settingsOrig), 0o600); err != nil {
			t.Fatal(err)
		}

		outside := t.TempDir()
		outsideMCP := filepath.Join(outside, "outside-mcp.json")
		const mcpSentinel = `{"mcpServers":{"keep":{"command":"x"}}}` + "\n"
		if err := os.WriteFile(outsideMCP, []byte(mcpSentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		mcpPath := installer.GlobalMCPConfigPath(home)
		if err := os.MkdirAll(filepath.Dir(mcpPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideMCP, mcpPath); err != nil {
			t.Fatal(err)
		}

		m := NewModel(dir, testOptions(home, nil, nil))
		for i := 0; i < 5; i++ {
			m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		}
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		for i := 0; i < 4; i++ {
			m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		}
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

		if m.errMsg == "" {
			t.Fatalf("expected errMsg when mcp_config.json is a symlink")
		}
		if got, _ := os.ReadFile(settingsPath); string(got) != settingsOrig {
			t.Fatalf("settings.json mutated despite symlink rejection:\ngot:\n%s\nwant:\n%s", string(got), settingsOrig)
		}
		if matches, _ := filepath.Glob(settingsPath + ".odd-agy.bak.*"); len(matches) != 0 {
			t.Fatalf("unexpected settings backups created: %v", matches)
		}
		if got, _ := os.ReadFile(outsideMCP); string(got) != mcpSentinel {
			t.Fatalf("external symlink target mutated: %q", string(got))
		}
	})

	t.Run("disable_rejected_on_mcp_config_symlink_leaves_settings_unchanged", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()

		if _, err := installer.GrantGlobalMCPPermissions(home); err != nil {
			t.Fatal(err)
		}
		if _, err := installer.RegisterGlobalMCPServer(home, "odd-agy"); err != nil {
			t.Fatal(err)
		}
		settingsPath := installer.GlobalSettingsPath(home)
		settingsOrig, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}

		// Load model while enabled, then replace mcp_config.json with a symlink before toggling.
		m := NewModel(dir, testOptions(home, nil, nil))
		outside := t.TempDir()
		outsideMCP := filepath.Join(outside, "outside-mcp.json")
		const mcpSentinel = `{"mcpServers":{"odd-agy":{"command":"odd-agy","args":["mcp"],"disabled":false}}}` + "\n"
		if err := os.WriteFile(outsideMCP, []byte(mcpSentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		mcpPath := installer.GlobalMCPConfigPath(home)
		if err := os.Remove(mcpPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideMCP, mcpPath); err != nil {
			t.Fatal(err)
		}

		for i := 0; i < 5; i++ {
			m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		}
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		for i := 0; i < 4; i++ {
			m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
		}
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

		if m.errMsg == "" {
			t.Fatalf("expected errMsg when mcp_config.json is a symlink during disable")
		}
		if got, _ := os.ReadFile(settingsPath); string(got) != string(settingsOrig) {
			t.Fatalf("settings.json mutated despite symlink rejection during disable:\ngot:\n%s\nwant:\n%s", string(got), string(settingsOrig))
		}
		if got, _ := os.ReadFile(outsideMCP); string(got) != mcpSentinel {
			t.Fatalf("external symlink target mutated: %q", string(got))
		}
	})
}

