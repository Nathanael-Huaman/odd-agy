package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/installer"
	"github.com/Nathanael-Huaman/odd-agy/internal/llm"
)

type screenKind int

const (
	screenOverview screenKind = iota
	screenProfiles
	screenRoles
	screenModelPicker
	screenEffortPicker
	screenProviders
	screenActions
)

type inputKind int

const (
	inputNone inputKind = iota
	inputRoleModel
	inputProviderURL
	inputProviderKeyEnv
	inputProviderModel
	inputProviderKey
)

// AgyModelOption represents a selectable Antigravity CLI model entry.
type AgyModelOption struct {
	ID    string
	Label string
}

// Options injects external dependencies so the TUI is 100% testable hermetically.
type Options struct {
	HomeDir       string
	LookPath      func(string) (string, error)
	LookupEnv     func(string) (string, bool)
	ProbeFunc     func(ctx context.Context, providerName string, providerCfg config.ProviderConfig, lookupEnv func(string) (string, bool)) llm.ProbeResult
	ListAgyModels func() []AgyModelOption
}

type probeResultMsg struct {
	result llm.ProbeResult
}

// Model is the Bubble Tea state model for the odd-agy interactive terminal UI.
type Model struct {
	cwd  string
	opts Options

	cfg                 config.Config
	profiles            config.ProfilesConfig
	installed           bool
	globalMCPAllowed    bool
	globalMCPRegistered bool
	globalMCPCommand    string

	screen        screenKind
	cursor        int
	profileNames  []string
	providerNames []string
	probeResults  map[string]llm.ProbeResult
	doctorReport  *installer.DoctorReport

	confirmUninstall   bool
	inputMode          inputKind
	inputBuffer        string
	inputTarget        string
	pickerRole         string
	pickerReturnCursor int
	agyModels          []AgyModelOption
	agyModelsLoaded    bool

	statusMsg string
	errMsg    string
	width     int
	height    int
}

