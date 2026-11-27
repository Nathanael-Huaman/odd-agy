package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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

	if !strings.Contains(m.View(), "explore") || !strings.Contains(m.View(), "review-refuter") {
		t.Fatalf("expected all roles in editor view:\n%s", m.View())
	}

	// Cursor is on role 0 ("explore"). Toggle runtime agy-native -> external-mcp.
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

	// Edit model with 'm', backspace existing, enter new model
	m = sendKey(t, m, keyRune('m'))
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

	// Move down to Item 4: Allow MCP globally (~/.gemini)
	for i := 0; i < 4; i++ {
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	enabled, err := installer.CheckGlobalMCPPermissions(home)
	if err != nil || !enabled {
		t.Fatalf("CheckGlobalMCPPermissions after TUI grant = (%t, %v)", enabled, err)
	}
	if !strings.Contains(m.View(), "Revoke global MCP rule") {
		t.Fatalf("expected Revoke global MCP rule label after grant:\n%s", m.View())
	}

	// Toggle again -> Revoke global MCP rule
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	enabled, err = installer.CheckGlobalMCPPermissions(home)
	if err != nil || enabled {
		t.Fatalf("CheckGlobalMCPPermissions after TUI revoke = (%t, %v)", enabled, err)
	}
	if !strings.Contains(m.View(), "Allow MCP globally (~/.gemini)") {
		t.Fatalf("expected Allow MCP globally label after revoke:\n%s", m.View())
	}
}