// Run launches the interactive Bubble Tea TUI for the given workspace directory.
func Run(cwd string) error {
	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = "."
		}
	}
	p := tea.NewProgram(NewModel(cwd, Options{}), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// NewModel constructs an initialized TUI Model rooted at cwd.
func NewModel(cwd string, opts Options) Model {
	if strings.TrimSpace(cwd) == "" {
		cwd = "."
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if opts.HomeDir == "" {
		opts.HomeDir, _ = os.UserHomeDir()
	}
	if opts.ProbeFunc == nil {
		opts.ProbeFunc = llm.ProbeProvider
	}

	m := Model{
		cwd:          cwd,
		opts:         opts,
		screen:       screenOverview,
		probeResults: make(map[string]llm.ProbeResult),
	}
	m.reload()
	return m
}

func (m *Model) reload() {
	m.errMsg = ""
	if _, err := os.Stat(installer.ManifestPath(m.cwd)); err == nil {
		m.installed = true
	} else {
		m.installed = false
	}
	if allowed, err := installer.CheckGlobalMCPPermissions(m.opts.HomeDir); err == nil {
		m.globalMCPAllowed = allowed
	} else {
		m.globalMCPAllowed = false
	}
	if registered, cmd, err := installer.GlobalMCPServerRegistered(m.opts.HomeDir); err == nil {
		m.globalMCPRegistered = registered
		m.globalMCPCommand = cmd
	} else {
		m.globalMCPRegistered = false
		m.globalMCPCommand = ""
	}

	cfg, err := config.LoadConfig(m.cwd)
	if err != nil {
		m.errMsg = err.Error()
		cfg = config.DefaultConfig()
	}
	m.cfg = cfg

	profs, err := config.LoadProfiles(m.cwd)
	if err != nil {
		if m.errMsg == "" {
			m.errMsg = err.Error()
		}
		profs = config.DefaultProfilesConfig()
	}
	m.profiles = profs

	m.profileNames = make([]string, 0, len(profs.Profiles))
	for name := range profs.Profiles {
		m.profileNames = append(m.profileNames, name)
	}
	sort.Strings(m.profileNames)

	m.providerNames = make([]string, 0, len(profs.Providers))
	for name := range profs.Providers {
		m.providerNames = append(m.providerNames, name)
	}
	sort.Strings(m.providerNames)
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case probeResultMsg:
		m.probeResults[msg.result.Provider] = msg.result
		if msg.result.Reachable {
			m.statusMsg = fmt.Sprintf("Probe %s: reachable (HTTP %d, %dms)", msg.result.Provider, msg.result.StatusCode, msg.result.LatencyMS)
			m.errMsg = ""
		} else {
			m.errMsg = fmt.Sprintf("Probe %s: unreachable (%s)", msg.result.Provider, msg.result.Error)
		}
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		if m.inputMode != inputNone {
			return m.handleInputKey(msg)
		}
		if m.confirmUninstall {
			return m.handleUninstallConfirmKey(msg)
		}
		return m.handleNavigationKey(msg)
	}

	return m, nil
}

func (m Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.inputMode = inputNone
		m.inputBuffer = ""
		m.inputTarget = ""
		m.statusMsg = "Edit canceled"
		return m, nil

	case tea.KeyBackspace, tea.KeyDelete:
		runes := []rune(m.inputBuffer)
		if len(runes) > 0 {
			m.inputBuffer = string(runes[:len(runes)-1])
		}
		return m, nil

	case tea.KeyEnter:
		if m.inputMode == inputProviderKey {
			if err := m.commitProviderKey(); err != nil {
				m.errMsg = err.Error()
			}
			m.inputMode = inputNone
			m.inputBuffer = ""
			m.inputTarget = ""
			return m, nil
		}
		val := strings.TrimSpace(m.inputBuffer)
		if err := m.commitInput(val); err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.inputMode = inputNone
		m.inputBuffer = ""
		m.inputTarget = ""
		return m, nil

	default:
		if msg.Type == tea.KeySpace {
			m.inputBuffer += " "
			return m, nil
		}
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
			m.inputBuffer += string(msg.Runes)
		}
		return m, nil
	}
}

func (m *Model) commitInput(val string) error {
	switch m.inputMode {
	case inputRoleModel:
		if val == "" {
			return fmt.Errorf("model must not be empty")
		}
		prof := m.profiles.Profiles[m.profiles.ActiveProfile]
		route := prof.Roles[m.inputTarget]
		route.Model = val
		prof.Roles[m.inputTarget] = route
		m.profiles.Profiles[m.profiles.ActiveProfile] = prof
		return m.persistProfileChange(fmt.Sprintf("Updated %s model to %s", m.inputTarget, val))

	case inputProviderURL:
		prov := m.profiles.Providers[m.inputTarget]
		prov.BaseURL = val
		m.profiles.Providers[m.inputTarget] = prov
		if err := config.SaveProfiles(m.cwd, m.profiles); err != nil {
			m.reload()
			return err
		}
		m.errMsg = ""
		m.statusMsg = fmt.Sprintf("Updated %s baseUrl to %s", m.inputTarget, llm.RedactURL(val))

	case inputProviderKeyEnv:
		prov := m.profiles.Providers[m.inputTarget]
		prov.APIKeyEnv = val
		m.profiles.Providers[m.inputTarget] = prov
		if err := config.SaveProfiles(m.cwd, m.profiles); err != nil {
			m.reload()
			return err
		}
		m.errMsg = ""
		m.statusMsg = fmt.Sprintf("Updated %s apiKeyEnv to %s", m.inputTarget, val)

	case inputProviderModel:
		if val == "" {
			return fmt.Errorf("defaultModel must not be empty")
		}
		prov := m.profiles.Providers[m.inputTarget]
		prov.DefaultModel = val
		m.profiles.Providers[m.inputTarget] = prov
		if err := config.SaveProfiles(m.cwd, m.profiles); err != nil {
			m.reload()
			return err
		}
		m.errMsg = ""
		m.statusMsg = fmt.Sprintf("Updated %s defaultModel to %s", m.inputTarget, val)
	}
	return nil
}

func (m *Model) commitProviderKey() error {
	prov := m.profiles.Providers[m.inputTarget]
	keyEnv := prov.APIKeyEnv
	rawVal := m.inputBuffer
	m.inputBuffer = ""
	if err := config.SetSecret(m.opts.HomeDir, keyEnv, rawVal); err != nil {
		return err
	}
	m.errMsg = ""
	if strings.TrimSpace(rawVal) == "" {
		m.statusMsg = fmt.Sprintf("Cleared stored API key for %s", keyEnv)
	} else {
		m.statusMsg = fmt.Sprintf("Saved API key for %s in ~/.config/odd-agy/secrets.json (0600)", keyEnv)
	}
	return nil
}

func (m Model) handleUninstallConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.confirmUninstall = false
	key := strings.ToLower(msg.String())
	if key == "y" {
		res, err := installer.Uninstall(m.cwd)
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.reload()
		m.errMsg = ""
		m.statusMsg = fmt.Sprintf("Uninstalled: %d removed, %d updated, %d preserved", len(res.Removed), len(res.Updated), len(res.Preserved))
		return m, nil
	}
	m.statusMsg = "Uninstall canceled"
	return m, nil
}

func (m Model) handleNavigationKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "q":
		return m, tea.Quit
	case "r":
		m.reload()
		m.statusMsg = "Reloaded workspace configuration"
		return m, nil
	case "esc":
		switch m.screen {
		case screenModelPicker, screenEffortPicker:
			m.screen = screenRoles
			m.cursor = m.pickerReturnCursor
			return m, nil
		case screenOverview:
			return m, nil
		default:
			m.screen = screenOverview
			m.cursor = 0
			m.doctorReport = nil
			return m, nil
		}
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down", "j":
		maxIdx := m.maxCursorIndex()
		if m.cursor < maxIdx {
			m.cursor++
		}
		return m, nil
	}

	switch m.screen {
	case screenOverview:
		return m.handleOverviewKey(key)
	case screenProfiles:
		return m.handleProfilesKey(key)
	case screenRoles:
		return m.handleRolesKey(key)
	case screenModelPicker:
		return m.handleModelPickerKey(key)
	case screenEffortPicker:
		return m.handleEffortPickerKey(key)
	case screenProviders:
		return m.handleProvidersKey(key)
	case screenActions:
		return m.handleActionsKey(key)
	}
	return m, nil
}

func (m Model) maxCursorIndex() int {
	switch m.screen {
	case screenOverview:
		return 5
	case screenProfiles:
		if len(m.profileNames) == 0 {
			return 0
		}
		return len(m.profileNames) - 1
	case screenRoles:
		return len(config.AllRoles) - 1
	case screenModelPicker:
		opts := m.modelPickerOptions()
		if len(opts) == 0 {
			return 0
		}
		return len(opts) - 1
	case screenEffortPicker:
		return len(effortPickerOptions) - 1
	case screenProviders:
		if len(m.providerNames) == 0 {
			return 0
		}
		return len(m.providerNames) - 1
	case screenActions:
		return 4
	default:
		return 0
	}
}

func (m Model) handleOverviewKey(key string) (tea.Model, tea.Cmd) {
	isActivate := key == "enter" || key == " "
	if !isActivate {
		return m, nil
	}

	switch m.cursor {
	case 0: // Toggle Delegation Mode
		if m.cfg.DelegationMode == config.DelegationModeFull {
			m.cfg.DelegationMode = config.DelegationModeOrganic
		} else {
			m.cfg.DelegationMode = config.DelegationModeFull
		}
		if err := config.SaveConfig(m.cwd, m.cfg); err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.errMsg = ""
		m.statusMsg = fmt.Sprintf("Delegation mode set to %s", m.cfg.DelegationMode)

	case 1: // Toggle RDD
		m.cfg.RDDEnabled = !m.cfg.RDDEnabled
		if err := config.SaveConfig(m.cwd, m.cfg); err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.errMsg = ""
		m.statusMsg = fmt.Sprintf("RDD set to %s", boolOnOff(m.cfg.RDDEnabled))

	case 2: // Active Profile
		if key == " " && len(m.profileNames) > 0 {
			nextIdx := 0
			for i, name := range m.profileNames {
				if name == m.profiles.ActiveProfile {
					nextIdx = (i + 1) % len(m.profileNames)
					break
				}
			}
			m.profiles.ActiveProfile = m.profileNames[nextIdx]
			_ = m.persistProfileChange(fmt.Sprintf("Active profile set to %s", m.profiles.ActiveProfile))
			return m, nil
		}
		m.screen = screenProfiles
		m.cursor = 0
		for i, name := range m.profileNames {
			if name == m.profiles.ActiveProfile {
				m.cursor = i
				break
			}
		}

	case 3: // Roles Editor
		m.screen = screenRoles
		m.cursor = 0

	case 4: // Providers & Probe
		m.screen = screenProviders
		m.cursor = 0

	case 5: // Project Actions
		m.screen = screenActions
		m.cursor = 0
		m.doctorReport = nil
	}
	return m, nil
}

func (m Model) handleProfilesKey(key string) (tea.Model, tea.Cmd) {
	if key != "enter" && key != " " {
		return m, nil
	}
	if m.cursor < 0 || m.cursor >= len(m.profileNames) {
		return m, nil
	}
	chosen := m.profileNames[m.cursor]
	m.profiles.ActiveProfile = chosen
	if err := m.persistProfileChange(fmt.Sprintf("Active profile set to %s", chosen)); err != nil {
		return m, nil
	}
	m.screen = screenOverview
	m.cursor = 2
	return m, nil
}

func (m Model) handleRolesKey(key string) (tea.Model, tea.Cmd) {
	if m.cursor < 0 || m.cursor >= len(config.AllRoles) {
		return m, nil
	}
	role := config.AllRoles[m.cursor]
	prof := m.profiles.Profiles[m.profiles.ActiveProfile]
	route := prof.Roles[role]

	switch key {
	case "t", " ":
		if role == config.RoleOrchestrator {
			m.errMsg = ""
			m.statusMsg = "orchestrator always runs on agy-native (press [m] to edit its model)"
			return m, nil
		}
		m.toggleRoleRuntime(role)

	case "enter":
		if role == config.RoleOrchestrator {
			m.openModelPicker(role, m.cursor)
			return m, nil
		}
		m.toggleRoleRuntime(role)

	case "p":
		if role == config.RoleOrchestrator {
			m.errMsg = ""
			m.statusMsg = "orchestrator always runs on agy-native (press [m] to edit its model)"
			return m, nil
		}
		if len(m.providerNames) == 0 {
			m.errMsg = "No providers configured"
			return m, nil
		}
		nextIdx := 0
		if route.Runtime == config.RuntimeExternalMCP {
			for i, p := range m.providerNames {
				if p == route.Provider {
					nextIdx = (i + 1) % len(m.providerNames)
					break
				}
			}
		}
		nextProv := m.providerNames[nextIdx]
		route.Runtime = config.RuntimeExternalMCP
		route.Provider = nextProv
		route.Model = m.profiles.Providers[nextProv].DefaultModel
		prof.Roles[role] = route
		m.profiles.Profiles[m.profiles.ActiveProfile] = prof
		_ = m.persistProfileChange(fmt.Sprintf("Role %s provider set to %s", role, nextProv))

	case "m":
		if route.Runtime == config.RuntimeAgyNative {
			m.openModelPicker(role, m.cursor)
			return m, nil
		}
		m.inputMode = inputRoleModel
		m.inputTarget = role
		m.inputBuffer = route.Model
		m.errMsg = ""

	case "e":
		m.openEffortPicker(role, m.cursor)
		return m, nil
	}

	return m, nil
}

func (m *Model) toggleRoleRuntime(role string) {
	prof := m.profiles.Profiles[m.profiles.ActiveProfile]
	route := prof.Roles[role]
	if route.Runtime == config.RuntimeAgyNative {
		if len(m.providerNames) == 0 {
			m.errMsg = "No providers configured"
			return
		}
		provName := m.providerNames[0]
		provCfg := m.profiles.Providers[provName]
		route.Runtime = config.RuntimeExternalMCP
		route.Provider = provName
		if route.Model == "" || route.Model == "pro" || route.Model == "flash" {
			route.Model = provCfg.DefaultModel
		}
	} else {
		route.Runtime = config.RuntimeAgyNative
		route.Provider = ""
		if role == "explore" {
			route.Model = "flash"
		} else {
			route.Model = "pro"
		}
	}
	prof.Roles[role] = route
	m.profiles.Profiles[m.profiles.ActiveProfile] = prof
	_ = m.persistProfileChange(fmt.Sprintf("Role %s runtime set to %s", role, route.Runtime))
}

func (m *Model) openModelPicker(role string, roleIdx int) {
	m.pickerRole = role
	m.pickerReturnCursor = roleIdx
	m.screen = screenModelPicker
	m.errMsg = ""

	opts := m.modelPickerOptions()
	curModel := m.profiles.Profiles[m.profiles.ActiveProfile].Roles[role].Model
	m.cursor = 0
	for i, opt := range opts {
		if !opt.isCustom && opt.id == curModel {
			m.cursor = i
			break
		}
	}
}

func (m *Model) openEffortPicker(role string, roleIdx int) {
	m.pickerRole = role
	m.pickerReturnCursor = roleIdx
	m.screen = screenEffortPicker
	m.errMsg = ""

	curEffort := m.profiles.Profiles[m.profiles.ActiveProfile].Roles[role].Effort
	m.cursor = 0
	for i, opt := range effortPickerOptions {
		if opt.value == curEffort {
			m.cursor = i
			break
		}
	}
}

func (m Model) handleModelPickerKey(key string) (tea.Model, tea.Cmd) {
	if key != "enter" && key != " " {
		return m, nil
	}
	opts := m.modelPickerOptions()
	if m.cursor < 0 || m.cursor >= len(opts) {
		return m, nil
	}
	chosen := opts[m.cursor]
	prof := m.profiles.Profiles[m.profiles.ActiveProfile]
	route := prof.Roles[m.pickerRole]

	if chosen.isCustom {
		m.screen = screenRoles
		m.cursor = m.pickerReturnCursor
		m.inputMode = inputRoleModel
		m.inputTarget = m.pickerRole
		m.inputBuffer = route.Model
		m.errMsg = ""
		return m, nil
	}

	route.Model = chosen.id
	if chosen.fromCatalog {
		if suffix, ok := extractCatalogEffortSuffix(chosen.id); ok {
			route.Effort = suffix
		}
	}
	prof.Roles[m.pickerRole] = route
	m.profiles.Profiles[m.profiles.ActiveProfile] = prof

	m.screen = screenRoles
	m.cursor = m.pickerReturnCursor
	_ = m.persistProfileChange(fmt.Sprintf("Updated %s model to %s", m.pickerRole, chosen.id))
	return m, nil
}

func (m Model) handleEffortPickerKey(key string) (tea.Model, tea.Cmd) {
	if key != "enter" && key != " " {
		return m, nil
	}
	if m.cursor < 0 || m.cursor >= len(effortPickerOptions) {
		return m, nil
	}
	chosen := effortPickerOptions[m.cursor].value
	prof := m.profiles.Profiles[m.profiles.ActiveProfile]
	route := prof.Roles[m.pickerRole]

	route.Effort = chosen
	if route.Runtime == config.RuntimeAgyNative && chosen != "" {
		if variant, ok := findEffortModelVariant(m.ensureAgyModels(), route.Model, chosen); ok {
			route.Model = variant
		}
	}
	prof.Roles[m.pickerRole] = route
	m.profiles.Profiles[m.profiles.ActiveProfile] = prof

	m.screen = screenRoles
	m.cursor = m.pickerReturnCursor
	label := chosen
	if label == "" {
		label = "default"
	}
	_ = m.persistProfileChange(fmt.Sprintf("Updated %s effort to %s", m.pickerRole, label))
	return m, nil
}

func (m Model) handleProvidersKey(key string) (tea.Model, tea.Cmd) {
	if m.cursor < 0 || m.cursor >= len(m.providerNames) {
		return m, nil
	}
	name := m.providerNames[m.cursor]
	prov := m.profiles.Providers[name]

	switch key {
	case "p", "t", "enter", " ":
		m.statusMsg = fmt.Sprintf("Probing %s...", name)
		m.errMsg = ""
		probeFn := m.opts.ProbeFunc
		lookupEnv := config.NewSecretsLookupEnv(m.opts.HomeDir, m.opts.LookupEnv)
		return m, func() tea.Msg {
			res := probeFn(context.Background(), name, prov, lookupEnv)
			return probeResultMsg{result: res}
		}

	case "s":
		if err := config.ValidateSecretKeyName(prov.APIKeyEnv); err != nil {
			m.errMsg = fmt.Sprintf("Provider %s has invalid apiKeyEnv (%v); set [k] apiKeyEnv first", name, err)
			return m, nil
		}
		m.inputMode = inputProviderKey
		m.inputTarget = name
		m.inputBuffer = ""
		m.errMsg = ""
		m.statusMsg = ""

	case "u":
		m.inputMode = inputProviderURL
		m.inputTarget = name
		m.inputBuffer = prov.BaseURL
		m.errMsg = ""

	case "k":
		m.inputMode = inputProviderKeyEnv
		m.inputTarget = name
		m.inputBuffer = prov.APIKeyEnv
		m.errMsg = ""

	case "m":
		m.inputMode = inputProviderModel
		m.inputTarget = name
		m.inputBuffer = prov.DefaultModel
		m.errMsg = ""
	}
	return m, nil
}

func (m Model) handleActionsKey(key string) (tea.Model, tea.Cmd) {
	if key != "enter" && key != " " {
		return m, nil
	}

	switch m.cursor {
	case 0: // Init
		res, err := installer.Init(m.cwd, installer.InitOptions{
			LookPath: m.opts.LookPath,
		})
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.reload()
		m.errMsg = ""
		m.statusMsg = fmt.Sprintf("Initialized %d asset(s) under .agents/", len(res.InstalledFiles))

	case 1: // Sync
		res, err := installer.Sync(m.cwd, installer.SyncOptions{})
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.reload()
		m.errMsg = ""
		m.statusMsg = fmt.Sprintf("Synced: %d created, %d updated, %d unchanged, %d removed",
			len(res.Created), len(res.Updated), len(res.Unchanged), len(res.Removed))

	case 2: // Doctor
		report, err := installer.Doctor(m.cwd, installer.DoctorOptions{
			HomeDir:   m.opts.HomeDir,
			LookPath:  m.opts.LookPath,
			LookupEnv: m.opts.LookupEnv,
		})
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.doctorReport = &report
		m.errMsg = ""
		if report.HasFailures() {
			m.statusMsg = "Doctor finished with failures"
		} else {
			m.statusMsg = "Doctor finished cleanly"
		}

	case 3: // Uninstall (confirm first)
		m.confirmUninstall = true
		m.errMsg = ""
		m.statusMsg = "Confirm uninstall of managed .agents/ files"

	case 4: // Toggle global MCP permissions + registration (~/.gemini/)
		if m.globalMCPAllowed && m.globalMCPRegistered {
			permRes, err := installer.RevokeGlobalMCPPermissions(m.opts.HomeDir)
			if err != nil {
				m.errMsg = err.Error()
				return m, nil
			}
			regRes, err := installer.UnregisterGlobalMCPServer(m.opts.HomeDir)
			if err != nil {
				m.reload()
				m.errMsg = err.Error()
				return m, nil
			}
			m.reload()
			m.errMsg = ""
			m.statusMsg = fmt.Sprintf("Revoked global MCP permission & registration (%s, %s)", permRes.SettingsPath, regRes.ConfigPath)
		} else {
			permRes, err := installer.GrantGlobalMCPPermissions(m.opts.HomeDir)
			if err != nil {
				m.errMsg = err.Error()
				return m, nil
			}
			regRes, err := installer.RegisterGlobalMCPServer(m.opts.HomeDir, installer.ResolveBinaryPath(""))
			if err != nil {
				m.reload()
				m.errMsg = err.Error()
				return m, nil
			}
			m.reload()
			m.errMsg = ""
			if permRes.BackupPath != "" || regRes.BackupPath != "" {
				m.statusMsg = fmt.Sprintf("Allowed & registered MCP globally (backups created; %s, %s)", permRes.SettingsPath, regRes.ConfigPath)
			} else {
				m.statusMsg = fmt.Sprintf("Allowed & registered MCP globally (%s, %s)", permRes.SettingsPath, regRes.ConfigPath)
			}
		}
	}
	return m, nil
}

func (m *Model) persistProfileChange(baseStatus string) error {
	if err := config.ValidateProfiles(m.profiles); err != nil {
		m.reload()
		m.errMsg = err.Error()
		return err
	}
	updated, err := installer.ApplyProfileModels(m.cwd, m.profiles)
	if err != nil {
		m.reload()
		m.errMsg = err.Error()
		return err
	}
	if err := config.SaveProfiles(m.cwd, m.profiles); err != nil {
		_, _ = installer.ApplyActiveProfileModels(m.cwd)
		m.reload()
		m.errMsg = err.Error()
		return err
	}
	m.errMsg = ""
	if updated > 0 {
		m.statusMsg = fmt.Sprintf("%s (%d .agents/agents/*.md synced; restart odd-agy launch to apply)", baseStatus, updated)
	} else {
		m.statusMsg = baseStatus
	}
	return nil
}

func boolOnOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

type modelPickerItem struct {
	id          string
	label       string
	fromCatalog bool
	isCustom    bool
}

type effortPickerItem struct {
	value string
	label string
}

var effortPickerOptions = buildEffortPickerOptions()

func buildEffortPickerOptions() []effortPickerItem {
	opts := make([]effortPickerItem, 0, len(config.ValidEfforts))
	for _, v := range config.ValidEfforts {
		label := v
		if v == "" {
			label = "default (Antigravity default / inherit)"
		}
		opts = append(opts, effortPickerItem{value: v, label: label})
	}
	return opts
}

var fallbackAgyModels = []AgyModelOption{
	{ID: "gemini-4-hydrogen-high", Label: "Gemini 4 Hydrogen (High)"},
	{ID: "gemini-4-hydrogen-xhigh", Label: "Gemini 4 Hydrogen (XHigh)"},
	{ID: "gemini-4-hydrogen-medium", Label: "Gemini 4 Hydrogen (Medium)"},
	{ID: "gemini-4-hydrogen-low", Label: "Gemini 4 Hydrogen (Low)"},
	{ID: "gemini-3.8-flash-high", Label: "Gemini 3.8 Flash (High)"},
	{ID: "gemini-3.8-flash-medium", Label: "Gemini 3.8 Flash (Medium)"},
	{ID: "gemini-3.8-flash-low", Label: "Gemini 3.8 Flash (Low)"},
	{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6 (Thinking)"},
	{ID: "claude-opus-4-6-thinking", Label: "Claude Opus 4.6 (Thinking)"},
	{ID: "gpt-oss-120b-medium", Label: "GPT-OSS 120B (Medium)"},
}

func (m *Model) ensureAgyModels() []AgyModelOption {
	if !m.agyModelsLoaded {
		if m.opts.ListAgyModels != nil {
			m.agyModels = m.opts.ListAgyModels()
		} else {
			m.agyModels = defaultListAgyModels(m.opts.LookPath)
		}
		m.agyModelsLoaded = true
	}
	return m.agyModels
}

func (m *Model) modelPickerOptions() []modelPickerItem {
	if m.pickerRole != config.RoleOrchestrator {
		return m.buildModelPickerOptions(nil)
	}
	return m.buildModelPickerOptions(m.ensureAgyModels())
}

func (m Model) buildModelPickerOptions(catalog []AgyModelOption) []modelPickerItem {
	if m.pickerRole != config.RoleOrchestrator {
		return []modelPickerItem{
			{id: "inherit", label: "inherit session model"},
			{id: "flash", label: "Antigravity fast tier"},
			{id: "pro", label: "Antigravity pro/high tier"},
		}
	}
	items := make([]modelPickerItem, 0, len(catalog)+4)
	items = append(items,
		modelPickerItem{id: "inherit", label: "Antigravity session default"},
		modelPickerItem{id: "flash", label: "Antigravity fast alias"},
		modelPickerItem{id: "pro", label: "Antigravity pro alias"},
	)
	for _, opt := range catalog {
		if opt.ID == "inherit" || opt.ID == "flash" || opt.ID == "pro" {
			continue
		}
		items = append(items, modelPickerItem{
			id:          opt.ID,
			label:       opt.Label,
			fromCatalog: true,
		})
	}
	items = append(items, modelPickerItem{
		label:    "Custom model ID...",
		isCustom: true,
	})
	return items
}

func defaultListAgyModels(lookPath func(string) (string, error)) []AgyModelOption {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	agyPath, err := lookPath("agy")
	if err != nil || strings.TrimSpace(agyPath) == "" {
		return append([]AgyModelOption(nil), fallbackAgyModels...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, agyPath, "models").Output()
	if err != nil {
		return append([]AgyModelOption(nil), fallbackAgyModels...)
	}
	parsed := parseAgyModelsOutput(string(out))
	if len(parsed) == 0 {
		return append([]AgyModelOption(nil), fallbackAgyModels...)
	}
	return parsed
}

func parseAgyModelsOutput(raw string) []AgyModelOption {
	lines := strings.Split(raw, "\n")
	out := make([]AgyModelOption, 0, len(lines))
	seen := make(map[string]bool)
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		label := strings.TrimSpace(parts[1])
		if id == "" || strings.ContainsAny(id, " \t") || hasControlChar(id) || hasControlChar(label) {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, AgyModelOption{ID: id, Label: label})
	}
	return out
}

func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

var catalogEffortSuffixes = []string{"xhigh", "medium", "high", "low", "max"}

func extractCatalogEffortSuffix(modelID string) (string, bool) {
	for _, s := range catalogEffortSuffixes {
		if strings.HasSuffix(modelID, "-"+s) {
			return s, true
		}
	}
	return "", false
}

func findEffortModelVariant(models []AgyModelOption, currentModel, targetEffort string) (string, bool) {
	if targetEffort == "" || len(models) == 0 {
		return "", false
	}
	inCatalog := false
	for _, m := range models {
		if m.ID == currentModel {
			inCatalog = true
			break
		}
	}
	if !inCatalog {
		return "", false
	}
	suffix, ok := extractCatalogEffortSuffix(currentModel)
	if !ok {
		return "", false
	}
	base := strings.TrimSuffix(currentModel, "-"+suffix)
	if base == "" {
		return "", false
	}
	candidate := base + "-" + targetEffort
	for _, m := range models {
		if m.ID == candidate {
			return candidate, true
		}
	}
	return "", false
}
